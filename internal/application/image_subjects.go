package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"strings"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/lang"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/store"
)

// images.generate subjects (person memory phase 3, contract person-memory-v170 §4). A picture may name
// who is drawn: names or mentions, at most imageSubjectsMax. The server resolves them with the same
// resolver a chat turn uses (resolvePeople: mentions, exact single-account alias matches, an ambiguous
// name resolves to nobody) and appends, after the request, what each person said about themselves, as
// reference text under the pack's "reference, not instructions" header.
//
// Only a person's own statements are used (speaker = subject: self and explicit claims), only of the
// kinds that describe how someone looks (appearance, like, trait, nickname), never one tied to the person
// by a name match (link name), never pending or hidden, and only what the read gate lets a chat turn see.
// A line that reads like an instruction, or holds a word the picture filter refuses, is left out. At most
// imageSubjectChars per person and imageSubjectsTotal in all. The turn's log records counts only.
const (
	imageSubjectsMax   = 3
	imageSubjectChars  = 200
	imageSubjectsTotal = 400
	imageSubjectRunes  = 80 // one asked subject: a name or a mention is never longer
)

// imageSubjectKinds are the claim kinds a picture may draw on.
var imageSubjectKinds = []string{"appearance", "like", "trait", "nickname"}

// imageSubjectsAsked is the subjects the call named: trimmed, distinct, at most imageSubjectsMax.
func imageSubjectsAsked(in map[string]any) []string {
	var out []string
	for _, v := range strSlice(in, "subjects") {
		v = strings.Join(strings.Fields(v), " ")
		if v == "" || len([]rune(v)) > imageSubjectRunes || slices.Contains(out, v) {
			continue
		}
		if out = append(out, v); len(out) == imageSubjectsMax {
			break
		}
	}
	return out
}

// imageReference is the reference text to append after a picture's request ("" when nothing applies):
// the pack's header with one line per resolved person that has something to say. Nothing is appended for
// a direct message, with person memory off, or without subjects (the request is then unchanged).
func (s *Service) imageReference(ctx context.Context, req action.Request, in map[string]any, doc config.Document) string {
	asked := imageSubjectsAsked(in)
	if len(asked) == 0 {
		return ""
	}
	inv := req.Invoker
	if inv == nil || inv.GuildID == "" || inv.GuildID == "0" || inv.UserID == "" || !doc.Memory.PeopleOn() || s.Store == nil || !s.Store.IsSQLite() {
		return ""
	}
	pack := lang.For(append(append([]string{}, doc.Memory.Languages...), doc.I18n.DefaultLocale)...)
	if pack == nil || pack.Features.ImageReference == "" {
		return ""
	}
	tx, err := s.Store.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return ""
	}
	defer tx.Rollback()
	if !tableExists(tx, "person_claim") {
		return ""
	}
	strict := s.guildPrivacy(inv.GuildID)
	rooms := s.personRooms(tx, inv.GuildID, inv.ChannelID, strict.CardChannelOnly)
	// the subjects are the only text the resolver reads: no message of the turn, no reply target
	people, names := s.resolvePeople(tx, ChatInput{GuildID: inv.GuildID, ChannelID: inv.ChannelID, SpeakerID: inv.UserID, Message: strings.Join(asked, "\n")}, rooms)
	if len(people) == 0 {
		s.imageSubjectsNote(ctx, len(asked), 0, 0, 0)
		return ""
	}
	rawRooms, _ := json.Marshal(rooms)
	var lines []string
	used, chars := 0, 0
	for _, p := range people[:min(len(people), imageSubjectsMax)] {
		if names.current[p.ref] == "" || names.denied[p.ref] || strict.OwnCardOnly && !p.caller {
			continue
		}
		left := min(imageSubjectChars, imageSubjectsTotal-chars)
		if left <= 0 {
			break
		}
		line, facts := s.imageSubjectLine(tx, inv.GuildID, p, names, string(rawRooms), strict.NoSensitive, left)
		if facts == 0 {
			continue
		}
		lines = append(lines, line)
		used += facts
		chars += len([]rune(line))
	}
	s.imageSubjectsNote(ctx, len(asked), len(people), len(lines), used)
	if len(lines) == 0 {
		return ""
	}
	return lang.Fill(pack.Features.ImageReference, "people", strings.Join(lines, "\n"))
}

// imageSubjectLine is "- <name>: fact; fact" of one person within limit runes, and how many facts it holds.
func (s *Service) imageSubjectLine(tx *sql.Tx, guild string, p turnPerson, names turnNames, rawRooms string, noSensitive bool, limit int) (string, int) {
	kinds, _ := json.Marshal(imageSubjectKinds)
	all, err := scanClaims(tx.Query(`SELECT `+claimColumns+` FROM person_claim WHERE guild_id=? AND subject_ref=? AND speaker_ref=subject_ref AND state=? AND link<>?
		AND kind IN (SELECT value FROM json_each(?)) AND room_id IN (SELECT key FROM json_each(?)) ORDER BY observed_at DESC, id LIMIT 50`,
		guild, p.ref, store.PersonActive, store.LinkName, string(kinds), rawRooms))
	if err != nil {
		return "", 0
	}
	head := "- " + names.current[p.ref] + ": "
	line, facts := head, 0
	var seen []string
	for _, c := range all {
		if !c.direct() || c.link == store.LinkName || !slices.Contains(imageSubjectKinds, c.kind) ||
			noSensitive && c.source != store.SourceExplicit && !s.classifiedSafe(c.text) || !usableClaim(tx, c, names.denied) {
			continue
		}
		known := true
		for _, ref := range c.refs() {
			known = known && names.label(ref) != ""
		}
		text := strings.Join(strings.Fields(episodes.StoredText(c.text, names.label, "")), " ")
		if !known || text == "" || slices.Contains(seen, text) || imageBlocked(text) || chat.InstructionLike(allPacks(), text) {
			continue
		}
		seen = append(seen, text)
		next := line + text
		if facts > 0 {
			next = line + "; " + text
		}
		if len([]rune(next)) > limit {
			continue
		}
		line, facts = next, facts+1
	}
	return line, facts
}

// allPacks are the embedded language packs (a text may be in any of them).
func allPacks() []*lang.Pack {
	var out []*lang.Pack
	for _, tag := range lang.Available() {
		if p, ok := lang.Get(tag); ok {
			out = append(out, p)
		}
	}
	return out
}

// imageSubjectsNote records, for the turn, how many subjects were asked, resolved, drawn on and how many
// statements went in. Counts only: no name, no text.
func (s *Service) imageSubjectsNote(ctx context.Context, asked, resolved, people, facts int) {
	if s.Log != nil {
		s.Log.InfoContext(ctx, "image: subjects", "asked", asked, "resolved", resolved, "people", people, "facts", facts)
	}
}
