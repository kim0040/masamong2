package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

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

// liveAskedPrefix + room is the one-line statements of a room whose people request was paid for and whose
// result is saved, until the room's saved cursor covers them (liveAsked).
const liveAskedPrefix = "people:live_asked:"

// liveBoundaryKey is where live people extraction started in this database (liveBoundary).
const liveBoundaryKey = "people:live_boundary"

// liveBoundary is written once, the first time live people extraction runs in this database (a room
// memory pass with person memory on): the time, and the last observation captured before it (rowid, the
// capture order: a clock of one-second steps could not tell a line stored just before it from one just
// after). The one-line path asks only about lines captured after it: a stored line from before the upgrade
// (an unsealed singleton the bot was addressed with) is never bought on the first pass. Episode
// extraction of segments is unchanged.
type liveBoundary struct {
	At    string `json:"at"`
	RowID int64  `json:"rowid"`
}

// markLiveBoundary writes liveBoundary when there is none yet; a value once written is never moved.
func (s *Service) markLiveBoundary(db *storage.Store) error {
	if ok, err := s.kvGet(liveBoundaryKey, &liveBoundary{}); err == nil && ok {
		return nil
	}
	var mark int64
	if err := db.Reader().QueryRow(`SELECT COALESCE(MAX(rowid),0) FROM memory_observations`).Scan(&mark); err != nil {
		return err
	}
	at := s.Clock.Now().UTC().Format(time.RFC3339Nano)
	return kvUpdate(s, liveBoundaryKey, func(b *liveBoundary) *liveBoundary {
		if b != nil && b.At != "" {
			return b // written meanwhile: kept
		}
		return &liveBoundary{At: at, RowID: mark}
	})
}

// afterLiveBoundary reports whether a line was captured after live people extraction started. No
// boundary, or an unreadable one, is false (nothing is bought); so is a line no longer stored.
func (s *Service) afterLiveBoundary(db *storage.Store, id string) (bool, error) {
	var b liveBoundary
	ok, err := s.kvGet(liveBoundaryKey, &b)
	if err != nil {
		var syntax *json.SyntaxError
		var kind *json.UnmarshalTypeError
		if errors.As(err, &syntax) || errors.As(err, &kind) {
			return false, nil
		}
		return false, err
	}
	if !ok || b.At == "" {
		return false, nil
	}
	var rowid int64
	switch err := db.Reader().QueryRow(`SELECT rowid FROM memory_observations WHERE observation_id=?`, id).Scan(&rowid); {
	case errors.Is(err, sql.ErrNoRows):
		return false, nil
	case err != nil:
		return false, err
	}
	return rowid > b.RowID, nil
}

// liveClaims turns a segment's validated people facts into claims for the write gate and counts the facts
// it skips. A fact is one speaker's statement or nothing: its "by" is one person and every line it cites
// was written by that person (a bot line, or a line of another author, is never evidence). A fact the
// model cited from two people's lines is skipped, never split: the subject's half would rest on the
// subject's own line (often only a reaction), be read server-wide as their own statement with no source
// label, and outlive the line that said it (the backfill drops such a fact too, pbAttribute). A self
// claim (the speaker is the subject) must also name nobody but its subject, the derivation's rule
// (derivedClaims). Each claim keeps the text each line had when the model read it, and its link says
// self only when the speaker is the subject. names (nil = none) first ties the people the text names by
// their exact label to their token, or refuses the fact (liveNames).
func liveClaims(r memRoom, source, readAt string, facts []episodes.PersonFact, lines []roomLine, names nameTie) (claims []store.PersonClaim, skipped int) {
	author, text := map[string]string{}, map[string]string{}
	for _, l := range lines {
		if !l.Bot {
			author[l.ID], text[l.ID] = personRefOf(r, l.Message), l.Text
		}
	}
	for _, f := range facts {
		if names != nil {
			tied, ok := names(f.Text, f.About)
			if !ok {
				skipped++
				continue
			}
			f.Text = tied
		}
		c, ok := liveClaim(r, source, readAt, f, author, text)
		if !ok {
			skipped++
			continue
		}
		claims = append(claims, c)
	}
	return claims, skipped
}

// liveClaim is liveClaims for one fact; false means skip it.
func liveClaim(r memRoom, source, readAt string, f episodes.PersonFact, author, text map[string]string) (store.PersonClaim, bool) {
	if len(f.By) != 1 || len(f.Ev) == 0 {
		return store.PersonClaim{}, false
	}
	speaker := f.By[0]
	read := map[string]string{}
	for _, id := range f.Ev {
		if a, ok := author[id]; !ok || a != speaker {
			return store.PersonClaim{}, false
		}
		read[id] = text[id]
	}
	link := f.Link
	switch {
	case speaker == f.About:
		if named := uniqueStrings(episodes.TextRefs(f.Text)); len(named) != 1 || named[0] != f.About {
			return store.PersonClaim{}, false // a self claim names only its subject
		}
		link = store.LinkSelf
	case link == store.LinkSelf || link == "":
		link = store.LinkMention // a participant the speaker named by token
	}
	return store.PersonClaim{GuildID: r.guild, SubjectRef: f.About, SpeakerRef: speaker, Kind: f.Kind, Topic: f.Topic,
		Text: f.Text, Evidence: append([]string(nil), f.Ev...), Source: source, Link: link, Lines: read, ReadAt: readAt}, true
}

// nameTie rewrites a claim's stored text so that the people it names by their exact label are written as
// their {@ref} token; false means the text names someone who cannot be told apart, and the claim is
// dropped.
type nameTie func(text, subject string) (string, bool)

// liveRefToken is a stored person reference in a text ({@ref}).
var liveRefToken = regexp.MustCompile(`\{@[^{}]+\}`)

// liveNames ties the people a live claim's text names literally (the model copied a name from a line
// instead of writing its token) to the people of the prompt: its people list (the authors of the lines,
// the people their mentions name and the exact alias matches) and every person the caller listed as
// mentioned, a name two people share included. A name stands for a person when it is a whole word or the
// word plus a particle or honorific (search.NameMatcher, the rule a chat turn resolves names by), outside
// a {@ref} token. A name of exactly one person other than the subject is replaced by that person's token,
// so forgetting that person reaches the claim; a name that maps to two people (or to the subject and
// someone else) drops the claim. The subject's own name is left as written.
func (s *Service) liveNames(p episodes.Prompt, in episodes.Input) nameTie {
	var (
		built   bool
		byLabel map[string]map[string]bool
		labels  []string
		matches func(string, string) bool
	)
	build := func() {
		built, byLabel = true, map[string]map[string]bool{}
		add := func(label, ref string) {
			if label = strings.TrimSpace(label); label == "" || ref == "" {
				return
			}
			if byLabel[label] == nil {
				byLabel[label] = map[string]bool{}
				labels = append(labels, label)
			}
			byLabel[label][ref] = true
		}
		for _, per := range p.PersonRefs() {
			add(per.Label, per.Ref)
		}
		for _, per := range in.Mentioned {
			add(per.Label, per.Ref)
		}
		for _, m := range in.Lines {
			if !m.Bot && in.RefOf != nil {
				add(m.Author, in.RefOf(m))
			}
		}
		// the longest name first, so a name inside a longer one is not taken for it
		slices.SortFunc(labels, func(a, b string) int { return len(b) - len(a) })
		doc := s.Doc()
		matches = search.NameMatcher(lang.MergeSearch(append(slices.Clone(doc.Memory.Languages), "ko", "ja"), doc.BotNames()))
	}
	return func(text, subject string) (string, bool) {
		if !built {
			build()
		}
		for _, label := range labels {
			only := ""
			if who := byLabel[label]; len(who) == 1 {
				for ref := range who {
					only = ref
				}
			}
			if only == subject {
				continue // the subject's own name: the claim is tied to them already
			}
			for from := 0; ; {
				i := literalName(text, label, from, matches)
				if i < 0 {
					break
				}
				if only == "" {
					return "", false // a name two people share: whose it is cannot be told
				}
				tok := "{@" + only + "}"
				text = text[:i] + tok + text[i+len(label):]
				from = i + len(tok)
			}
		}
		if utf8.RuneCountInString(text) > episodes.MaxPersonText {
			return "", false // the write gate's limit, on the stored text
		}
		return text, true
	}
}

// literalName is the byte offset of the first place at or after from where text names label as a person
// (outside a {@ref} token, at the start of a word, followed only by what matches allows), or -1.
func literalName(text, label string, from int, matches func(string, string) bool) int {
	tokens := liveRefToken.FindAllStringIndex(text, -1)
	for from < len(text) {
		off := strings.Index(text[from:], label)
		if off < 0 {
			return -1
		}
		i := from + off
		from = i + len(label)
		if slices.ContainsFunc(tokens, func(t []int) bool { return i >= t[0] && i < t[1] }) {
			continue
		}
		if r, _ := utf8.DecodeLastRuneInString(text[:i]); i > 0 && (unicode.IsLetter(r) || unicode.IsDigit(r)) {
			continue
		}
		end := i + len(label)
		for end < len(text) {
			r, size := utf8.DecodeRuneInString(text[end:])
			if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
				break
			}
			end += size
		}
		if matches(text[i:end], label) {
			return i
		}
	}
	return -1
}

// liveSource is the claim source of a segment: extract:<its first observation id>.
func liveSource(lines []roomLine) string {
	if len(lines) == 0 {
		return livePeoplePrefix
	}
	return livePeoplePrefix + lines[0].ID
}

// personBuild fills the person memory inputs of an episode prompt: whether the room is a Kakao one (no
// people section), whether person memory is off (memory.people: off, or the old memory.cards: off: no
// people section, no people answer, and the people named are not looked up), the bot's own reference,
// and the people the lines name without writing in them. A failed read of the people leaves the list
// empty (the prompt is then the one before person memory).
func (s *Service) personBuild(db *storage.Store, r memRoom, in episodes.Input, lines []roomLine) episodes.Input {
	if r.kakao() {
		in.Kakao = true
		return in
	}
	in.BotRef = s.liveBotRef(lines)
	if !s.Doc().Memory.PeopleOn() {
		in.NoPeople = true
		return in
	}
	mentioned, err := s.mentionedPeople(db, r.guild, in.Lines, in.BotRef)
	if err != nil {
		s.Log.Warn("memory: people named in a conversation could not be read", "err", err)
		return in
	}
	in.Mentioned = mentioned
	return in
}

// kvWrite is an admin_state value written inside a transaction, for the cache once it commits.
type kvWrite struct{ key, raw string }

// kvCacheWrites tells the admin_state cache the values a committed transaction wrote.
func (s *Service) kvCacheWrites(ws []kvWrite) {
	for _, w := range ws {
		s.kv.put(w.key, kvEntry{raw: w.raw, ok: true})
	}
}

// livePeopleMarksTx writes, in the transaction that saves a segment's claims, what must commit with them
// or not at all. The first live claims of a server record live_since, the time live extraction first saved
// a person claim: a value once written is never overwritten (the backfill's default range stays where it
// first ended), and the writer transaction makes "once" hold for rooms saving together. A one-line
// statement (asked) is marked asked (liveAsked). It returns the values written, for the cache.
func (s *Service) livePeopleMarksTx(tx *sql.Tx, backend string, r memRoom, lines []roomLine, saved int, asked string) ([]kvWrite, error) {
	var out []kvWrite
	now := s.Clock.Now().UTC()
	if saved > 0 {
		var raw string
		switch err := tx.QueryRow(`SELECT v FROM admin_state WHERE k=?`, pbLiveSinceKey+r.guild).Scan(&raw); {
		case errors.Is(err, sql.ErrNoRows), err == nil && (raw == "" || raw == `""`):
			w, err := kvWriteTx(tx, backend, pbLiveSinceKey+r.guild, now.Format(time.RFC3339), now)
			if err != nil {
				return nil, err
			}
			out = append(out, w)
		case err != nil:
			return nil, err
		}
	}
	if asked != "" {
		i := slices.IndexFunc(lines, func(l roomLine) bool { return l.ID == asked })
		if i < 0 {
			return out, nil
		}
		var marks []liveAsk
		var raw string
		switch err := tx.QueryRow(`SELECT v FROM admin_state WHERE k=?`, liveAskedPrefix+r.room).Scan(&raw); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return nil, err
		default:
			_ = json.Unmarshal([]byte(raw), &marks) // unreadable: the marks start over
		}
		if !slices.ContainsFunc(marks, func(m liveAsk) bool { return m.ID == asked }) {
			w, err := kvWriteTx(tx, backend, liveAskedPrefix+r.room, append(marks, liveAsk{ID: asked, At: lines[i].parsed}), now)
			if err != nil {
				return nil, err
			}
			out = append(out, w)
		}
	}
	return out, nil
}

// kvWriteTx writes one admin_state value inside tx.
func kvWriteTx(tx *sql.Tx, backend, key string, v any, now time.Time) (kvWrite, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return kvWrite{}, err
	}
	if _, err := tx.Exec(kvUpsertSQL(backend), key, string(raw), now.Format(time.RFC3339)); err != nil {
		return kvWrite{}, err
	}
	return kvWrite{key, string(raw)}, nil
}

// liveAsk is a one-line statement whose people request was paid for and saved: its observation and the
// time it was written (roomLine.parsed, the cursor's unit).
type liveAsk struct {
	ID string `json:"id"`
	At string `json:"at"`
}

// liveAsked reports whether the people request of a one-line statement was already paid for and its
// result saved. The room cursor that records its segment is written after that, apart: when that write
// fails and the process restarts, the line comes back unsealed and is not asked (and paid for) again.
func (s *Service) liveAsked(room, id string) (bool, error) {
	var marks []liveAsk
	if _, err := s.kvGet(liveAskedPrefix+room, &marks); err != nil {
		var syntax *json.SyntaxError
		var kind *json.UnmarshalTypeError
		if errors.As(err, &syntax) || errors.As(err, &kind) {
			return false, nil // unreadable marks are none
		}
		return false, err
	}
	return slices.ContainsFunc(marks, func(m liveAsk) bool { return m.ID == id }), nil
}

// markLiveAsked marks a one-line statement asked when its answer saved nothing (no transaction did it).
func (s *Service) markLiveAsked(r memRoom, l roomLine) error {
	return kvUpdate(s, liveAskedPrefix+r.room, func(marks []liveAsk) []liveAsk {
		if slices.ContainsFunc(marks, func(m liveAsk) bool { return m.ID == l.ID }) {
			return marks
		}
		return append(marks, liveAsk{ID: l.ID, At: l.parsed})
	})
}

// pruneLiveAsked forgets, once a room's cursor is saved, the asked marks it covers: a line whose segment
// it records as sealed, or one before where it reads from.
func (s *Service) pruneLiveAsked(room string, cur episodeCursor) {
	var marks []liveAsk
	if ok, err := s.kvGet(liveAskedPrefix+room, &marks); err != nil || !ok || len(marks) == 0 {
		return
	}
	sealed := map[string]bool{}
	for _, x := range cur.Sealed {
		for _, id := range x.IDs {
			sealed[id] = true
		}
	}
	covered := func(m liveAsk) bool { return sealed[m.ID] || cur.BlockStart != "" && m.At < cur.BlockStart }
	if !slices.ContainsFunc(marks, covered) {
		return
	}
	if err := kvUpdate(s, liveAskedPrefix+room, func(marks []liveAsk) []liveAsk { return slices.DeleteFunc(marks, covered) }); err != nil {
		s.Log.Warn("memory: one-line asked marks not pruned", "err", err)
	}
}

// savePeopleTx saves a segment's claims through the write gate inside the caller's transaction and
// returns how many rows were written. A refused claim is only skipped. Nothing is written once person
// memory was switched off while the model answered.
func (s *Service) savePeopleTx(ctx context.Context, tx *sql.Tx, claims []store.PersonClaim) (int, error) {
	if len(claims) == 0 || !s.Doc().Memory.PeopleOn() {
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
// validate. It returns what it did (model calls made, 0 or 1; claims saved; facts skipped) and an error
// that ends the pass (budget, provider); a bad answer is not one.
func (s *Service) extractSelfStatement(ctx context.Context, db *storage.Store, r memRoom, lines []roomLine, bots []episodes.Message, pack *lang.Pack, botName string) (ps peopleStats, err error) {
	if r.kakao() || !s.Doc().Memory.PeopleOn() {
		return ps, nil // person memory off: the line costs nothing, as before person memory
	}
	botRef := s.liveBotRef(lines)
	if !s.selfStatement(r, lines, bots, botRef) {
		return ps, nil
	}
	// a line paid for and saved before is not asked again, even when the room cursor could not record it
	one := lines[slices.IndexFunc(lines, func(l roomLine) bool { return !l.Bot })]
	if asked, err := s.liveAsked(r.room, one.ID); err != nil || asked {
		return ps, err
	}
	// a line stored before live people extraction started (before the upgrade) is never bought
	if after, err := s.afterLiveBoundary(db, one.ID); err != nil || !after {
		return ps, err
	}
	// what is no longer current is never sent (the gate's check after the answer could not undo that):
	// when the line is dropped there is nothing to ask
	lines, err = s.sendableLines(db, r, lines)
	if err != nil || !slices.ContainsFunc(lines, func(l roomLine) bool { return !l.Bot }) {
		return ps, err
	}
	msgs := segmentMessages(lines, bots)
	readAt := s.Clock.Now().UTC().Format(time.RFC3339Nano)
	in := s.personBuild(db, r, episodes.Input{Lines: msgs, BotName: botName, Now: lines[len(lines)-1].At, Location: s.instanceZone(),
		RefOf: func(m episodes.Message) string { return personRefOf(r, m) }, LabelOf: s.refLabels(db, r.room)}, lines)
	p, ok := episodes.BuildPersonExtract(in)
	if !ok {
		return ps, nil
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
		ps.calls = 1
	}
	if err != nil {
		return ps, err
	}
	claims, skipped := liveClaims(r, liveSource(lines), readAt, facts, lines, s.liveNames(p, in))
	ps.skipped = skipped
	if len(claims) == 0 {
		// paid for, nothing to save: marked on its own, so a restart before the cursor is saved does not
		// ask again
		if err := s.markLiveAsked(r, one); err != nil {
			s.Log.Warn("memory: a one-line people request was not marked asked", "err", err)
		}
		return ps, nil
	}
	var lost bool
	ps.claims, lost, err = s.savePeopleOnly(ctx, db, r, lines, claims, one.ID)
	if lost {
		ps.failed++ // the line is still sealed and marked asked when it can be: never bought again
	}
	return ps, err
}

// sendableLines are the lines of a segment that may still be sent to a model: a human line is dropped when
// its observation is gone, deleted or changed, a deletion or edit intent was committed for it
// (store.ObservationCurrent, which also refuses a blocked account), its author withdrew memory or profile
// consent or is blocked here (deniedPeople) or has a tombstone of their own (store.SourceBlocked); every
// line goes when the room is a deleted thread (threadDeleted). Bot lines are kept: segmentMessages keeps
// only the replies to the lines that remain.
func (s *Service) sendableLines(db *storage.Store, r memRoom, lines []roomLine) ([]roomLine, error) {
	if s.threadDeleted(r.channel) {
		return nil, nil
	}
	tx, err := db.Reader().Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	denied, err := s.deniedPeople(tx, r.guild)
	if err != nil {
		return nil, err
	}
	out := make([]roomLine, 0, len(lines))
	for _, l := range lines {
		if l.Bot {
			out = append(out, l)
			continue
		}
		var content, deleted string
		err := tx.QueryRow(`SELECT content, deleted_at FROM memory_observations WHERE observation_id=?`, l.ID).Scan(&content, &deleted)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if deleted != "" || strings.TrimSpace(content) != l.Text {
			continue
		}
		if current, err := store.ObservationCurrent(tx, l.ID); err != nil || !current {
			if err != nil {
				return nil, err
			}
			continue
		}
		ref := personRefOf(r, l.Message)
		if denied[ref] {
			continue
		}
		if blocked, err := store.SourceBlocked(tx, ref); err != nil || blocked {
			if err != nil {
				return nil, err
			}
			continue
		}
		out = append(out, l)
	}
	return out, nil
}

// savePeopleOnly saves claims of a segment that made no episode: the same transaction rule as
// saveEpisodeOps (a line that changed meanwhile refuses everything), without any episode op, and marks
// the one-line statement asked in that transaction. lost reports a people part that could not be written
// (saveEpisodeOps): the segment is then done all the same.
func (s *Service) savePeopleOnly(ctx context.Context, db *storage.Store, r memRoom, lines []roomLine, claims []store.PersonClaim, asked string) (int, bool, error) {
	_, people, lost, err := s.saveEpisodeOps(ctx, db, r, lines, nil, "", claims, asked)
	return people, lost, err
}
