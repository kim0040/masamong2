package application

import (
	"context"
	"database/sql"
	"strings"
	"sync"
	"time"

	"masamong/internal/lang"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// Person memory, live extraction (v1.7.0 phase 2). The episode extraction may answer with a "people"
// list; it is validated apart (episodes.Prompt.ParsePeople), turned into claims here and saved by the
// write gate in the same transaction as the episode ops. A bad or missing people list never fails,
// re-asks or regenerates the episode extraction: it only costs the claims.

// livePeoplePrefix is the source of a live claim: extract:<first observation of the segment>.
const livePeoplePrefix = "extract:"

// liveSinceMu makes "write once" hold when two rooms save their first claim together.
var liveSinceMu sync.Mutex

// liveClaims turns a segment's validated people facts into claims for the write gate. A fact is split
// into one claim per speaker, each with that speaker's own evidence lines only (the gate refuses the
// evidence of another author or of another room), the text each line had when the model read it, and a
// link that says self only when the speaker is the subject. A speaker with no line among the evidence
// is dropped and counted. A bot line is never evidence (ParsePeople already refused it; the author map
// holds human lines only).
func liveClaims(r memRoom, source, readAt string, facts []episodes.PersonFact, lines []roomLine) (claims []store.PersonClaim, dropped int) {
	author, text := map[string]string{}, map[string]string{}
	for _, l := range lines {
		if !l.Bot {
			author[l.ID], text[l.ID] = personRefOf(r, l.Message), l.Text
		}
	}
	for _, f := range facts {
		for _, speaker := range f.By {
			var ev []string
			read := map[string]string{}
			for _, id := range f.Ev {
				if author[id] == speaker {
					ev = append(ev, id)
					read[id] = text[id]
				}
			}
			if len(ev) == 0 {
				dropped++
				continue
			}
			link := f.Link
			switch {
			case speaker == f.About:
				link = store.LinkSelf
			case link == store.LinkSelf || link == "":
				link = store.LinkMention // a participant the speaker named by token
			}
			claims = append(claims, store.PersonClaim{GuildID: r.guild, SubjectRef: f.About, SpeakerRef: speaker, Kind: f.Kind, Topic: f.Topic,
				Text: f.Text, Evidence: ev, Source: source, Link: link, Lines: read, ReadAt: readAt})
		}
	}
	return claims, dropped
}

// liveSource is the claim source of a segment: extract:<its first observation id>.
func liveSource(lines []roomLine) string {
	if len(lines) == 0 {
		return livePeoplePrefix
	}
	return livePeoplePrefix + lines[0].ID
}

// personBuild fills the person memory inputs of an episode prompt: whether the room is a Kakao one (no
// people section), the bot's own reference, and the people the lines name without writing in them. A
// failed read of the people leaves the list empty (the prompt is then the one before person memory).
func (s *Service) personBuild(db *storage.Store, r memRoom, in episodes.Input, lines []roomLine) episodes.Input {
	if r.kakao() {
		in.Kakao = true
		return in
	}
	in.BotRef = s.liveBotRef(lines)
	mentioned, err := s.mentionedPeople(db, r.guild, in.Lines, in.BotRef)
	if err != nil {
		s.Log.Warn("memory: people named in a conversation could not be read", "err", err)
		return in
	}
	in.Mentioned = mentioned
	return in
}

// noteLiveSince records, once per server, the time live extraction first saved a person claim. It never
// overwrites a value, so the backfill's default range stays where it first ended.
func (s *Service) noteLiveSince(guild string) {
	liveSinceMu.Lock()
	defer liveSinceMu.Unlock()
	var at string
	ok, err := s.kvGet(pbLiveSinceKey+guild, &at)
	if err != nil || ok && at != "" {
		return
	}
	if err := s.kvSet(pbLiveSinceKey+guild, s.Clock.Now().UTC().Format(time.RFC3339)); err != nil {
		s.Log.Warn("memory: the first live people save was not recorded", "err", err)
	}
}

// savePeopleTx saves a segment's claims through the write gate inside the caller's transaction and
// returns how many rows were written. A refused claim is only skipped.
func (s *Service) savePeopleTx(ctx context.Context, tx *sql.Tx, claims []store.PersonClaim) (int, error) {
	if len(claims) == 0 {
		return 0, nil
	}
	return s.savePersonClaims(ctx, tx, claims)
}

// selfStatement reports whether a segment is one line the bot was addressed with: exactly one human line,
// that the gateway saw addressed to the bot (a mention or a reply to it) or that the bot answered with a
// reply, and with at least search.MinMessageRunes meaningful characters once mentions are taken away.
// Such a line is too short to be a conversation but can still say who its writer is ("나 광양 살아").
func (s *Service) selfStatement(r memRoom, lines []roomLine, bots []episodes.Message, botRef string) bool {
	var one roomLine
	n := 0
	for _, l := range lines {
		if !l.Bot {
			one = l
			n++
		}
	}
	if n != 1 || strings.TrimSpace(one.Text) == "" {
		return false
	}
	addressed := false
	for _, b := range bots {
		addressed = addressed || b.ReplyTo == one.ID
	}
	for _, l := range s.liveLines(r.guild, r.channel) {
		addressed = addressed || one.native != "" && l.ID == one.native && l.ToBot
	}
	if !addressed && botRef != "" {
		for _, m := range mentionPattern.FindAllStringSubmatch(one.Text, -1) {
			addressed = addressed || personRef(m[1]) == botRef
		}
	}
	return addressed && search.MeaningfulRunes(mentionPattern.ReplaceAllString(one.Text, " ")) >= search.MinMessageRunes
}

// extractSelfStatement asks the person_extract prompt (people only, no episode) about a one-line segment
// the bot was addressed with, on the summary role inside the derived budget, and saves the claims that
// validate. It returns the number of model calls made (0 or 1), the claims saved and an error that
// ends the pass (budget, provider); a bad answer is not one.
func (s *Service) extractSelfStatement(ctx context.Context, db *storage.Store, r memRoom, lines []roomLine, bots []episodes.Message, pack *lang.Pack, botName string) (calls, saved int, err error) {
	if r.kakao() {
		return 0, 0, nil
	}
	botRef := s.liveBotRef(lines)
	if !s.selfStatement(r, lines, bots, botRef) {
		return 0, 0, nil
	}
	msgs := segmentMessages(lines, bots)
	readAt := s.Clock.Now().UTC().Format(time.RFC3339Nano)
	in := s.personBuild(db, r, episodes.Input{Lines: msgs, BotName: botName, Now: lines[len(lines)-1].At, Location: s.instanceZone(),
		RefOf: func(m episodes.Message) string { return personRefOf(r, m) }, LabelOf: s.refLabels(db, r.room)}, lines)
	p, ok := episodes.BuildPersonExtract(in)
	if !ok {
		return 0, 0, nil
	}
	system := lang.Fill(pack.Memory.PersonExtractSystem, "bot", botName)
	prompt := p.FillPersonExtract(pack.Memory.PersonExtract)
	var facts []episodes.PersonFact
	asked := false
	err = s.askValidated(ctx, spendEpisode, system, prompt, extractTokens, func(text string, _ bool) error {
		asked = true
		facts, _ = p.ParsePeople(text, episodes.PeopleOptions{}) // a bad answer is no claims, never a second ask
		return nil
	})
	if asked {
		calls = 1
	}
	if err != nil {
		return calls, 0, err
	}
	claims, _ := liveClaims(r, liveSource(lines), readAt, facts, lines)
	if len(claims) == 0 {
		return calls, 0, nil
	}
	saved, err = s.savePeopleOnly(ctx, db, r, lines, claims)
	return calls, saved, err
}

// savePeopleOnly saves claims of a segment that made no episode: the same transaction rule as
// saveEpisodeOps (a line that changed meanwhile refuses everything), without any episode op.
func (s *Service) savePeopleOnly(ctx context.Context, db *storage.Store, r memRoom, lines []roomLine, claims []store.PersonClaim) (int, error) {
	_, people, err := s.saveEpisodeOps(ctx, db, r, lines, nil, "", claims)
	return people, err
}
