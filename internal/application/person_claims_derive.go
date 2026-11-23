package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"masamong/internal/memory/episodes"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// Person claims derived from episode items (contract person-memory-v170 §2.2), with no model call.
//
//   - An active item of kind fact gives one claim per subject {@X} in its text and per speaker in its
//     by_refs: the speaker is the subject (self) or not (said). A moment gives only the self claims of
//     its speakers. The claim's evidence is the item's evidence written by that speaker; a speaker who
//     wrote none of it gives no claim.
//   - A self claim (server-wide) needs the whole item to be the subject's own: it names nobody else, and
//     the subject wrote every line of it. An item that also names someone else, or rests on another
//     speaker's lines too, gives only what others said (hearsay, local to its channel group): its words
//     may carry what someone else said.
//   - Declarative: claims with source episode_item:<id> live as long as that item is active. A new item
//     derives its claims in the transaction that saves it (insertItems); the room-memory pass walks every
//     guild's items once with a kv cursor (people:derive:<guild>, the one-time pass for the items saved
//     before this release and a safety net after it) and deletes the claims of items that were hidden,
//     made dirty or removed. Both are idempotent: a claim's id is its statement.
//   - Kakao items, label: speakers and bot lines never give a claim (the write gate refuses them).

const (
	deriveBatch   = 200  // items per write transaction
	derivePerPass = 2000 // items per guild and pass
	derivedKind   = "other"
)

func deriveCursorKey(guild string) string { return "people:derive:" + guild }

// deriveCursor is a guild's progress: the last episode_item rowid walked, and when the walk first
// reached the end with the newest item rowid then (Boundary). Items after Boundary were saved after the
// release (their claims are live ones, derived when they were saved).
type deriveCursor struct {
	Last     int64  `json:"last"`
	Done     string `json:"done,omitempty"`
	Boundary int64  `json:"boundary,omitempty"`
}

type deriveItem struct {
	rowid                 int64
	id, kind, text, saved string
	evidence, by          []string
}

// derivedClaims are the claims one item gives; authors maps its evidence to the person reference of
// their writer.
func derivedClaims(guild string, it deriveItem, authors map[string]string) []store.PersonClaim {
	if it.kind != "fact" && it.kind != "moment" {
		return nil
	}
	var subjects []string
	named := 0 // people the text names (a self claim names only its subject)
	for _, ref := range uniqueStrings(episodes.TextRefs(it.text)) {
		named++
		if !store.DiscordPersonRef(ref) {
			continue
		}
		if it.kind == "moment" && !explicitContains(it.by, ref) {
			continue // a moment is kept only as its own speakers' statement about themselves
		}
		subjects = append(subjects, ref)
	}
	var out []store.PersonClaim
	for _, speaker := range uniqueStrings(it.by) {
		if !store.DiscordPersonRef(speaker) {
			continue
		}
		var ev []string
		for _, id := range it.evidence {
			if authors[id] == speaker {
				ev = append(ev, id)
			}
		}
		if len(ev) == 0 {
			continue
		}
		for _, subject := range subjects {
			if it.kind == "moment" && subject != speaker {
				continue
			}
			link := store.LinkMention
			if subject == speaker {
				if named != 1 || len(uniqueStrings(ev)) != len(uniqueStrings(it.evidence)) {
					continue // not all theirs: another person named, or another speaker's lines
				}
				link = store.LinkSelf
			}
			out = append(out, store.PersonClaim{GuildID: guild, SubjectRef: subject, SpeakerRef: speaker, Kind: derivedKind, Text: it.text,
				Evidence: ev, Source: "episode_item:" + it.id, Link: link, ReadAt: it.saved})
		}
	}
	return out
}

// evidenceAuthors reads who wrote each evidence observation (Discord lines only) and the guild of the
// first one's room ("" when it is not a Discord room of a server).
func evidenceAuthors(q interface {
	Query(string, ...any) (*sql.Rows, error)
}, ids []string) (map[string]string, string, error) {
	raw, _ := json.Marshal(uniqueStrings(ids))
	rows, err := q.Query(`SELECT observation_id,room,actor_account FROM memory_observations WHERE observation_id IN (SELECT value FROM json_each(?)) AND platform='discord'`, string(raw))
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := map[string]string{}
	guild := ""
	for rows.Next() {
		var id, room, account string
		if err := rows.Scan(&id, &room, &account); err != nil {
			return nil, "", err
		}
		if account != "" {
			out[id] = personRef(account)
		}
		if rest, ok := strings.CutPrefix(room, "discord:"); ok && guild == "" {
			if g, _, ok := strings.Cut(rest, ":"); ok && g != "0" {
				guild = g
			}
		}
	}
	return out, guild, rows.Err()
}

// derivePersonClaimsTx is the hook of insertItems: the claims of one item just saved, in its
// transaction.
func (s *Service) derivePersonClaimsTx(tx *sql.Tx, it deriveItem) error {
	if !s.Doc().Memory.PeopleOn() || (it.kind != "fact" && it.kind != "moment") || !strings.Contains(it.text, "{@") {
		return nil
	}
	authors, guild, err := evidenceAuthors(tx, it.evidence)
	if err != nil || guild == "" {
		return err
	}
	_, err = s.savePersonClaims(context.Background(), tx, derivedClaims(guild, it, authors))
	return err
}

// derivePersonClaims walks the guilds' items past their cursors (bounded per pass) and saves their
// claims. It returns how many rows were written.
func (s *Service) derivePersonClaims(ctx context.Context, db *storage.Store) (int, error) {
	rows, err := db.Reader().QueryContext(ctx, `SELECT DISTINCT guild_id FROM channel_episode WHERE guild_id<>'' AND guild_id<>'0' AND source NOT LIKE 'kakao:%'`)
	if err != nil {
		return 0, err
	}
	var guilds []string
	for rows.Next() {
		var g string
		if rows.Scan(&g) == nil {
			guilds = append(guilds, g)
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	saved := 0
	for _, g := range guilds {
		n, err := s.deriveGuild(ctx, db, g)
		saved += n
		if err != nil {
			return saved, err
		}
	}
	return saved, nil
}

func (s *Service) deriveGuild(ctx context.Context, db *storage.Store, guild string) (int, error) {
	var cur deriveCursor
	if _, err := s.kvGet(deriveCursorKey(guild), &cur); err != nil {
		return 0, err
	}
	saved := 0
	for walked := 0; walked < derivePerPass; {
		if err := ctx.Err(); err != nil {
			return saved, err
		}
		items, err := deriveBatchAfter(ctx, db, guild, cur.Last)
		if err != nil {
			return saved, err
		}
		if len(items) == 0 {
			break
		}
		err = db.WithWriterContext(ctx, func(tx *sql.Tx) error {
			// one gate call per batch: the gate reads consent and the server's known names once per call
			var claims []store.PersonClaim
			for _, it := range items {
				if it.kind != "fact" && it.kind != "moment" {
					continue
				}
				authors, _, err := evidenceAuthors(tx, it.evidence)
				if err != nil {
					return err
				}
				claims = append(claims, derivedClaims(guild, it, authors)...)
			}
			n, err := s.savePersonClaims(ctx, tx, claims)
			saved += n
			return err
		})
		if err != nil {
			return saved, err
		}
		cur.Last = items[len(items)-1].rowid
		walked += len(items)
		if err := s.kvSet(deriveCursorKey(guild), cur); err != nil {
			return saved, err
		}
		if len(items) < deriveBatch {
			break
		}
	}
	if cur.Done == "" {
		if more, err := deriveBatchAfter(ctx, db, guild, cur.Last); err == nil && len(more) == 0 {
			cur.Done = s.Clock.Now().UTC().Format(time.RFC3339)
			if err := db.Reader().QueryRowContext(ctx, `SELECT COALESCE(MAX(rowid),0) FROM episode_item`).Scan(&cur.Boundary); err != nil {
				return saved, err
			}
			if err := s.kvSet(deriveCursorKey(guild), cur); err != nil {
				return saved, err
			}
		}
	}
	return saved, nil
}

// deriveBatchAfter reads the next active items of a guild after rowid last (any kind: the cursor walks
// past the others too).
func deriveBatchAfter(ctx context.Context, db *storage.Store, guild string, last int64) ([]deriveItem, error) {
	rows, err := db.Reader().QueryContext(ctx, `SELECT i.rowid,i.id,i.kind,i.text,i.evidence,i.by_refs,i.created_at FROM episode_item i JOIN channel_episode e ON e.id=i.episode_id
		WHERE e.guild_id=? AND i.rowid>? AND i.state='active' AND e.source NOT LIKE 'kakao:%' ORDER BY i.rowid LIMIT ?`, guild, last, deriveBatch)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []deriveItem
	for rows.Next() {
		var it deriveItem
		var ev, by string
		if err := rows.Scan(&it.rowid, &it.id, &it.kind, &it.text, &ev, &by, &it.saved); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(ev), &it.evidence)
		_ = json.Unmarshal([]byte(by), &it.by)
		out = append(out, it)
	}
	return out, rows.Err()
}

// sweepDerivedClaims deletes the claims whose episode item is no longer active (hidden, dirty, removed).
func (s *Service) sweepDerivedClaims(ctx context.Context, db *storage.Store) (int, error) {
	n := 0
	err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		if !tableExists(tx, "person_claim") {
			return nil
		}
		var err error
		n, err = s.sweepDerivedClaimsTx(tx, "")
		return err
	})
	return n, err
}

// sweepDerivedClaimsTx is sweepDerivedClaims inside a transaction, for one item ("" = all). A claim
// whose item went but that another active item of the same evidence still gives (a regenerated episode)
// moves to that item and keeps its state; the others are deleted.
func (s *Service) sweepDerivedClaimsTx(tx *sql.Tx, item string) (int, error) {
	q := `SELECT id,guild_id,evidence FROM person_claim c WHERE c.source>='episode_item:' AND c.source<'episode_item;'
		AND NOT EXISTS(SELECT 1 FROM episode_item i WHERE i.id=substr(c.source,14) AND i.state='active')`
	var args []any
	if item != "" {
		q = `SELECT id,guild_id,evidence FROM person_claim c WHERE c.source=? AND NOT EXISTS(SELECT 1 FROM episode_item i WHERE i.id=? AND i.state='active')`
		args = []any{"episode_item:" + item, item}
	}
	rows, err := tx.Query(q, args...)
	if err != nil {
		return 0, err
	}
	type orphan struct{ id, guild, evidence string }
	var list []orphan
	for rows.Next() {
		var o orphan
		if err := rows.Scan(&o.id, &o.guild, &o.evidence); err != nil {
			rows.Close()
			return 0, err
		}
		list = append(list, o)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	deleted := 0
	for _, o := range list {
		moved, err := repointDerivedClaim(tx, o.id, o.guild, o.evidence)
		if err != nil {
			return deleted, err
		}
		if moved {
			continue
		}
		if err := deletePersonClaimTx(tx, o.id); err != nil {
			return deleted, err
		}
		deleted++
	}
	return deleted, nil
}

// repointDerivedClaim moves a derived claim to another active item that gives the same statement.
func repointDerivedClaim(tx *sql.Tx, id, guild, evidence string) (bool, error) {
	rows, err := tx.Query(`SELECT DISTINCT i.id,i.kind,i.text,i.evidence,i.by_refs,i.created_at FROM evidence_refs r JOIN episode_item i ON i.id=r.holder_id
		WHERE r.holder_kind=? AND r.source_id IN (SELECT value FROM json_each(?)) AND i.state='active' AND i.kind IN ('fact','moment') ORDER BY i.rowid`,
		store.HolderEpisodeItem, evidence)
	if err != nil {
		return false, err
	}
	var items []deriveItem
	for rows.Next() {
		var it deriveItem
		var ev, by string
		if err := rows.Scan(&it.id, &it.kind, &it.text, &ev, &by, &it.saved); err != nil {
			rows.Close()
			return false, err
		}
		_ = json.Unmarshal([]byte(ev), &it.evidence)
		_ = json.Unmarshal([]byte(by), &it.by)
		items = append(items, it)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return false, err
	}
	for _, it := range items {
		authors, _, err := evidenceAuthors(tx, it.evidence)
		if err != nil {
			return false, err
		}
		for _, c := range derivedClaims(guild, it, authors) {
			if store.PersonClaimID(c.GuildID, c.SubjectRef, c.SpeakerRef, c.Kind, c.Topic, c.Evidence) == id {
				_, err := tx.Exec(`UPDATE person_claim SET text=?,source=? WHERE id=?`, c.Text, c.Source, id)
				return err == nil, err
			}
		}
	}
	return false, nil
}

// classifyLivePending runs the no_sensitive classifier over the live pending claims of a guild (from
// items saved after its one-time walk ended, or from live extraction), within the derived budget: a
// passed claim becomes active, a sensitive one is deleted, one that could not be classified now waits.
// Claims of the items the walk covered (saved before this release, or in its first pass) and of the
// paid backfill wait for an operator.
func (s *Service) classifyLivePending(ctx context.Context, db *storage.Store) error {
	for guild, g := range s.Doc().Guilds {
		if !g.Privacy.NoSensitive {
			continue
		}
		var cur deriveCursor
		if ok, err := s.kvGet(deriveCursorKey(guild), &cur); err != nil || !ok || cur.Done == "" {
			continue
		}
		if err := s.classifyPending(ctx, db, guild, cur.Boundary, 20); err != nil {
			return err
		}
	}
	return nil
}

// classifyPending classifies up to limit live pending claims of a guild: from live extraction, or from
// an item saved after rowid boundary.
func (s *Service) classifyPending(ctx context.Context, db *storage.Store, guild string, boundary int64, limit int) error {
	rows, err := db.Reader().QueryContext(ctx, `SELECT c.id,c.text FROM person_claim c WHERE c.guild_id=? AND c.state=?
		AND (c.source LIKE 'extract:%' OR EXISTS(SELECT 1 FROM episode_item i WHERE i.id=substr(c.source,14) AND c.source>='episode_item:' AND c.source<'episode_item;' AND i.rowid>?))
		ORDER BY c.created_at, c.id LIMIT ?`, guild, store.PersonPending, boundary, limit)
	if err != nil {
		return err
	}
	type pending struct{ id, text string }
	var list []pending
	var want []cardWant
	for rows.Next() {
		var p pending
		if rows.Scan(&p.id, &p.text) == nil {
			list = append(list, p)
			want = append(want, cardWant{text: p.text})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil || len(list) == 0 {
		return err
	}
	s.dropSensitive(ctx, want) // caches each verdict by text
	// The verdicts are read before the write transaction: kvGet may query the database, and inside the
	// writer that query would wait for the connection the transaction holds. Each change applies only
	// to a row still pending with the text that was classified.
	verdicts := map[string]bool{} // claim id → sensitive
	for _, p := range list {
		var sensitive bool
		if ok, err := s.kvGet(sensitiveKey(p.text), &sensitive); err == nil && ok {
			verdicts[p.id] = sensitive
		}
	}
	if len(verdicts) == 0 {
		return nil // nothing classified now: they wait
	}
	return db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		for _, p := range list {
			sensitive, ok := verdicts[p.id]
			if !ok {
				continue
			}
			if sensitive {
				var still bool
				if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM person_claim WHERE id=? AND state=? AND text=?)`, p.id, store.PersonPending, p.text).Scan(&still); err != nil {
					return err
				}
				if still {
					if err := deletePersonClaimTx(tx, p.id); err != nil {
						return err
					}
				}
				continue
			}
			if _, err := tx.Exec(`UPDATE person_claim SET state=? WHERE id=? AND state=? AND text=?`, store.PersonActive, p.id, store.PersonPending, p.text); err != nil {
				return err
			}
		}
		return nil
	})
}

// peopleClaimsPass is the person claim part of room memory's model-free pass: the one-time walk (and
// its safety net), the sweep of claims whose item went, then the no_sensitive classification of live
// claims.
func (s *Service) peopleClaimsPass(ctx context.Context, db *storage.Store) {
	if !s.Doc().Memory.PeopleOn() || !db.IsSQLite() || !roomMemoryTables(db) || !tableReady(db, "person_claim") {
		return
	}
	if _, err := s.derivePersonClaims(ctx, db); err != nil && !errors.Is(err, context.Canceled) {
		s.Log.Warn("memory: person claims not derived", "err", err)
	}
	if _, err := s.sweepDerivedClaims(ctx, db); err != nil && !errors.Is(err, context.Canceled) {
		s.Log.Warn("memory: person claims not swept", "err", err)
	}
	if err := s.classifyLivePending(ctx, db); err != nil && !errors.Is(err, context.Canceled) {
		s.Log.Warn("memory: person claims not classified", "err", err)
	}
}
