package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/storage"
)

// A checkpoint is trusted only after every leaf batch has committed. Keeping it
// in SQLite, rather than a process cache, makes restarts cheap without treating
// a partly written cut as a safe prefix. TiDB keeps the full-cut path: its text
// collation is not guaranteed to match corpus.OrderByRoom's byte ordering.
func migrateLeafSync(db *storage.Store) error {
	if db.Backend == storage.BackendTiDB {
		return nil
	}
	// WHY transactional: a failed optional migration must not leave triggers
	// referring to an unavailable checkpoint table on the full-cut fallback.
	return db.WithWriter(func(tx *sql.Tx) error {
		for _, q := range []string{
			`CREATE TABLE IF NOT EXISTS memory_leaf_sync (
			room TEXT PRIMARY KEY, valid INTEGER NOT NULL DEFAULT 0,
			signature TEXT NOT NULL DEFAULT '', timezone TEXT NOT NULL DEFAULT '',
			revision BIGINT NOT NULL DEFAULT 0, observed_revision BIGINT NOT NULL DEFAULT -1,
			last_time TEXT NOT NULL DEFAULT '', last_source TEXT NOT NULL DEFAULT '', last_id TEXT NOT NULL DEFAULT '',
			dirty_time TEXT, dirty_source TEXT NOT NULL DEFAULT '', dirty_id TEXT NOT NULL DEFAULT '', added BIGINT NOT NULL DEFAULT 0,
			restart_id TEXT NOT NULL DEFAULT '', tail_ids TEXT NOT NULL DEFAULT '[]',
			prefix_messages BIGINT NOT NULL DEFAULT 0, prefix_leaves BIGINT NOT NULL DEFAULT 0,
			tail_messages BIGINT NOT NULL DEFAULT 0)`,
			// WHY drop the unreleased dev index: room_live already seeks the small
			// tail; this wider duplicate slowed console counts and blocked startup.
			`DROP INDEX IF EXISTS idx_memory_obs_leaf_tail`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		// WHY -1 for old checkpoints: no revision chain was observed by that
		// version, so its first pass must establish trust with a full cut.
		rows, err := tx.Query(`PRAGMA table_info(memory_leaf_sync)`)
		if err != nil {
			return err
		}
		hasObserved := false
		for rows.Next() {
			var cid, notNull, pk int
			var name, typ string
			var def any
			if err := rows.Scan(&cid, &name, &typ, &notNull, &def, &pk); err != nil {
				rows.Close()
				return err
			}
			hasObserved = hasObserved || name == "observed_revision"
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if !hasObserved {
			if _, err := tx.Exec(`ALTER TABLE memory_leaf_sync ADD COLUMN observed_revision BIGINT NOT NULL DEFAULT -1`); err != nil {
				return err
			}
		}
		// WHY leaf guards as well as revisions: old SyncLeaves does not bump the
		// room revision. A recipe/WithPrefix round trip (or leaf replacement) must
		// invalidate the saved prefix even when room metadata ends up identical.
		// New syncs invalidate before their batches; vector/summary writes do not
		// change cut structure and deliberately do not fire the UPDATE guard.
		// WHY drop and create: CREATE TRIGGER IF NOT EXISTS would keep an older
		// column list forever once a later version changes it; recreating them on
		// every Open (a few statements) keeps each database on this binary's guards.
		for _, name := range []string{"memory_leaf_sync_insert", "memory_leaf_sync_delete", "memory_leaf_sync_update", "memory_leaf_sync_recipe"} {
			if _, err := tx.Exec(`DROP TRIGGER IF EXISTS ` + name); err != nil {
				return err
			}
		}
		for _, q := range []string{
			`CREATE TRIGGER memory_leaf_sync_insert AFTER INSERT ON memory_leaves BEGIN
			UPDATE memory_leaf_sync SET valid=0 WHERE room=NEW.room AND valid=1; END`,
			`CREATE TRIGGER memory_leaf_sync_delete AFTER DELETE ON memory_leaves BEGIN
			UPDATE memory_leaf_sync SET valid=0 WHERE room=OLD.room AND valid=1; END`,
			`CREATE TRIGGER memory_leaf_sync_update AFTER UPDATE OF room,recipe,first_observation_id,observation_ids,n,body,body_hash,speakers,end_time,closed ON memory_leaves BEGIN
			UPDATE memory_leaf_sync SET valid=0 WHERE room IN (OLD.room,NEW.room) AND valid=1; END`,
			`CREATE TRIGGER memory_leaf_sync_recipe AFTER UPDATE OF active_recipe ON memory_rooms WHEN OLD.active_recipe<>NEW.active_recipe BEGIN
			UPDATE memory_leaf_sync SET valid=0 WHERE room=NEW.room AND valid=1; END`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		return nil
	})
}

type leafMessageKey struct{ at, source, id string }

func messageKey(m corpus.Message) leafMessageKey {
	return leafMessageKey{m.ParsedTime, m.RoomOriginal, m.ObservationID}
}

func (a leafMessageKey) before(b leafMessageKey) bool {
	if a.at != b.at {
		return a.at < b.at
	}
	if a.source != b.source {
		return a.source < b.source
	}
	return a.id < b.id
}

type leafAppend struct {
	first leafMessageKey
	n     int
}

// Only successfully inserted rows count: duplicate exports must neither shift
// the prefix counts nor make an old timestamp look like a late arrival.
func (s *Store) recordLeafAppends(ctx context.Context, tx *sql.Tx, room string, a leafAppend) error {
	if !s.leafSyncEnabled {
		return nil
	}
	var at sql.NullString
	var old leafMessageKey
	err := tx.QueryRowContext(ctx, `SELECT dirty_time,dirty_source,dirty_id FROM memory_leaf_sync WHERE room=?`, room).Scan(&at, &old.source, &old.id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // no successful cut yet; the first sync must be full
	}
	if err != nil {
		return err
	}
	old.at = at.String
	if !at.Valid || a.first.before(old) {
		old = a.first
	}
	_, err = tx.ExecContext(ctx, `UPDATE memory_leaf_sync SET dirty_time=?,dirty_source=?,dirty_id=?,added=added+? WHERE room=?`, old.at, old.source, old.id, a.n, room)
	return err
}

type leafSyncPlan struct {
	incremental                  bool
	prefixMessages, prefixLeaves int
	tailIDs                      []string
	checkpointRevision           int64
}

// Recipe.Version deliberately omits rendering fields (WithPrefix) and some
// unused cut fields. Comparing the entire recipe avoids reusing a prefix when
// that historical identity string stays the same but the requested body changes.
func leafSyncSignature(r leaves.Recipe, snapshot leafRoomState) string {
	b, err := json.Marshal(struct {
		Recipe   leaves.Recipe
		Platform string
		Zone     string
	}{r, snapshot.platform, snapshot.naiveTimezone})
	if err != nil {
		return "" // unrepresentable recipes cannot certify an unchanged prefix
	}
	return hashText(string(b))
}

func (s *Store) leafSyncMessages(room string, r leaves.Recipe, _ *time.Location, snapshot leafRoomState) ([]corpus.Message, leafSyncPlan, error) {
	full := func() ([]corpus.Message, leafSyncPlan, error) {
		msgs, err := s.Messages([]string{room})
		return msgs, leafSyncPlan{}, err
	}
	if !s.leafSyncEnabled {
		return full()
	}
	var valid bool
	var signature, restartID, tailJSON string
	var revision, observedRevision, added, tailMessages int64
	var last, dirty leafMessageKey
	var dirtyTime sql.NullString
	var plan leafSyncPlan
	err := s.DB.Reader().QueryRow(`SELECT valid,signature,revision,observed_revision,last_time,last_source,last_id,
		dirty_time,dirty_source,dirty_id,added,restart_id,tail_ids,prefix_messages,prefix_leaves,tail_messages
		FROM memory_leaf_sync WHERE room=?`, room).Scan(&valid, &signature, &revision, &observedRevision,
		&last.at, &last.source, &last.id, &dirtyTime, &dirty.source, &dirty.id, &added, &restartID, &tailJSON,
		&plan.prefixMessages, &plan.prefixLeaves, &tailMessages)
	if errors.Is(err, sql.ErrNoRows) {
		return full()
	}
	if err != nil {
		return nil, plan, err
	}
	dirty.at = dirtyTime.String
	// Source APIs advance evidence_revision for edits/deletions; captures only
	// advance revision plus the append watermark. The former cannot certify an
	// unchanged prefix, even if a changed message is outside the saved tail.
	// WHY tolerate caller timezone changes: prefix leaves are closed by position,
	// and the final two leaves always recalculate idle closure using this pass's
	// timezone. Resync and indexing may therefore alternate zones safely.
	if !valid || restartID == "" || revision > snapshot.revision || observedRevision != snapshot.revision || snapshot.evidenceRevision > revision ||
		snapshot.recipe != r.Version() || signature == "" || signature != leafSyncSignature(r, snapshot) ||
		plan.prefixMessages < 0 || plan.prefixLeaves < 0 || tailMessages <= 0 ||
		(dirtyTime.Valid && !last.before(dirty)) ||
		json.Unmarshal([]byte(tailJSON), &plan.tailIDs) != nil || len(plan.tailIDs) == 0 || len(plan.tailIDs) > 2 {
		return full()
	}
	var restart leafMessageKey
	err = s.DB.Reader().QueryRow(`SELECT parsed_time,source_room,observation_id FROM memory_observations
		WHERE observation_id=? AND room=? AND deleted_at=''`, restartID, room).Scan(&restart.at, &restart.source, &restart.id)
	if errors.Is(err, sql.ErrNoRows) {
		return full()
	}
	if err != nil {
		return nil, plan, err
	}
	rows, err := s.DB.Reader().Query(leafTailQuery, room, restart.at, restart.at, restart.source, restart.id)
	if err != nil {
		return nil, plan, err
	}
	var msgs []corpus.Message
	for rows.Next() {
		var m corpus.Message
		if err := rows.Scan(&m.ObservationID, &m.Room, &m.RoomOriginal, &m.ActorLabel, &m.ActorAccount, &m.Content, &m.ParsedTime); err != nil {
			rows.Close()
			return nil, plan, err
		}
		m.Content = strings.TrimSpace(m.Content)
		msgs = append(msgs, m)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, plan, err
	}
	// A missing boundary or a regressed tail makes the checkpoint uncertain.
	if len(msgs) == 0 || messageKey(msgs[0]) != restart || messageKey(msgs[len(msgs)-1]).before(last) ||
		added < 0 || int64(len(msgs)) != tailMessages+added {
		return full()
	}
	plan.incremental, plan.checkpointRevision = true, revision
	return msgs, plan, nil
}

// WHY explicit time bound: room_live can seek (room,deleted_at,parsed_time),
// then SQL filters/orders only the small tail by the exact corpus tuple.
// Pin that existing index because SQLite can prefer room/parsed_time to satisfy
// partial ordering, which would also read deleted rows on a busy live channel.
const leafTailQuery = `SELECT observation_id,room,source_room,actor_label,actor_account,content,parsed_time
	FROM memory_observations INDEXED BY idx_memory_obs_room_live
	WHERE room=? AND deleted_at='' AND parsed_time>=? AND (parsed_time,source_room,observation_id)>=(?,?,?)
	ORDER BY parsed_time,source_room,observation_id`

func leafSyncExistingQuery(room, recipe string, plan leafSyncPlan) (string, []any) {
	if plan.incremental {
		// Leaf IDs already incorporate room and recipe. Query only those global
		// primary keys: adding room/recipe predicates can make SQLite choose its
		// room/recipe index and scan the entire room to return the same two rows.
		return `SELECT leaf_id,body_hash FROM memory_leaves WHERE leaf_id IN (` +
			strings.TrimSuffix(strings.Repeat("?,", len(plan.tailIDs)), ",") + `)`, toAny(plan.tailIDs)
	}
	return `SELECT leaf_id,body_hash FROM memory_leaves WHERE room=? AND recipe=?`, []any{room, recipe}
}

func (s *Store) beginLeafSync(tx *sql.Tx, room string, snapshot leafRoomState, plan leafSyncPlan) error {
	var revision int64
	var recipe string
	if err := tx.QueryRow(`SELECT revision,active_recipe FROM memory_rooms WHERE room=?`, room).Scan(&revision, &recipe); err != nil {
		return err
	}
	if revision != snapshot.revision || recipe != snapshot.recipe {
		return errors.New("memory source changed during leaf sync; retry")
	}
	if !s.leafSyncEnabled {
		return nil
	}
	if plan.incremental {
		res, err := tx.Exec(`UPDATE memory_leaf_sync SET valid=0 WHERE room=? AND valid=1 AND revision=?`, room, plan.checkpointRevision)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return errors.New("memory leaf checkpoint changed during sync; retry")
		}
		return nil
	}
	_, err := tx.Exec(`INSERT INTO memory_leaf_sync(room,valid) VALUES(?,0) ON CONFLICT(room) DO UPDATE SET valid=0`, room)
	return err
}

func (s *Store) finishLeafSync(tx *sql.Tx, room string, r leaves.Recipe, _ *time.Location, snapshot leafRoomState, plan leafSyncPlan, msgs []corpus.Message, cut []leaves.Leaf) error {
	if !s.leafSyncEnabled || len(cut) == 0 || leafSyncSignature(r, snapshot) == "" {
		return nil
	}
	// Restart at the penultimate leaf, even if it is closed: fixed windows can
	// overlap, a full final window can grow a following overlap window, and an
	// idle-closed/archived partial tail can still grow on the next append.
	start := max(0, len(cut)-2)
	var ids []string
	for _, l := range cut[start:] {
		ids = append(ids, LeafID(r.Version(), room, l.ObservationIDs[0]))
	}
	b, _ := json.Marshal(ids)
	last := messageKey(msgs[len(msgs)-1])
	_, err := tx.Exec(`UPDATE memory_leaf_sync SET valid=1,signature=?,revision=?,observed_revision=?,last_time=?,last_source=?,last_id=?,
		dirty_time=NULL,dirty_source='',dirty_id='',added=0,restart_id=?,tail_ids=?,prefix_messages=?,prefix_leaves=?,tail_messages=? WHERE room=?`,
		leafSyncSignature(r, snapshot), snapshot.revision, snapshot.revision, last.at, last.source, last.id,
		cut[start].ObservationIDs[0], string(b), plan.prefixMessages+cut[start].Start, plan.prefixLeaves+start,
		len(msgs)-cut[start].Start, room)
	return err
}
