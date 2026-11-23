package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"regexp"
	"time"

	"masamong/internal/conversation"
	"masamong/internal/features/privacy"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// Person memory's older parts (L3).
//
//   - person_card_item: the cards of releases before v1.7.0 (a fact a person said about themselves on two
//     days). The table is left in place and is no longer read or written (ops had no rows); forgetting,
//     withdrawal and invalidation still delete what may be in it. Person memory is person_claim now
//     (person_claims*.go).
//   - person_relation: counts of reply and mention exchanges per pair of people of a server, counted
//     without a model from the live lines; no message text, no IDs other than the one-way references.
//     Counted whenever person memory is on, never for someone who withdrew profile or memory.
//   - memory_explicit: the facts a person asked to have remembered about themselves, with the
//     classifier's topic key (a newer fact with the same key replaces the older one).
//   - KakaoTalk speakers never get person memory: they are kakao:<room>:<label> references, not people
//     of a server, and are never merged with a Discord account.

const (
	holderCardItem = "person_card_item"
	cardSimilar    = 0.5 // token overlap that makes two statements the same statement
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

// cardWant is one statement for the no_sensitive classifier (dropSensitive).
type cardWant struct {
	text string
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

// invalidateCards applies redaction jobs to person memory: a card item or person claim whose evidence
// was deleted, edited or withdrawn is deleted, and a withdrawn person loses every card, claim (about them
// or said by them) and relation.
func (s *Service) invalidateCards(ctx context.Context, db *storage.Store, jobs []redactionJob) error {
	if !db.IsSQLite() {
		return nil
	}
	return db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		cards, claims := tableExists(tx, "person_card_item"), tableExists(tx, "person_claim")
		if !cards && !claims {
			return nil
		}
		for _, j := range jobs {
			keys := uniqueStrings(append(append(append([]string{}, j.Messages...), j.Obs...), j.Leaves...))
			if j.Ref != "" {
				keys = append(keys, j.Ref)
			}
			if len(keys) > 0 && cards {
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
			if len(keys) > 0 && claims {
				if _, err := deleteClaimsCiting(tx, keys); err != nil {
					return err
				}
			}
			op := j.Operation
			if op == "" && j.User != "" {
				op = "withdraw"
			}
			if (op == "withdraw" || op == "withdraw_batch") && j.Ref != "" {
				if tableExists(tx, "person_alias") {
					if _, err := tx.Exec(`DELETE FROM person_alias_source WHERE person_ref=?`, j.Ref); err != nil {
						return err
					}
					if _, err := tx.Exec(`DELETE FROM person_alias WHERE person_ref=?`, j.Ref); err != nil {
						return err
					}
				}
				if _, err := deleteCardsOf(tx, "", j.Ref); err != nil {
					return err
				}
				if _, err := deleteClaimsOf(tx, "", j.Ref, true); err != nil {
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

var mentionPattern = regexp.MustCompile(`<@!?(\d+)>`)

// Only the gateway's ordinary text channels and positively known public threads
// can supply statements outside the current group. A private thread deliberately
// has no parent record, so an unknown room is never assumed to be public.
// It must be the first read on tx: the thread deletion boundary below relies on it.
func (s *Service) personRooms(tx *sql.Tx, guild, channel string, localOnly bool) map[string]bool {
	rooms := map[string]bool{} // value = local to this channel group
	gw, ok := s.Discord().(interface{ PersonMemoryChannels(string) map[string]bool })
	if !ok || channel == "" {
		return rooms
	}
	// A deleted thread leaves memory before its tombstone commits (threadRooms).
	// The generation is read before this snapshot starts and checked after it.
	t := s.threadsLoaded()
	t.mu.Lock()
	gen, gone := t.scopeGen, t.deleted[channel]
	t.mu.Unlock()
	if gone {
		return rooms
	}
	var deleted bool
	if tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM thread_tombstones WHERE thread_id=?)`, channel).Scan(&deleted) != nil || deleted {
		return rooms
	}
	channels := gw.PersonMemoryChannels(guild)
	parent := channel
	threads := map[string]string{}
	rows, err := tx.Query(`SELECT thread_id,parent_id FROM channel_threads WHERE guild_id=? AND `+threadVisibleSQL("channel_threads"), guild)
	if err != nil {
		return rooms
	}
	defer rows.Close()
	for rows.Next() {
		var id, p string
		if rows.Scan(&id, &p) != nil {
			return rooms
		}
		threads[id] = p
	}
	if rows.Err() != nil {
		return rooms
	}
	t.mu.Lock()
	settled := gen == t.scopeGen
	for id := range threads {
		// After a lifecycle change during the read, only the in-memory table (which
		// never holds a deleted thread) vouches for a stored row.
		if _, live := t.threads[id]; t.deleted[id] || !settled && !live {
			delete(threads, id)
		}
	}
	gone = t.deleted[channel]
	t.mu.Unlock()
	if gone {
		return rooms
	}
	if p := threads[channel]; p != "" {
		parent = p
	}
	if public, known := channels[parent]; !known || !public {
		if !known {
			return rooms
		}
		// A private/restricted room is its own group: its statements and hearsay
		// stay local, while a person's own public statements remain usable here.
		rooms[roomOf(guild, channel)] = true
		if localOnly {
			return rooms
		}
		parent = ""
	}
	add := func(id, p string) {
		capture := s.ownChannelPolicy(guild, p).Capture
		if id != p && s.hasChannelPolicy(guild, id) {
			capture = s.ownChannelPolicy(guild, id).Capture
		}
		if capture && channels[p] && (!localOnly || p == parent) {
			rooms[roomOf(guild, id)] = p == parent
		}
	}
	for id := range channels {
		add(id, id)
	}
	for id, p := range threads {
		add(id, p)
	}
	return rooms
}

// relationKey is the stored pair, in a fixed order.
func relationKey(a, b string) (string, string) {
	if a > b {
		return b, a
	}
	return a, b
}

func relCursorKey(guild, channel string) string { return "cards:relations:" + guild + ":" + channel }

// countRelations adds the reply and mention exchanges of a room's new live lines to the pair counts. A
// pair with someone who withdrew memory or profile, or whose forget marker covers the line, is not
// counted: checked again in the write transaction, so a withdrawal or forget that commits after the
// lines were read is not undone.
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
	type exchange struct{ from, to, col, at string }
	var seen []exchange
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
		at := l.At.UTC().Format(store.PersonClaimStamp)
		if to, ok := authorOf[l.ReplyTo]; ok && !to.Bot && to.AuthorID != "" && to.AuthorID != l.AuthorID && s.MemoryAllowed(to.AuthorID) {
			seen = append(seen, exchange{from, personRef(to.AuthorID), "replies", at})
		}
		for _, m := range l.Mentions {
			if m == "" || m == l.AuthorID || bots[m] || !s.MemoryAllowed(m) {
				continue
			}
			seen = append(seen, exchange{from, personRef(m), "mentions", at})
		}
	}
	if len(seen) > 0 {
		now := s.Clock.Now().UTC().Format(time.RFC3339)
		err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
			gone, err := s.relationGone(tx, r.guild)
			if err != nil {
				return err
			}
			type pair struct{ a, b string }
			replies, mentions := map[pair]int{}, map[pair]int{}
			for _, e := range seen {
				if gone(e.from, e.at) || gone(e.to, e.at) {
					continue
				}
				a, b := relationKey(e.from, e.to)
				if e.col == "replies" {
					replies[pair{a, b}]++
				} else {
					mentions[pair{a, b}]++
				}
			}
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

// relationGone is the relation counters' check, on the write transaction, of a person and the time of a
// line: true when they withdrew memory or profile, are blocked or tombstoned, or have a forget marker
// (person_memory_suppress, all) at or after the line.
func (s *Service) relationGone(tx *sql.Tx, guild string) (func(ref, at string) bool, error) {
	out, err := withdrawnRefsTx(tx, privacy.Memory)
	if err != nil {
		return nil, err
	}
	profileOut, err := withdrawnRefsTx(tx, privacy.Profile)
	if err != nil {
		return nil, err
	}
	blocked := map[string]bool{}
	doc := s.Doc()
	for _, list := range [][]string{doc.Access.BlockedUsers, doc.Guilds[guild].BlockedUsers} {
		for _, id := range list {
			blocked[personRef(id)] = true
		}
	}
	forgot := map[string]string{} // ref -> newest forget marker
	if tableExists(tx, "person_memory_suppress") {
		rows, err := tx.Query(`SELECT person_ref,MAX(before_ts) FROM person_memory_suppress WHERE guild_id=? AND scope='all' GROUP BY person_ref`, guild)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var ref, ts string
			if rows.Scan(&ref, &ts) == nil {
				forgot[ref] = ts
			}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	goneOf := map[string]bool{}
	return func(ref, at string) bool {
		if forgot[ref] != "" && at <= forgot[ref] {
			return true
		}
		v, ok := goneOf[ref]
		if !ok {
			v = out[ref] || profileOut[ref] || blocked[ref]
			if !v {
				b, err := store.SourceBlocked(tx, ref)
				v = err != nil || b
			}
			goneOf[ref] = v
		}
		return v
	}, nil
}

// relFillKey marks a server whose pair counts were recomputed from the stored messages (v1.7.0).
func relFillKey(guild string) string { return "people:relations:fill:" + guild }

// relationsFill recomputes, once per server, the mention counts from the stored messages and replaces
// what the live counter had (never adds to it): a mention of a person by another in a captured human line
// of the server counts, except where either of them withdrew memory or profile, is blocked, or has a
// forget marker (person_memory_suppress, all) at or after the line. Replies are not stored with the
// messages, so the reply counts stay the live ones. The live cursors of the server's rooms move to the
// newest live line seen before the scan, and the scan leaves out a room's lines after it; a live line
// before it that is not stored yet is counted from the live window. A line is counted once, by the fill
// or by the live counter, even when it arrives or is stored during the fill.
func (s *Service) relationsFill(ctx context.Context, db *storage.Store, rooms []memRoom) error {
	var guilds []string
	seen := map[string]bool{}
	for _, r := range rooms {
		if r.kakao() || r.guild == "" || r.guild == "0" || seen[r.guild] {
			continue
		}
		seen[r.guild] = true
		var done bool
		if ok, err := s.kvGet(relFillKey(r.guild), &done); err != nil {
			return err
		} else if !ok || !done {
			guilds = append(guilds, r.guild)
		}
	}
	naive := s.naiveZone()
	type liveMark struct {
		at    time.Time
		id    string
		stamp string
	}
	for _, guild := range guilds {
		if err := ctx.Err(); err != nil {
			return err
		}
		marks := map[string]liveMark{}               // room → the newest live line before the scan
		snapshot := map[string][]conversation.Line{} // room → its live lines then
		for _, r := range rooms {
			if r.guild != guild || r.kakao() {
				continue
			}
			lines := s.liveLines(r.guild, r.channel)
			snapshot[roomOf(r.guild, r.channel)] = lines
			for _, l := range lines {
				stamp := l.At.UTC().Format(time.RFC3339Nano) + "|" + l.ID
				if m := marks[roomOf(r.guild, r.channel)]; stamp > m.stamp {
					marks[roomOf(r.guild, r.channel)] = liveMark{at: l.At.UTC().Truncate(time.Microsecond), id: l.ID, stamp: stamp}
				}
			}
		}
		blocked := map[string]bool{}
		doc := s.Doc()
		for _, list := range [][]string{doc.Access.BlockedUsers, doc.Guilds[guild].BlockedUsers} {
			for _, id := range list {
				blocked[personRef(id)] = true
			}
		}
		err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
			if !tableExists(tx, "memory_observations") || !tableExists(tx, "person_relation") {
				return nil
			}
			out, err := withdrawnRefsTx(tx, privacy.Memory)
			if err != nil {
				return err
			}
			profileOut, err := withdrawnRefsTx(tx, privacy.Profile)
			if err != nil {
				return err
			}
			forgot := map[string]string{} // ref -> newest forget marker
			if tableExists(tx, "person_memory_suppress") {
				rows, err := tx.Query(`SELECT person_ref,MAX(before_ts) FROM person_memory_suppress WHERE guild_id=? AND scope='all' GROUP BY person_ref`, guild)
				if err != nil {
					return err
				}
				for rows.Next() {
					var ref, ts string
					if rows.Scan(&ref, &ts) == nil {
						forgot[ref] = ts
					}
				}
				rows.Close()
			}
			bots := map[string]bool{}
			rows, err := tx.Query(`SELECT DISTINCT actor_account FROM memory_observations WHERE platform='discord' AND room LIKE ? AND record_kind<>? AND actor_account<>''`,
				"discord:"+guild+":%", corpus.HumanText)
			if err != nil {
				return err
			}
			for rows.Next() {
				var a string
				if rows.Scan(&a) == nil {
					bots[a] = true
				}
			}
			rows.Close()
			gone := func(ref string) bool {
				if out[ref] || profileOut[ref] || blocked[ref] {
					return true
				}
				b, err := store.SourceBlocked(tx, ref)
				return err != nil || b
			}
			type pair struct{ a, b string }
			counts := map[pair]int{}
			rows, err = tx.Query(`SELECT actor_account,content,parsed_time,room,COALESCE(native_message_id,'') FROM memory_observations WHERE platform='discord' AND room LIKE ? AND record_kind=?
				AND deleted_at='' AND source_room=room AND actor_account<>'' AND content LIKE '%<@%'`, "discord:"+guild+":%", corpus.HumanText)
			if err != nil {
				return err
			}
			type line struct{ account, content, parsed, room, native string }
			var lines []line
			for rows.Next() {
				var l line
				if rows.Scan(&l.account, &l.content, &l.parsed, &l.room, &l.native) == nil {
					lines = append(lines, l)
				}
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			goneOf := map[string]bool{}
			isGone := func(ref string) bool {
				v, ok := goneOf[ref]
				if !ok {
					v = gone(ref)
					goneOf[ref] = v
				}
				return v
			}
			for _, l := range lines {
				t, ok := s.obsTime(l.parsed, naive)
				if !ok || bots[l.account] {
					continue
				}
				if m, live := marks[l.room]; live && (t.UTC().After(m.at) || t.UTC().Equal(m.at) && l.native > m.id) {
					continue // after the live cursor: the live counter counts it
				}
				at := t.UTC().Format(store.PersonClaimStamp)
				from := personRef(l.account)
				if isGone(from) || forgot[from] != "" && at <= forgot[from] {
					continue
				}
				for _, m := range mentionPattern.FindAllStringSubmatch(l.content, -1) {
					to := personRef(m[1])
					if m[1] == l.account || bots[m[1]] || isGone(to) || forgot[to] != "" && at <= forgot[to] {
						continue
					}
					a, b := relationKey(from, to)
					counts[pair{a, b}]++
				}
			}
			// a live line the cursor moves past but that is not stored yet (captured after the scan) is
			// counted here, from the live window: the live counter will not see it again
			for room, lines := range snapshot {
				for _, l := range lines {
					if l.Bot || l.AuthorID == "" || bots[l.AuthorID] || len(l.Mentions) == 0 {
						continue
					}
					var stored bool
					if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM memory_observations WHERE room=? AND native_message_id=?)`, room, l.ID).Scan(&stored); err != nil {
						return err
					}
					if stored {
						continue
					}
					at := l.At.UTC().Format(store.PersonClaimStamp)
					from := personRef(l.AuthorID)
					if isGone(from) || forgot[from] != "" && at <= forgot[from] {
						continue
					}
					for _, m := range l.Mentions {
						to := personRef(m)
						if m == "" || m == l.AuthorID || bots[m] || isGone(to) || forgot[to] != "" && at <= forgot[to] {
							continue
						}
						a, b := relationKey(from, to)
						counts[pair{a, b}]++
					}
				}
			}
			now := s.Clock.Now().UTC().Format(time.RFC3339)
			if _, err := tx.Exec(`UPDATE person_relation SET mentions=0, updated_at=? WHERE guild_id=? AND mentions<>0`, now, guild); err != nil {
				return err
			}
			for p, n := range counts {
				if _, err := tx.Exec(`INSERT INTO person_relation(guild_id,a_ref,b_ref,mentions,updated_at) VALUES(?,?,?,?,?)
					ON CONFLICT(guild_id,a_ref,b_ref) DO UPDATE SET mentions=excluded.mentions, updated_at=excluded.updated_at`, guild, p.a, p.b, n, now); err != nil {
					return err
				}
			}
			_, err = tx.Exec(`DELETE FROM person_relation WHERE guild_id=? AND replies=0 AND mentions=0`, guild)
			return err
		})
		if err != nil {
			return err
		}
		for _, r := range rooms {
			if r.guild != guild || r.kakao() {
				continue
			}
			if m := marks[roomOf(r.guild, r.channel)]; m.stamp != "" {
				if err := s.kvSet(relCursorKey(r.guild, r.channel), m.stamp); err != nil {
					return err
				}
			}
		}
		if err := s.kvSet(relFillKey(guild), true); err != nil {
			return err
		}
	}
	return nil
}

// cardsPass is room memory's model-free person work: the one-time relation fill and the relation counts
// of the capturing rooms, then the person claims (derivation, sweep, live classification). It runs when
// person memory is on (memory.people; memory.cards: off is the old name for off).
func (s *Service) cardsPass(ctx context.Context, db *storage.Store, rooms []memRoom) {
	if !s.Doc().Memory.PeopleOn() {
		return
	}
	if err := s.relationsFill(ctx, db, rooms); err != nil && !errors.Is(err, context.Canceled) {
		s.Log.Warn("memory: relation counts not recomputed", "err", err)
	}
	for _, r := range rooms {
		if err := s.countRelations(ctx, db, r); err != nil && !errors.Is(err, context.Canceled) {
			s.Log.Warn("memory: relation counts not saved", "err", err)
		}
	}
	s.peopleClaimsPass(ctx, db)
}
