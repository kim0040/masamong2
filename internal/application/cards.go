package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"sort"
	"strings"
	"time"

	"masamong/internal/chat"
	"masamong/internal/conversation"
	"masamong/internal/features/privacy"
	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/lexical"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// Person cards (L3, docs/plan-memory-conversation.md §5.4.4, P4-5), a minimal version behind
// memory.cards (default off).
//
//   - person_card_item: what the bot keeps about one person of one server (guild × PersonRef), at most
//     cardBudget runes in all. Every item has its evidence (observation IDs, registered in evidence_refs
//     like every derived copy) and a supersession key: a newer item with the same key replaces the old
//     one, which stays as superseded. Two servers never share a card: the guild is part of the key.
//   - Promotion is model-free and strict, which is how "a friend does not remember everything" is kept:
//     a fact the person said about themselves becomes a card item only when it came up on two different
//     days, in two different episodes of importance 3 or more, and none of its evidence is the bot's —
//     unless the person asked to have it remembered (memory_explicit), which goes in right away.
//   - person_relation: counts of reply and mention exchanges per pair of people of a server, counted
//     without a model from the live lines; no message text, no IDs other than the one-way references.
//   - KakaoTalk speakers never get a card: they are kakao:<room>:<label> references, not people of a
//     server, and are never merged with a Discord account.
//
// Invalidation follows the same evidence_refs path as the episodes (invalidateCards): any deletion, edit
// or withdrawal that reaches a card item's evidence deletes the item (the next promotion makes it again
// from what remains); a withdrawal also deletes the person's cards and relations everywhere.

const (
	holderCardItem  = "person_card_item"
	cardBudget      = 400 // runes of one person's active card items
	cardMinDays     = 2
	cardMinEpisodes = 2
	cardMinImport   = 3
	cardSimilar     = 0.5 // token overlap that makes two facts the same fact
)

func init() {
	featureDDL = append(featureDDL,
		`CREATE TABLE IF NOT EXISTS person_card_item (
			id TEXT PRIMARY KEY,
			guild_id TEXT NOT NULL,
			person_ref TEXT NOT NULL,
			key TEXT NOT NULL,
			text TEXT NOT NULL,
			evidence TEXT NOT NULL,
			episodes TEXT NOT NULL DEFAULT '[]',
			origin TEXT NOT NULL,
			state TEXT NOT NULL DEFAULT 'active',
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_person_card_person ON person_card_item(guild_id, person_ref, state)`,
		`CREATE TABLE IF NOT EXISTS person_relation (
			guild_id TEXT NOT NULL,
			a_ref TEXT NOT NULL,
			b_ref TEXT NOT NULL,
			replies INTEGER NOT NULL DEFAULT 0,
			mentions INTEGER NOT NULL DEFAULT 0,
			updated_at TEXT NOT NULL,
			PRIMARY KEY(guild_id, a_ref, b_ref)
		) WITHOUT ROWID`,
		// explicit "remember this" facts a person asked for about themselves, with the key the classifier
		// gave the topic (a newer fact with the same key replaces the older one)
		`CREATE TABLE IF NOT EXISTS memory_explicit (
			episode_id TEXT PRIMARY KEY,
			guild_id TEXT NOT NULL,
			person_ref TEXT NOT NULL,
			key TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_explicit_person ON memory_explicit(guild_id, person_ref, key)`)
}

// cardFact is one candidate fact about a person.
type cardFact struct {
	item, episode, text string
	evidence            []string
	importance          int
	explicitKey         string
	tokens              map[string]bool
}

// promoteCards brings the cards of every person up to date with the episodes (no model call).
func (s *Service) promoteCards(ctx context.Context, db *storage.Store) (int, error) {
	if !s.Doc().Memory.CardsOn() || !db.IsSQLite() || !roomMemoryTables(db) {
		return 0, nil
	}
	facts, err := s.cardFacts(db)
	if err != nil {
		return 0, err
	}
	tok := lexical.FromLanguage(s.memorySearch())
	naive, loc, boundary := s.naiveZone(), s.instanceZone(), s.Doc().Memory.Boundary()
	changed := 0
	optedOut := s.profileWithdrawn(db)
	for key, list := range facts {
		guild, ref, _ := strings.Cut(key, "\x00")
		var want []cardWant
		if !optedOut[ref] { // "don't analyze me": nothing is wanted, and saveCard deletes what slipped in
			want = s.cardItemsFor(db, list, &tok, naive, loc, boundary)
			if s.guildPrivacy(guild).NoSensitive {
				want = s.dropSensitive(ctx, want)
			}
		}
		n, err := s.saveCard(ctx, db, guild, ref, want)
		if err != nil {
			return changed, err
		}
		changed += n
	}
	return changed, nil
}

func (s *Service) memorySearch() lang.Search {
	return lang.MergeSearch(s.Doc().Memory.Languages, s.Doc().BotNames())
}

// cardFacts are the active fact items people said about themselves in this server's episodes, by
// guild and person.
func (s *Service) cardFacts(db *storage.Store) (map[string][]cardFact, error) {
	rows, err := db.Reader().Query(`SELECT i.id, i.text, i.evidence, i.by_refs, e.id, e.guild_id, e.importance, COALESCE(x.key,''), COALESCE(x.person_ref,'')
		FROM episode_item i JOIN channel_episode e ON e.id=i.episode_id LEFT JOIN memory_explicit x ON x.episode_id=e.id
		WHERE i.state='active' AND i.kind='fact' AND e.guild_id<>'' AND e.source NOT LIKE 'kakao:%'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]cardFact{}
	for rows.Next() {
		var f cardFact
		var evRaw, byRaw, guild, explicitRef string
		if err := rows.Scan(&f.item, &f.text, &evRaw, &byRaw, &f.episode, &guild, &f.importance, &f.explicitKey, &explicitRef); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(evRaw), &f.evidence)
		var by []string
		_ = json.Unmarshal([]byte(byRaw), &by)
		subjects := map[string]bool{}
		for _, r := range episodes.TextRefs(f.text) {
			subjects[r] = true
		}
		for _, ref := range by {
			// about themselves: the person said it and the sentence is about them
			if ref == "" || strings.HasPrefix(ref, "kakao:") || strings.HasPrefix(ref, "label:") || !subjects[ref] {
				continue
			}
			if explicitRef != "" && explicitRef != ref {
				continue
			}
			k := guild + "\x00" + ref
			fc := f
			if explicitRef == "" {
				fc.explicitKey = ""
			}
			out[k] = append(out[k], fc)
		}
	}
	return out, rows.Err()
}

type cardWant struct {
	key, text, origin string
	evidence          []string
	episodes          []string
	at                string
}

// cardItemsFor groups a person's facts into the same fact (token overlap) and keeps the groups past the
// promotion threshold, plus every explicit fact.
func (s *Service) cardItemsFor(db *storage.Store, facts []cardFact, tok *lexical.Tokenizer, naive, loc *time.Location, boundary string) []cardWant {
	var out []cardWant
	var auto []cardFact
	for _, f := range facts {
		if f.explicitKey != "" {
			out = append(out, cardWant{key: "explicit:" + f.explicitKey, text: f.text, origin: "explicit", evidence: f.evidence, episodes: []string{f.episode}})
			continue
		}
		if f.importance < cardMinImport {
			continue
		}
		f.tokens = map[string]bool{}
		for _, w := range tok.QueryWords(storedRefPattern.ReplaceAllString(f.text, " ")) {
			for _, g := range strings.Fields(tok.ToNgrams(strings.ToLower(w))) {
				f.tokens[g] = true
			}
		}
		auto = append(auto, f)
	}
	sort.SliceStable(auto, func(i, j int) bool { return auto[i].item < auto[j].item })
	used := make([]bool, len(auto))
	for i := range auto {
		if used[i] {
			continue
		}
		group := []cardFact{auto[i]}
		used[i] = true
		for j := i + 1; j < len(auto); j++ {
			if !used[j] && overlap(auto[i].tokens, auto[j].tokens) >= cardSimilar {
				group = append(group, auto[j])
				used[j] = true
			}
		}
		if w, ok := s.promotable(db, group, naive, loc, boundary); ok {
			out = append(out, w)
		}
	}
	return out
}

var storedRefPattern = regexp.MustCompile(`\{@[^{}]+\}`)

func overlap(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	shared := 0
	for t := range a {
		if b[t] {
			shared++
		}
	}
	return float64(shared) / float64(min(len(a), len(b)))
}

// promotable applies the threshold to one group of the same fact: two episodes, two days, human
// evidence only.
func (s *Service) promotable(db *storage.Store, group []cardFact, naive, loc *time.Location, boundary string) (cardWant, bool) {
	eps := map[string]bool{}
	var evidence []string
	for _, f := range group {
		eps[f.episode] = true
		evidence = append(evidence, f.evidence...)
	}
	if len(eps) < cardMinEpisodes {
		return cardWant{}, false
	}
	evidence = uniqueStrings(evidence)
	raw, _ := json.Marshal(evidence)
	rows, err := db.Reader().Query(`SELECT record_kind, parsed_time, deleted_at FROM memory_observations WHERE observation_id IN (SELECT value FROM json_each(?))`, string(raw))
	if err != nil {
		return cardWant{}, false
	}
	days := map[string]bool{}
	found := 0
	for rows.Next() {
		var kind, at, deleted string
		if rows.Scan(&kind, &at, &deleted) != nil {
			continue
		}
		found++
		if kind != corpus.HumanText || deleted != "" {
			rows.Close()
			return cardWant{}, false // bot-sourced or gone: never a card
		}
		if t, ok := s.obsTime(at, naive); ok {
			days[episodes.DayKey(t, boundary, loc)] = true
		}
	}
	rows.Close()
	if found != len(evidence) || len(days) < cardMinDays {
		return cardWant{}, false
	}
	latest := group[len(group)-1]
	var epList []string
	for e := range eps {
		epList = append(epList, e)
	}
	sort.Strings(epList)
	if len(evidence) > 8 {
		evidence = evidence[len(evidence)-8:]
	}
	return cardWant{key: "auto:" + group[0].item, text: latest.text, origin: "promoted", evidence: evidence, episodes: epList}, true
}

// saveCard makes a person's active card items what want says: new and changed items are written (a
// changed one supersedes the old), items no longer wanted are deleted, and the card is cut to
// cardBudget runes keeping explicit items first, then the newest.
func (s *Service) saveCard(ctx context.Context, db *storage.Store, guild, ref string, want []cardWant) (int, error) {
	sort.SliceStable(want, func(i, j int) bool {
		if (want[i].origin == "explicit") != (want[j].origin == "explicit") {
			return want[i].origin == "explicit"
		}
		return want[i].key > want[j].key
	})
	var kept []cardWant
	used := 0
	for _, w := range want {
		n := len([]rune(w.text))
		if used+n > cardBudget {
			continue
		}
		used += n
		kept = append(kept, w)
	}
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	changed := 0
	err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		if blocked, err := store.SourceBlocked(tx, ref); err != nil || blocked {
			return err
		}
		// "don't analyze me" may have come after this pass read the consents (a classifier call takes
		// seconds): read it again under the writer, so a withdrawal is either before this or deletes after it
		if withdrawn, err := profileWithdrawnTx(tx, ref); err != nil || withdrawn {
			if err == nil {
				changed, err = deleteCardsOf(tx, "", ref)
			}
			return err
		}
		type cur struct{ id, text, evidence string }
		existing := map[string]cur{}
		rows, err := tx.Query(`SELECT id, key, text, evidence FROM person_card_item WHERE guild_id=? AND person_ref=? AND state='active'`, guild, ref)
		if err != nil {
			return err
		}
		for rows.Next() {
			var c cur
			var key string
			if rows.Scan(&c.id, &key, &c.text, &c.evidence) == nil {
				existing[key] = c
			}
		}
		rows.Close()
		wanted := map[string]bool{}
		for _, w := range kept {
			wanted[w.key] = true
			ev, _ := json.Marshal(w.evidence)
			if c, ok := existing[w.key]; ok && c.text == w.text && c.evidence == string(ev) {
				continue
			}
			if c, ok := existing[w.key]; ok {
				if _, err := tx.Exec(`UPDATE person_card_item SET state='superseded', updated_at=? WHERE id=?`, now, c.id); err != nil {
					return err
				}
				if _, err := tx.Exec(`DELETE FROM evidence_refs WHERE holder_kind=? AND holder_id=?`, holderCardItem, c.id); err != nil {
					return err
				}
			}
			id := newID("pc_")
			eps, _ := json.Marshal(w.episodes)
			if _, err := tx.Exec(`INSERT INTO person_card_item(id,guild_id,person_ref,key,text,evidence,episodes,origin,state,created_at,updated_at) VALUES(?,?,?,?,?,?,?,?,'active',?,?)`,
				id, guild, ref, w.key, w.text, string(ev), string(eps), w.origin, now, now); err != nil {
				return err
			}
			keys, err := copySources(tx, w.evidence, append(episodes.TextRefs(w.text), ref))
			if err != nil {
				return err
			}
			if err := indexCopy(tx, holderCardItem, id, keys, false); err != nil {
				return err
			}
			changed++
		}
		for key, c := range existing {
			if !wanted[key] {
				if err := deleteCardItemTx(tx, c.id); err != nil {
					return err
				}
				changed++
			}
		}
		// superseded history is kept short: the newest few per person
		_, err = tx.Exec(`DELETE FROM person_card_item WHERE guild_id=? AND person_ref=? AND state='superseded' AND id NOT IN
			(SELECT id FROM person_card_item WHERE guild_id=? AND person_ref=? AND state='superseded' ORDER BY updated_at DESC LIMIT 10)`, guild, ref, guild, ref)
		return err
	})
	return changed, err
}

func deleteCardItemTx(tx *sql.Tx, id string) error {
	if _, err := tx.Exec(`DELETE FROM evidence_refs WHERE holder_kind=? AND holder_id=?`, holderCardItem, id); err != nil {
		return err
	}
	_, err := tx.Exec(`DELETE FROM person_card_item WHERE id=?`, id)
	return err
}

// deleteCardsOf deletes a person's cards (all servers when guild is "") and relations.
func deleteCardsOf(tx *sql.Tx, guild, ref string) (int, error) {
	if !tableExists(tx, "person_card_item") {
		return 0, nil
	}
	q := `SELECT id FROM person_card_item WHERE person_ref=?`
	args := []any{ref}
	if guild != "" {
		q += ` AND guild_id=?`
		args = append(args, guild)
	}
	rows, err := tx.Query(q, args...)
	if err != nil {
		return 0, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			ids = append(ids, id)
		}
	}
	rows.Close()
	for _, id := range ids {
		if err := deleteCardItemTx(tx, id); err != nil {
			return 0, err
		}
	}
	rq := `DELETE FROM person_relation WHERE (a_ref=? OR b_ref=?)`
	rargs := []any{ref, ref}
	if guild != "" {
		rq += ` AND guild_id=?`
		rargs = append(rargs, guild)
	}
	if _, err := tx.Exec(rq, rargs...); err != nil {
		return 0, err
	}
	return len(ids), nil
}

func tableExists(tx *sql.Tx, name string) bool {
	var n int
	return tx.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name=?`, name).Scan(&n) == nil && n > 0
}

// invalidateCards applies redaction jobs to person cards: an item whose evidence was deleted, edited or
// withdrawn is deleted, and a withdrawn person loses every card and relation.
func (s *Service) invalidateCards(ctx context.Context, db *storage.Store, jobs []redactionJob) error {
	if !db.IsSQLite() {
		return nil
	}
	return db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		if !tableExists(tx, "person_card_item") {
			return nil
		}
		for _, j := range jobs {
			keys := uniqueStrings(append(append(append([]string{}, j.Messages...), j.Obs...), j.Leaves...))
			if j.Ref != "" {
				keys = append(keys, j.Ref)
			}
			if len(keys) > 0 {
				raw, _ := json.Marshal(keys)
				rows, err := tx.Query(`SELECT DISTINCT holder_id FROM evidence_refs WHERE holder_kind=? AND source_id IN (SELECT value FROM json_each(?))`, holderCardItem, string(raw))
				if err != nil {
					return err
				}
				var ids []string
				for rows.Next() {
					var id string
					if rows.Scan(&id) == nil {
						ids = append(ids, id)
					}
				}
				rows.Close()
				for _, id := range ids {
					if err := deleteCardItemTx(tx, id); err != nil {
						return err
					}
				}
			}
			op := j.Operation
			if op == "" && j.User != "" {
				op = "withdraw"
			}
			if (op == "withdraw" || op == "withdraw_batch") && j.Ref != "" {
				if _, err := deleteCardsOf(tx, "", j.Ref); err != nil {
					return err
				}
				if _, err := tx.Exec(`DELETE FROM memory_explicit WHERE person_ref=?`, j.Ref); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// cardOf reads a person's active card items in a server, explicit ones first, rendered with names.
func (s *Service) cardOf(db *storage.Store, guild, ref string, labelOf func(string) string) []string {
	return s.cardIn(db, guild, ref, labelOf, nil)
}

// cardIn is cardOf limited to the items whose evidence is all in rooms (nil = every item): a promoted
// item merges evidence from every channel the fact came up in, and its text may be from any of them.
func (s *Service) cardIn(db *storage.Store, guild, ref string, labelOf func(string) string, rooms []string) []string {
	q := `SELECT text FROM person_card_item p WHERE guild_id=? AND person_ref=? AND state='active'`
	args := []any{guild, ref}
	if rooms != nil {
		raw, _ := json.Marshal(rooms)
		q += ` AND json_array_length(p.evidence)>0 AND NOT EXISTS (SELECT 1 FROM json_each(p.evidence) e LEFT JOIN memory_observations o ON o.observation_id=e.value
			WHERE o.room IS NULL OR o.room NOT IN (SELECT value FROM json_each(?)))`
		args = append(args, string(raw))
	}
	rows, err := db.Reader().Query(q+` ORDER BY CASE origin WHEN 'explicit' THEN 0 ELSE 1 END, updated_at DESC`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if rows.Scan(&t) == nil {
			out = append(out, episodes.StoredText(t, labelOf, "?"))
		}
	}
	return out
}

var mentionPattern = regexp.MustCompile(`<@!?(\d+)>`)

// turnCards are the caller's card and those of up to two other people of the caller's conversation (its
// speakers and the people the message mentions), when memory.cards is on.
func (s *Service) turnCards(db *storage.Store, in ChatInput, recent []corpus.Message, sources []string) []chat.RoomCard {
	if in.GuildID == "" || !s.MemoryAllowed(in.SpeakerID) {
		return nil
	}
	var check int
	if db.Reader().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='person_card_item'`).Scan(&check) != nil || check == 0 {
		return nil
	}
	room := roomOf(in.GuildID, in.ChannelID)
	labels := s.refLabels(db, room)
	caller := personRef(in.SpeakerID)
	strict := s.guildPrivacy(in.GuildID)
	var rooms []string // card_channel_only: the items from this channel and its threads
	if strict.CardChannelOnly {
		rooms = []string{}
		for _, r := range s.channelRooms(in.GuildID, in.ChannelID) {
			if !strings.HasPrefix(r, "kakao:") {
				rooms = append(rooms, r)
			}
		}
	}
	labelOf := func(ref string) string {
		if ref == caller && in.Speaker != "" {
			return in.Speaker
		}
		return labels(ref)
	}
	var out []chat.RoomCard
	if !s.ConsentAllows(in.SpeakerID, privacy.Profile) {
		// "don't analyze me": no card of theirs (withdrawing deleted it; this covers the time in between)
	} else if facts := s.cardIn(db, in.GuildID, caller, labelOf, rooms); len(facts) > 0 {
		name := in.Speaker
		if name == "" {
			name = labels(caller)
		}
		out = append(out, chat.RoomCard{Name: name, Facts: facts, Caller: true})
	}
	// other people of the conversation, newest first
	type person struct{ account, label string }
	var people []person
	seen := map[string]bool{in.SpeakerID: true, "": true}
	for _, m := range mentionPattern.FindAllStringSubmatch(in.Message, -1) {
		if !seen[m[1]] {
			seen[m[1]] = true
			people = append(people, person{account: m[1]})
		}
	}
	for i := len(recent) - 1; i >= 0; i-- {
		if a := recent[i].ActorAccount; !seen[a] {
			seen[a] = true
			people = append(people, person{account: a, label: recent[i].ActorLabel})
		}
	}
	if len(sources) > 0 {
		raw, _ := json.Marshal(sources)
		rows, err := db.Reader().Query(`SELECT actor_account, actor_label FROM memory_observations WHERE observation_id IN (SELECT value FROM json_each(?))
			AND actor_account<>'' AND record_kind=? ORDER BY parsed_time DESC LIMIT 40`, string(raw), corpus.HumanText)
		if err == nil {
			for rows.Next() {
				var a, l string
				if rows.Scan(&a, &l) == nil && !seen[a] {
					seen[a] = true
					people = append(people, person{account: a, label: l})
				}
			}
			rows.Close()
		}
	}
	others := 0
	botID := s.botAccount(nil)
	if strict.OwnCardOnly {
		people = nil // own_card_only: only the caller's card
	}
	for _, p := range people {
		if others == chat.MaxOtherCards {
			break
		}
		if p.account == botID || !s.MemoryAllowed(p.account) || !s.ConsentAllows(p.account, privacy.Profile) {
			continue
		}
		ref := personRef(p.account)
		facts := s.cardIn(db, in.GuildID, ref, labelOf, rooms)
		if len(facts) == 0 {
			continue
		}
		name := p.label
		if name == "" {
			name = labels(ref)
		}
		if name == "" {
			continue
		}
		out = append(out, chat.RoomCard{Name: name, Facts: facts})
		others++
	}
	return out
}

// relationKey is the stored pair, in a fixed order.
func relationKey(a, b string) (string, string) {
	if a > b {
		return b, a
	}
	return a, b
}

func relCursorKey(guild, channel string) string { return "cards:relations:" + guild + ":" + channel }

// countRelations adds the reply and mention exchanges of a room's new live lines to the pair counts.
func (s *Service) countRelations(ctx context.Context, db *storage.Store, r memRoom) error {
	if r.kakao() || r.guild == "" {
		return nil
	}
	lines := s.liveLines(r.guild, r.channel)
	if len(lines) == 0 {
		return nil
	}
	var last string
	_, _ = s.kvGet(relCursorKey(r.guild, r.channel), &last)
	authorOf := map[string]conversation.Line{}
	bots := map[string]bool{}
	for _, l := range lines {
		authorOf[l.ID] = l
		if l.Bot {
			bots[l.AuthorID] = true
		}
	}
	type pair struct{ a, b string }
	replies, mentions := map[pair]int{}, map[pair]int{}
	newest := last
	for _, l := range lines {
		stamp := l.At.UTC().Format(time.RFC3339Nano) + "|" + l.ID
		if stamp <= last {
			continue
		}
		if stamp > newest {
			newest = stamp
		}
		if l.Bot || l.AuthorID == "" || !s.MemoryAllowed(l.AuthorID) {
			continue
		}
		from := personRef(l.AuthorID)
		if to, ok := authorOf[l.ReplyTo]; ok && !to.Bot && to.AuthorID != "" && to.AuthorID != l.AuthorID && s.MemoryAllowed(to.AuthorID) {
			a, b := relationKey(from, personRef(to.AuthorID))
			replies[pair{a, b}]++
		}
		for _, m := range l.Mentions {
			if m == "" || m == l.AuthorID || bots[m] || !s.MemoryAllowed(m) {
				continue
			}
			a, b := relationKey(from, personRef(m))
			mentions[pair{a, b}]++
		}
	}
	if len(replies)+len(mentions) > 0 {
		now := s.Clock.Now().UTC().Format(time.RFC3339)
		err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
			for _, set := range []struct {
				m   map[pair]int
				col string
			}{{replies, "replies"}, {mentions, "mentions"}} {
				for p, n := range set.m {
					if _, err := tx.Exec(`INSERT INTO person_relation(guild_id,a_ref,b_ref,`+set.col+`,updated_at) VALUES(?,?,?,?,?)
						ON CONFLICT(guild_id,a_ref,b_ref) DO UPDATE SET `+set.col+`=`+set.col+`+excluded.`+set.col+`, updated_at=excluded.updated_at`,
						r.guild, p.a, p.b, n, now); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if newest != last {
		return s.kvSet(relCursorKey(r.guild, r.channel), newest)
	}
	return nil
}

// cardsPass is room memory's model-free card work: relation counts of the capturing rooms, then
// promotion.
func (s *Service) cardsPass(ctx context.Context, db *storage.Store, rooms []memRoom) {
	if !s.Doc().Memory.CardsOn() {
		return
	}
	for _, r := range rooms {
		if err := s.countRelations(ctx, db, r); err != nil && !errors.Is(err, context.Canceled) {
			s.Log.Warn("memory: relation counts not saved", "err", err)
		}
	}
	if _, err := s.promoteCards(ctx, db); err != nil && !errors.Is(err, context.Canceled) {
		s.Log.Warn("memory: person cards not updated", "err", err)
	}
}
