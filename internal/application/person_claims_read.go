package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"slices"
	"sort"
	"strings"
	"time"

	"masamong/internal/chat"
	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/lexical"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// Person memory, read side (contract person-memory-v170 §2.3). One gate decides which stored claims a
// reader may see; chat turns, /내기억 and the console all go through usableClaim. Trust, supersession
// and grouping are computed here, never stored.
//
//   - Scope (owner decision 6): personRooms gives the rooms this channel may draw on, true for its own
//     channel group. A person's own statements (self, explicit) are used in any of them; what others
//     said (said, told) only inside its group. A private thread known to the gateway is its own group;
//     one it does not know (a cache miss) has no rooms at all.
//   - Every evidence line is read again: live, current, a human Discord line of the claim's room written
//     by its speaker, not edited after the claim was written; an episode_item source must still be
//     active. Nobody the claim names may have withdrawn memory or profile, be blocked or tombstoned, and
//     no forget or hide marker may cover it.
//   - The caller's block comes on every server turn with what the caller said about themselves; what
//     others said about the caller only when the resolver made the caller a target of the turn (a request
//     about themselves, a self mention, a reply to their own line); a planner's name alone never does. A
//     chime-in or banter reply may use everything (owner decision 9). A DM reads no person memory.

// maxPersonFacts is how many lines of one person are ranked into the block (the budget cuts further).
const maxPersonFacts = 12

// Claims of one person are read newest first in pages of claimPage eligible rows (scope, tier and state
// are in the query); a page whose rows fail the gate is followed by the next, up to claimScan rows.
const (
	claimPage = 200
	claimScan = 1000
)

// singleTopics are the status topics that hold one value at a time: the newest statement of one of them
// replaces the older ones (§2.3.4). Every other topic accumulates. The keys are store.PersonTopic's (its
// synonyms, 직업 or 거주지, are written as these).
var singleTopics = map[string]bool{
	"residence": true, "job": true, "school": true, "birthday": true, "relationship_status": true,
}

// singleTopic reports whether a claim's topic holds one value at a time.
func singleTopic(kind, topic string) bool {
	return kind == "status" && singleTopics[store.PersonTopic(topic)]
}

// claimRow is one stored claim as the read gate sees it.
type claimRow struct {
	id, subject, speaker, kind, topic, text, room, source, link, observed, created, state string
	evidence                                                                              []string
}

func (c claimRow) direct() bool { return c.subject == c.speaker }

// origin is how the claim came about: self or explicit (the person themselves), said or told (someone
// else; told = asked the bot to remember it).
func (c claimRow) origin() string {
	switch {
	case c.direct() && c.source == store.SourceExplicit:
		return "explicit"
	case c.direct():
		return "self"
	case c.source == store.SourceExplicit:
		return "told"
	}
	return "said"
}

func (c claimRow) refs() []string {
	return uniqueStrings(append([]string{c.subject, c.speaker}, episodes.TextRefs(c.text)...))
}

const claimColumns = `id,subject_ref,speaker_ref,kind,topic,text,evidence,observed_at,room_id,source,link,created_at,state`

func scanClaims(rows *sql.Rows, err error) ([]claimRow, error) {
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []claimRow
	for rows.Next() {
		var c claimRow
		var ev string
		if err := rows.Scan(&c.id, &c.subject, &c.speaker, &c.kind, &c.topic, &c.text, &ev, &c.observed, &c.room, &c.source, &c.link, &c.created, &c.state); err != nil {
			return nil, err
		}
		if json.Unmarshal([]byte(ev), &c.evidence) != nil || len(c.evidence) == 0 {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// usableClaim is the gate every reader applies to a stored claim: the people it names, the markers, the
// source item and every evidence line, on the reader's snapshot. denied are the people nobody may read
// about (deniedPeople).
func usableClaim(tx *sql.Tx, c claimRow, denied map[string]bool) bool {
	for _, ref := range c.refs() {
		if denied[ref] || !store.DiscordPersonRef(ref) {
			return false
		}
		if blocked, err := store.SourceBlocked(tx, ref); err != nil || blocked {
			return false
		}
	}
	marker := store.PersonClaim{ID: c.id, GuildID: guildOfRoom(c.room), SubjectRef: c.subject, SpeakerRef: c.speaker, Text: c.text, ObservedAt: c.observed}
	if suppressed, err := personSuppressed(tx, marker); err != nil || suppressed {
		return false
	}
	if item, ok := strings.CutPrefix(c.source, "episode_item:"); ok {
		var active bool
		if tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM episode_item WHERE id=? AND state='active')`, item).Scan(&active) != nil || !active {
			return false
		}
	}
	created, err := time.Parse(time.RFC3339Nano, c.created)
	if err != nil {
		return false
	}
	for _, id := range c.evidence {
		var room, account, edited, kind string
		if tx.QueryRow(`SELECT room,actor_account,edited_at,record_kind FROM memory_observations WHERE observation_id=? AND platform='discord' AND source_room=room
			AND deleted_at='' AND trim(content)<>''`, id).Scan(&room, &account, &edited, &kind) != nil {
			return false
		}
		if room != c.room || kind != corpus.HumanText || account == "" || personRef(account) != c.speaker {
			return false
		}
		if edited != "" {
			t, err := time.Parse(time.RFC3339Nano, edited)
			if err != nil || t.After(created) {
				return false
			}
		}
		if current, err := store.ObservationCurrent(tx, id); err != nil || !current {
			return false
		}
	}
	return true
}

// guildOfRoom is the server of a discord:<guild>:<channel> room ("" otherwise).
func guildOfRoom(room string) string {
	rest, ok := strings.CutPrefix(room, "discord:")
	if !ok {
		return ""
	}
	g, _, _ := strings.Cut(rest, ":")
	return g
}

// topicMarkers are the hide markers on hearsay topics about a person: kind:topic → set at.
func topicMarkers(tx *sql.Tx, guild, ref string) map[string]string {
	out := map[string]string{}
	rows, err := tx.Query(`SELECT scope,before_ts FROM person_memory_suppress WHERE guild_id=? AND person_ref=? AND role=? AND scope LIKE 'topic:%'`,
		guild, ref, store.SuppressSubject)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var scope, ts string
		if rows.Scan(&scope, &ts) == nil {
			out[strings.TrimPrefix(scope, "topic:")] = ts
		}
	}
	return out
}

// hiddenTopic reports whether a hearsay claim's topic was hidden and the subject has not said it
// themselves since (unlocked).
func hiddenTopic(c claimRow, markers map[string]string, unlocked func(kind, topic, at string) bool) bool {
	if c.direct() || c.topic == "" {
		return false
	}
	at, ok := markers[store.PersonTopicKey(c.kind, c.topic)]
	return ok && !unlocked(c.kind, c.topic, at)
}

// topicUnlocker answers hiddenTopic's question for one person and reader: whether the person stated a
// hidden topic themselves after it was hidden, in a claim of the reader's rooms that may be read itself
// (usable). Topics are stored by their one key (store.PersonTopic), so a synonym unlocks nothing new.
func topicUnlocker(tx *sql.Tx, guild, ref string, rooms map[string]bool, usable func(claimRow) bool) func(kind, topic, at string) bool {
	done := map[string]bool{}
	raw, _ := json.Marshal(rooms)
	return func(kind, topic, at string) bool {
		key := store.PersonTopicKey(kind, topic) + "\x00" + at
		if v, ok := done[key]; ok {
			return v
		}
		direct, _ := scanClaims(tx.Query(`SELECT `+claimColumns+` FROM person_claim WHERE guild_id=? AND subject_ref=? AND speaker_ref=subject_ref AND state=?
			AND kind=? AND topic=? AND observed_at>? AND room_id IN (SELECT key FROM json_each(?)) ORDER BY observed_at DESC, id LIMIT 20`,
			guild, ref, store.PersonActive, kind, store.PersonTopic(topic), at, string(raw)))
		v := false
		for _, d := range direct {
			if usable(d) {
				v = true
				break
			}
		}
		done[key] = v
		return v
	}
}

// claimFilter is what one reader may see of a person's claims: the rooms in scope (true = the
// reader's channel group), whether hearsay may be shown at all, and the states read.
type claimFilter struct {
	rooms   map[string]bool
	hearsay bool
	states  []string
}

// eligibleClaims reads a person's claims newest first in pages, with the scope, tier and state in the
// query, and keeps the ones keep accepts, until claimPage are kept or claimScan were read.
func eligibleClaims(tx *sql.Tx, guild, ref string, f claimFilter, keep func(claimRow) bool) []claimRow {
	var kept []claimRow
	read := 0
	rooms, local := map[string]bool{}, map[string]bool{}
	for r, l := range f.rooms {
		rooms[r] = true
		if l {
			local[r] = true
		}
	}
	rawRooms, _ := json.Marshal(rooms)
	rawLocal, _ := json.Marshal(local)
	rawStates, _ := json.Marshal(f.states)
	after, afterID := "~", ""
	for read < claimScan {
		page, err := scanClaims(tx.Query(`SELECT `+claimColumns+` FROM person_claim WHERE guild_id=?1 AND subject_ref=?2 AND state IN (SELECT value FROM json_each(?3))
			AND room_id IN (SELECT key FROM json_each(?4))
			AND (subject_ref=speaker_ref OR (?5 AND room_id IN (SELECT key FROM json_each(?6))))
			AND (observed_at<?7 OR (observed_at=?7 AND id>?8))
			ORDER BY observed_at DESC, id LIMIT ?9`, guild, ref, string(rawStates), string(rawRooms), f.hearsay, string(rawLocal), after, afterID, claimPage))
		if err != nil || len(page) == 0 {
			break
		}
		read += len(page)
		last := page[len(page)-1]
		after, afterID = last.observed, last.id
		for _, c := range page {
			if keep(c) {
				kept = append(kept, c)
			}
		}
		if len(kept) >= claimPage || len(page) < claimPage {
			break
		}
	}
	return kept
}

// saidByLabel is the source label of a hearsay line: the speaker's name, at most 24 runes.
func saidByLabel(pack *lang.Pack, name string) string {
	name = clip(name, 24)
	if pack != nil && pack.Memory.SaidBy != "" {
		return lang.Fill(pack.Memory.SaidBy, "name", name)
	}
	return "(" + name + ")"
}

// shownClaim is one claim on its way into a card.
type shownClaim struct {
	claimRow
	line   string // the rendered statement, with its source label for hearsay
	body   string // the statement without the label (ranking, grouping)
	score  int
	tokens map[string]bool
	refs   []string
}

// turnCards is the person block of a chat turn: the caller's card and the cards of the people the turn
// is about (at most chat.MaxOtherCards), each ranked by the words it shares with the conversation, then
// what people said about themselves, then newest. targets are the planner's names (the late hook).
func (s *Service) turnCards(ctx context.Context, db *storage.Store, in ChatInput, targets []string, q episodes.Query, pack *lang.Pack, used *[]string) ([]chat.RoomCard, personEpisodeScope) {
	if in.GuildID == "" || in.GuildID == "0" || !s.Doc().Memory.PeopleOn() {
		return nil, personEpisodeScope{} // a DM reads no person memory
	}
	tx, err := db.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, personEpisodeScope{}
	}
	defer tx.Rollback()
	strict := s.guildPrivacy(in.GuildID)
	rooms := s.personRooms(tx, in.GuildID, in.ChannelID, strict.CardChannelOnly)
	people, names := s.resolvePeople(tx, in, rooms)
	scope := personEpisodeScope{guild: in.GuildID, rooms: rooms}
	if names.current == nil || names.tx == nil || !tableExists(tx, "person_claim") {
		return nil, scope // no rooms, or the caller may not use memory
	}
	caller := personRef(in.SpeakerID)
	targeted := false
	for _, p := range people {
		targeted = targeted || p.caller
	}
	if label := names.label(caller); label == "" && strings.TrimSpace(in.Speaker) != "" {
		names.current[caller] = strings.TrimSpace(in.Speaker)
	}
	// a planner's names (targets) never make the caller a target on their own: the resolver decides
	_ = targets
	subjects := []turnPerson{}
	if !names.denied[caller] && names.current[caller] != "" {
		subjects = append(subjects, turnPerson{ref: caller, label: names.current[caller], caller: true, link: store.LinkSelf})
	}
	for _, p := range people {
		if !p.caller {
			subjects = append(subjects, p)
		}
	}
	if len(subjects) == 0 {
		return nil, scope
	}
	distinguishNames(subjects, names.current)
	everything := in.Proactive || in.banter
	doc := s.Doc()
	tok := lexical.FromLanguage(lang.MergeSearch(append(slices.Clone(doc.Memory.Languages), pack.Tag), doc.BotNames()))
	var out []chat.RoomCard
	for _, p := range subjects {
		if names.current[p.ref] == "" || names.denied[p.ref] || strict.OwnCardOnly && !p.caller {
			continue
		}
		markers := topicMarkers(tx, in.GuildID, p.ref)
		usable := map[string]bool{}
		isUsable := func(c claimRow) bool {
			v, ok := usable[c.id]
			if !ok {
				v = usableClaim(tx, c, names.denied)
				usable[c.id] = v
			}
			return v
		}
		filter := claimFilter{rooms: rooms, states: []string{store.PersonActive},
			hearsay: !strict.NoHearsay && (!p.caller || targeted || everything)}
		unlocked := topicUnlocker(tx, in.GuildID, p.ref, rooms, isUsable)
		eligible := eligibleClaims(tx, in.GuildID, p.ref, filter, func(c claimRow) bool {
			return !hiddenTopic(c, markers, unlocked) &&
				!(strict.NoSensitive && c.source != store.SourceExplicit && !s.classifiedSafe(c.text)) && isUsable(c)
		})
		var kept []shownClaim
		for _, c := range eligible {
			hearsay := !c.direct()
			sc := shownClaim{claimRow: c, refs: c.refs()}
			sc.body = episodes.StoredText(c.text, names.label, "?")
			sc.line = sc.body
			if hearsay {
				speaker := names.label(c.speaker)
				if speaker == "" {
					continue // a statement nobody can be named for is not shown
				}
				sc.line += " " + saidByLabel(pack, speaker)
			}
			for _, w := range q.Matches(sc.body) {
				if q.FromMessage(w) {
					sc.score += 2
				} else {
					sc.score++
				}
			}
			sc.tokens = map[string]bool{}
			for _, w := range tok.QueryWords(strings.ToLower(storedRefPattern.ReplaceAllString(c.text, " "))) {
				for _, g := range strings.Fields(tok.ToNgrams(w)) {
					sc.tokens[g] = true
				}
			}
			kept = append(kept, sc)
		}
		facts, refs := arrangeClaims(kept)
		if len(facts) == 0 {
			continue
		}
		out = append(out, chat.RoomCard{Name: names.current[p.ref], Facts: facts, Caller: p.caller})
		if used != nil {
			for _, ref := range refs {
				*used = append(*used, ref)
				if id := names.sources[ref]; id != "" {
					*used = append(*used, id)
				}
			}
		}
		if !p.caller || targeted {
			scope.people = append(scope.people, p)
		}
	}
	return out, scope
}

// arrangeClaims supersedes single-valued topics, ranks the rest, groups near-duplicates (the group shows
// its newest statement in the place of its best-ranked one) and returns at most maxPersonFacts lines
// with the references and evidence they rest on.
func arrangeClaims(claims []shownClaim) ([]string, []string) {
	sort.SliceStable(claims, func(i, j int) bool { return claims[i].observed > claims[j].observed })
	newest := map[string]bool{}
	live := claims[:0:0]
	for _, c := range claims {
		if singleTopic(c.kind, c.topic) {
			key := store.PersonTopicKey(c.kind, c.topic)
			if newest[key] {
				continue
			}
			newest[key] = true
		}
		live = append(live, c)
	}
	sort.SliceStable(live, func(i, j int) bool {
		a, b := live[i], live[j]
		if a.score != b.score {
			return a.score > b.score
		}
		if a.direct() != b.direct() {
			return a.direct()
		}
		if a.observed != b.observed {
			return a.observed > b.observed
		}
		return a.id < b.id
	})
	var groups []shownClaim
	var refs []string
	for _, c := range live {
		merged := false
		for i := range groups {
			if overlap(groups[i].tokens, c.tokens) >= cardSimilar {
				if c.observed > groups[i].observed {
					groups[i].line, groups[i].observed = c.line, c.observed
				}
				merged = true
				break
			}
		}
		if !merged {
			if len(groups) == maxPersonFacts {
				continue
			}
			groups = append(groups, c)
		}
		refs = append(append(refs, c.refs...), c.evidence...)
	}
	var lines []string
	for _, g := range groups {
		if !slices.Contains(lines, g.line) {
			lines = append(lines, g.line)
		}
	}
	return lines, uniqueStrings(refs)
}

// classifiedSafe reports whether the no_sensitive classifier has passed a text (its cached verdict).
func (s *Service) classifiedSafe(text string) bool {
	var sensitive bool
	ok, err := s.kvGet(sensitiveKey(text), &sensitive)
	return err == nil && ok && !sensitive
}

// claimsAbout reads the claims about a person in one server that the gate lets through, newest first,
// for the person's own view (/내기억, the console).
//
//   - A member in a channel (channel set) sees what a chat turn in that channel could show about them when
//     they ask about themselves: the channel's scope (decision 6: hearsay only from its own channel group,
//     a private thread's statements only inside it), the server's strict options and hidden topics, and
//     only active claims the classifier passed. A member with no channel sees nothing of it.
//   - The console (an operator, operator true) sees every active and pending claim of the server; a
//     pending one is marked by its state.
func (s *Service) claimsAbout(ctx context.Context, db *storage.Store, guild, ref, channel string, operator bool) ([]claimRow, error) {
	if guild == "" || ref == "" || !db.IsSQLite() || !tableReady(db, "person_claim") {
		return nil, nil
	}
	tx, err := db.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	strict := s.guildPrivacy(guild)
	filter := claimFilter{rooms: map[string]bool{}, hearsay: true, states: []string{store.PersonActive, store.PersonPending}}
	if !operator {
		// the first read of the snapshot: the thread deletion boundary relies on it (personRooms)
		filter = claimFilter{rooms: s.personRooms(tx, guild, channel, strict.CardChannelOnly), hearsay: !strict.NoHearsay, states: []string{store.PersonActive}}
		if len(filter.rooms) == 0 {
			return nil, nil
		}
	} else {
		rows, err := tx.Query(`SELECT DISTINCT room_id FROM person_claim WHERE guild_id=? AND subject_ref=?`, guild, ref)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var room string
			if rows.Scan(&room) == nil {
				filter.rooms[room] = true
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	denied, err := s.deniedPeople(tx, guild)
	if err != nil {
		return nil, err
	}
	delete(denied, ref) // the person reads their own memory even with profile withdrawn (then it is empty anyway)
	usable := map[string]bool{}
	isUsable := func(c claimRow) bool {
		v, ok := usable[c.id]
		if !ok {
			v = usableClaim(tx, c, denied)
			usable[c.id] = v
		}
		return v
	}
	markers := map[string]string{}
	if !operator {
		markers = topicMarkers(tx, guild, ref)
	}
	unlocked := topicUnlocker(tx, guild, ref, filter.rooms, isUsable)
	out := eligibleClaims(tx, guild, ref, filter, func(c claimRow) bool {
		if !operator && (hiddenTopic(c, markers, unlocked) || strict.NoSensitive && c.source != store.SourceExplicit && !s.classifiedSafe(c.text)) {
			return false
		}
		return isUsable(c)
	})
	return out, nil
}
