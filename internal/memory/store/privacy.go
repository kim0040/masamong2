package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
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
	return s.DB.WithWriter(func(tx *sql.Tx) error {
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
	})
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
