package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"masamong/internal/storage"
)

// DeleteObservationsContext erases one bounded withdrawal batch, with one leaf invalidation pass.
func (s *Store) DeleteObservationsContext(ctx context.Context, ids []string, at string) ([]Revision, error) {
	var out []Revision
	if len(ids) == 0 {
		return out, nil
	}
	err := s.DB.WithWriterContext(ctx, func(tx *sql.Tx) error {
		rows, err := tx.QueryContext(ctx, `SELECT observation_id,room FROM memory_observations WHERE observation_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`) AND deleted_at=''`, toAny(ids)...)
		if err != nil {
			return err
		}
		for rows.Next() {
			var r Revision
			if err := rows.Scan(&r.ObservationID, &r.Room); err != nil {
				rows.Close()
				return err
			}
			r.Deleted = true
			out = append(out, r)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		for _, r := range out {
			if _, err := tx.ExecContext(ctx, `DELETE FROM chat_turns WHERE native_id IN (SELECT native_message_id FROM memory_observations WHERE observation_id=?)`, r.ObservationID); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `UPDATE memory_observations SET content='',deleted_at=? WHERE observation_id=?`, at, r.ObservationID); err != nil {
				return err
			}
		}
		if err := invalidateLeaves(tx, ids...); err != nil {
			return err
		}
		if err := s.reviseRooms(ctx, tx, out); err != nil {
			return err
		}
		return ctx.Err()
	})
	return out, err
}

// Privacy tombstones outlive cleanup jobs. They contain identifiers only and retention prunes them
// once no pending cleanup can need them. Consent remains authoritative after speaker tombstones expire.
func MigratePrivacy(db *storage.Store) error {
	if !db.IsSQLite() {
		return nil
	}
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS privacy_tombstones(kind TEXT NOT NULL, source_id TEXT NOT NULL, at TEXT NOT NULL, PRIMARY KEY(kind,source_id)) WITHOUT ROWID`,
	} {
		if _, err := db.DB.Exec(q); err != nil {
			return err
		}
	}
	return migrateEvidenceRefs(db)
}

// Summary caches from the old release were untyped blobs with no source references. Discard that
// cache once rather than retain untraceable copied text; persisted leaf vectors and summaries stay.
func (s *Store) migrateSummaryCache() error {
	if s.tidb() {
		return nil
	}
	if _, err := s.DB.DB.Exec(`CREATE TABLE IF NOT EXISTS memory_privacy_migration(id INTEGER PRIMARY KEY,done INTEGER NOT NULL)`); err != nil {
		return err
	}
	var done int
	if s.DB.DB.QueryRow(`SELECT done FROM memory_privacy_migration WHERE id=1`).Scan(&done) == nil && done == 1 {
		return nil
	}
	for {
		res, err := s.DB.DB.Exec(`DELETE FROM memory_embed_cache WHERE cache_key IN (SELECT cache_key FROM memory_embed_cache WHERE kind='' LIMIT 400)`)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if n == 0 {
			break
		}
	}
	_, err := s.DB.DB.Exec(`INSERT OR REPLACE INTO memory_privacy_migration VALUES(1,1)`)
	return err
}

// Summaries written while the summary model spent its whole output budget thinking hold the body's
// opening instead (postmortem 0005). Clear them once so the next pass summarizes those leaves again. The
// stand-in that was also cached under the body is not deleted by its text (a real summary elsewhere may
// read the same): the indexer ignores a cached stand-in and PutSummaryText replaces it.
func (s *Store) migrateFallbackSummaries() error {
	if s.tidb() {
		return nil
	}
	if _, err := s.DB.DB.Exec(`CREATE TABLE IF NOT EXISTS memory_privacy_migration(id INTEGER PRIMARY KEY,done INTEGER NOT NULL)`); err != nil {
		return err
	}
	var done int
	if s.DB.DB.QueryRow(`SELECT done FROM memory_privacy_migration WHERE id=2`).Scan(&done) == nil && done == 1 {
		return nil
	}
	const fallback = `summary<>'' AND summary=substr(body,1,180)`
	for _, q := range []string{
		`UPDATE memory_leaves SET summary='', summary_vec=NULL WHERE ` + fallback,
		`INSERT OR REPLACE INTO memory_privacy_migration VALUES(2,1)`,
	} {
		if _, err := s.DB.DB.Exec(q); err != nil {
			return err
		}
	}
	return nil
}

// expectedBody identifies the snapshot the model saw. A summary finishing after invalidation or
// re-cutting cannot overwrite the current leaf, even when its stable leaf ID was reused.
func leafSnapshotCurrent(tx *sql.Tx, id string, expected []string) (bool, error) {
	var body string
	err := tx.QueryRow(`SELECT body FROM memory_leaves WHERE leaf_id=?`, id).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if len(expected) > 0 {
		if body == "" || body != expected[0] {
			return false, nil
		}
		rows, err := tx.Query(`SELECT value FROM memory_leaves l,json_each(l.observation_ids) WHERE l.leaf_id=?`, id)
		if err != nil {
			return false, err
		}
		var ids []string
		for rows.Next() {
			var source string
			if err := rows.Scan(&source); err != nil {
				rows.Close()
				return false, err
			}
			ids = append(ids, source)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return false, err
		}
		for _, source := range ids {
			current, err := ObservationCurrent(tx, source)
			if err != nil || !current {
				return false, err
			}
		}
		return true, nil
	}
	blocked, err := SourceBlocked(tx, id)
	return body != "" && !blocked, err
}

func (s *Store) PutSummaryText(key, leaf, body, summary string) error {
	return s.DB.WithWriter(func(tx *sql.Tx) error { return putSummaryTextTx(tx, key, leaf, body, summary) })
}

func putSummaryTextTx(tx *sql.Tx, key, leaf, body, summary string) error {
	ok, err := leafSnapshotCurrent(tx, leaf, []string{body})
	if err != nil || !ok {
		return err
	}
	var ids string
	if err := tx.QueryRow(`SELECT observation_ids FROM memory_leaves WHERE leaf_id=?`, leaf).Scan(&ids); err != nil {
		return err
	}
	// a newer summary of the same body replaces the old one (an earlier release cached the body's opening
	// when the model answered nothing)
	if _, err := tx.Exec(`INSERT INTO memory_embed_cache(cache_key,vec,created_at,kind) VALUES(?,?,?,'summary_text')
		ON CONFLICT(cache_key) DO UPDATE SET vec=excluded.vec, created_at=excluded.created_at WHERE memory_embed_cache.kind='summary_text'`, key, []byte(summary), now()); err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT OR IGNORE INTO evidence_refs(source_id,holder_kind,holder_id) SELECT value,'cache',? FROM json_each(?) UNION SELECT ?,'cache',?`, key, ids, leaf, key)
	return err
}

func Tombstone(tx *sql.Tx, kind, id, at string) error {
	if id == "" {
		return nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, at); err == nil {
		at = parsed.UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	_, err := tx.Exec(`INSERT INTO privacy_tombstones(kind,source_id,at) VALUES(?,?,?) ON CONFLICT(kind,source_id) DO UPDATE SET at=MAX(privacy_tombstones.at,excluded.at)`, kind, id, at)
	return err
}

func currentMessageRevision(tx *sql.Tx, native, at string) (bool, error) {
	wanted, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return true, nil
	}
	var raw string
	err = tx.QueryRow(`SELECT at FROM privacy_tombstones WHERE kind='message' AND source_id=?`, native).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	latest, err := time.Parse(time.RFC3339Nano, raw)
	return err != nil || !latest.After(wanted), nil
}

// ObservationCurrent also closes the interval between a committed intent and source replay.
// An edited observation becomes usable again only after the corresponding revision has committed.
func ObservationCurrent(tx *sql.Tx, id string) (bool, error) {
	var deleted, account, native, edited string
	err := tx.QueryRow(`SELECT deleted_at,actor_account,COALESCE(native_message_id,''),edited_at FROM memory_observations WHERE observation_id=?`, id).Scan(&deleted, &account, &native, &edited)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil || deleted != "" {
		return false, err
	}
	blocked, err := MessageBlocked(tx, "", account)
	if err != nil || blocked {
		return false, err
	}
	var raw string
	err = tx.QueryRow(`SELECT at FROM privacy_tombstones WHERE kind='message' AND source_id=?`, native).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	latest, err := time.Parse(time.RFC3339Nano, raw)
	if err != nil {
		return false, err
	}
	applied, err := time.Parse(time.RFC3339Nano, edited)
	return err == nil && !applied.Before(latest), nil
}

func SourceBlocked(tx *sql.Tx, id string) (bool, error) {
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM privacy_tombstones WHERE source_id=? AND kind IN ('source','message')`, id).Scan(&n)
	return n > 0, err
}

// sourceChunk keeps each batched lookup well under SQLite's bound-variable limit.
const sourceChunk = 500

// SourcesBlocked is SourceBlocked for a whole list at once. A source list can be thousands of IDs (older
// rows are flat), and this runs while the one write connection is held: one statement per chunk, not
// per ID. A link to a chat turn that is no longer kept also blocks: the turn's own sources are only
// guaranteed to be followed from a link written while the turn was there (PruneChatHolders).
func SourcesBlocked(tx *sql.Tx, keys []string) (bool, error) {
	for lo := 0; lo < len(keys); lo += sourceChunk {
		chunk := keys[lo:min(len(keys), lo+sourceChunk)]
		var n int
		err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM privacy_tombstones WHERE kind IN ('source','message') AND source_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+`))`, toAny(chunk)...).Scan(&n)
		if err != nil || n > 0 {
			return n > 0, err
		}
	}
	return chatLinkGone(tx, keys)
}

// chatLinkGone reports whether any of the keys is a link to a chat turn that no longer exists.
func chatLinkGone(tx *sql.Tx, keys []string) (bool, error) {
	var ids []any
	for _, key := range keys {
		digits, ok := strings.CutPrefix(key, chatHolderPrefix)
		if !ok {
			continue
		}
		id, err := strconv.ParseInt(digits, 10, 64)
		if err != nil {
			return true, nil
		}
		ids = append(ids, id)
	}
	for lo := 0; lo < len(ids); lo += sourceChunk {
		chunk := ids[lo:min(len(ids), lo+sourceChunk)]
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")+`)`, chunk...).Scan(&n); err != nil {
			return false, err
		}
		if n != len(chunk) {
			return true, nil
		}
	}
	return false, nil
}

// chatHolderPrefix names a chat turn as a holder in evidence_refs, and as a source of a later turn.
const chatHolderPrefix = "chat:"

// ChatLink is what a turn built on a chat turn holds instead of a copy of that turn's sources.
func ChatLink(id int64) string { return chatHolderPrefix + strconv.FormatInt(id, 10) }

// ChatLineage closes the link graph: every chat turn built on any of the keys, directly or through other
// turns, however long ago those turns left the kept window (their index entries stay while anything
// links to them, see PruneChatHolders). Call it in a write transaction and tombstone the result there:
// a turn committed before that transaction is found here, and one committed after it sees the tombstone
// of the turn it links to and refuses itself.
func ChatLineage(tx *sql.Tx, keys []string) ([]string, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	raw, err := json.Marshal(keys)
	if err != nil {
		return nil, err
	}
	rows, err := tx.Query(`WITH RECURSIVE walk(name) AS (
			SELECT value FROM json_each(?)
			UNION
			SELECT h.holder_id FROM walk
				CROSS JOIN evidence_sources s ON s.source_id=walk.name
				CROSS JOIN evidence_links l ON l.source=s.id
				CROSS JOIN evidence_holders h ON h.id=l.holder
				WHERE h.holder_kind IN ('chat','chat_from')
		) SELECT name FROM walk WHERE name GLOB 'chat:*'`, string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// TombstoneSources tombstones many sources in one statement.
func TombstoneSources(tx *sql.Tx, ids []string, at string) error {
	if len(ids) == 0 {
		return nil
	}
	if parsed, err := time.Parse(time.RFC3339Nano, at); err == nil {
		at = parsed.UTC().Format("2006-01-02T15:04:05.000000000Z")
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return err
	}
	_, err = tx.Exec(`INSERT INTO privacy_tombstones(kind,source_id,at) SELECT 'source',value,? FROM json_each(?) WHERE true ON CONFLICT(kind,source_id) DO UPDATE SET at=MAX(privacy_tombstones.at,excluded.at)`, at, string(raw))
	return err
}

// PruneChatHolders removes the index entries of chat turns that are gone and that nothing still kept
// builds on. A turn past the kept window stays indexed while a later turn links to it (the promise is
// that deleting a source reaches every reply built on it, however old), so the entries of one chain go
// when its last reply goes. It computes the whole set in one read, so a chain thousands of turns deep is
// not unwound one layer per pass, and it is safe to act on later: nothing new links to a turn that is
// gone (SourcesBlocked).
func (s *Store) PruneChatHolders(ctx context.Context, batch int) (int64, error) {
	// the join order is fixed: walking from each kept turn is a few indexed lookups, scanning every chat
	// link for each of them is quadratic in the chain (postmortem 0004)
	rows, err := s.DB.Reader().QueryContext(ctx, `WITH RECURSIVE kept(name) AS (
			SELECT src.source_id FROM evidence_sources src
				CROSS JOIN evidence_links l ON l.source=src.id
				CROSS JOIN evidence_holders h ON h.id=l.holder
				WHERE src.source_id GLOB 'chat:*' AND (h.holder_kind NOT IN ('chat','chat_from')
					OR EXISTS (SELECT 1 FROM chat_turns t WHERE t.id=CAST(substr(h.holder_id,6) AS INTEGER)))
			UNION
			SELECT src.source_id FROM kept
				CROSS JOIN evidence_holders h ON h.holder_id=kept.name AND h.holder_kind IN ('chat','chat_from')
				CROSS JOIN evidence_links l ON l.holder=h.id
				CROSS JOIN evidence_sources src ON src.id=l.source
				WHERE src.source_id GLOB 'chat:*'
		) SELECT h.id FROM evidence_holders h
			WHERE h.holder_kind IN ('chat','chat_from')
				AND NOT EXISTS (SELECT 1 FROM chat_turns t WHERE t.id=CAST(substr(h.holder_id,6) AS INTEGER))
				AND h.holder_id NOT IN (SELECT name FROM kept)`)
	if err != nil {
		return 0, err
	}
	var holders []any
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return 0, err
		}
		holders = append(holders, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return 0, err
	}
	var total int64
	for lo := 0; lo < len(holders); lo += sourceChunk {
		chunk := holders[lo:min(len(holders), lo+sourceChunk)]
		// bounded by rows, not holders: a holder written by an earlier release has thousands of entries
		query := `DELETE FROM evidence_links WHERE (source,holder) IN (SELECT source,holder FROM evidence_links WHERE holder IN (` + strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",") + `) LIMIT ?)`
		for {
			res, err := s.DB.DB.ExecContext(ctx, query, append(append([]any{}, chunk...), batch)...)
			if err != nil {
				return total, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return total, err
			}
			total += n
			if n < int64(batch) {
				break
			}
			select {
			case <-ctx.Done():
				return total, ctx.Err()
			case <-time.After(50 * time.Millisecond): // others get the write connection in between
			}
		}
	}
	return total, nil
}

// uniqueSources joins source lists, in first-seen order, without repeats or empty IDs.
func uniqueSources(lists ...[]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, list := range lists {
		for _, id := range list {
			if id != "" && !seen[id] {
				seen[id] = true
				out = append(out, id)
			}
		}
	}
	return out
}

func MessageBlocked(tx *sql.Tx, native, account string) (bool, error) {
	var n int
	err := tx.QueryRow(`SELECT COUNT(*) FROM privacy_tombstones WHERE (kind='message' AND source_id=? AND source_id<>'') OR (kind='account' AND source_id=? AND source_id<>'')`, native, account).Scan(&n)
	return n > 0, err
}

// PersonRef is the same one-way account reference used by every holder of source text.
func PersonRef(account string) string {
	if account == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("masamong-person:" + account))
	return hex.EncodeToString(sum[:8])
}

func IndexSourceRefs(tx *sql.Tx, kind, id string, keys []string) error {
	keys = append([]string{}, keys...)
	raw, _ := json.Marshal(keys)
	rows, err := tx.Query(`SELECT value FROM memory_leaves l,json_each(l.observation_ids) WHERE l.leaf_id IN (SELECT value FROM json_each(?))`, string(raw))
	if err != nil {
		return err
	}
	for rows.Next() {
		var key string
		if err := rows.Scan(&key); err != nil {
			rows.Close()
			return err
		}
		keys = append(keys, key)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	raw, _ = json.Marshal(keys)
	_, err = tx.Exec(`INSERT OR IGNORE INTO evidence_refs(source_id,holder_kind,holder_id) SELECT value,?,? FROM json_each(?) WHERE value<>''`, kind, id, string(raw))
	return err
}
