package store

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"masamong/internal/storage"
)

func migrateRoomState(db *storage.Store) error {
	// A global sequence survives deleting and re-importing a room, so no old
	// loaded snapshot can acquire the same identity as a replacement.
	if _, err := db.DB.Exec(`CREATE TABLE IF NOT EXISTS memory_revision_sequence(id INTEGER PRIMARY KEY, revision BIGINT NOT NULL)`); err != nil {
		return err
	}
	q := `INSERT INTO memory_revision_sequence(id,revision) VALUES(1,0) ON CONFLICT(id) DO NOTHING`
	if db.Backend == storage.BackendTiDB {
		q = `INSERT IGNORE INTO memory_revision_sequence(id,revision) VALUES(1,0)`
	}
	if _, err := db.DB.Exec(q); err != nil {
		return err
	}
	for _, c := range []struct{ name, def string }{{"revision", "BIGINT NOT NULL DEFAULT 0"}, {"evidence_revision", "BIGINT NOT NULL DEFAULT -1"}, {"active_recipe", "VARCHAR(96) NOT NULL DEFAULT ''"}, {"pending_sync", "BIGINT NOT NULL DEFAULT 1"}} {
		if db.HasColumn("memory_rooms", c.name) {
			continue
		}
		if _, err := db.DB.Exec(`ALTER TABLE memory_rooms ADD COLUMN ` + c.name + ` ` + c.def); err != nil {
			return fmt.Errorf("memory migration: room %s: %w", c.name, err)
		}
		if c.name == "evidence_revision" {
			if _, err := db.DB.Exec(`UPDATE memory_rooms SET evidence_revision=revision`); err != nil {
				return err
			}
		}
		if c.name == "active_recipe" {
			// Unambiguous legacy sets remain usable. Mixed legacy recipes stay
			// inactive until SyncLeaves rebuilds the configured recipe.
			if _, err := db.DB.Exec(`UPDATE memory_rooms SET active_recipe=(SELECT MIN(recipe) FROM memory_leaves l WHERE l.room=memory_rooms.room)
				WHERE (SELECT COUNT(DISTINCT recipe) FROM memory_leaves l WHERE l.room=memory_rooms.room)=1`); err != nil {
				return err
			}
		}
	}
	return nil
}

// WHY a Store method: failed optional checkpoint migrations and TiDB must keep
// every source writer working without querying an unavailable SQLite table.
func (s *Store) bumpRoomRevision(ctx context.Context, tx *sql.Tx, room string, invalidate bool) (int64, error) {
	if _, err := tx.ExecContext(ctx, `UPDATE memory_revision_sequence SET revision=revision+1 WHERE id=1`); err != nil {
		return 0, err
	}
	var revision int64
	if err := tx.QueryRowContext(ctx, `SELECT revision FROM memory_revision_sequence WHERE id=1`).Scan(&revision); err != nil {
		return 0, err
	}
	if s.leafSyncEnabled {
		// WHY compare to the previous room revision: a downgrade can advance the
		// room without recording an append. Never bridge that gap on a later
		// instrumented write, even if the old binary restored recipe/metadata.
		// This single PK update observes every new writer in the same transaction.
		if _, err := tx.ExecContext(ctx, `UPDATE memory_leaf_sync SET observed_revision=? WHERE room=?
			AND observed_revision=(SELECT revision FROM memory_rooms WHERE room=?)`, revision, room, room); err != nil {
			return 0, err
		}
	}
	// Captures can reuse all existing evidence while an open tail waits. Edits,
	// deletions and replacement rooms invalidate snapshots older than this guard.
	_, err := tx.ExecContext(ctx, `UPDATE memory_rooms SET revision=?,updated_at=?,pending_sync=1,
		evidence_revision=CASE WHEN ? OR evidence_revision<0 THEN ? ELSE evidence_revision END WHERE room=?`, revision, now(), invalidate, revision, room)
	return revision, err
}

// PendingSyncRooms is committed with source changes. A failed queue callback or a process restart
// therefore cannot lose the room; SyncLeaves clears its marker only for the revision it synchronized.
func (s *Store) PendingSyncRooms() ([]string, error) {
	rows, err := s.DB.Reader().Query(`SELECT room FROM memory_rooms WHERE pending_sync=1 ORDER BY room`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rooms []string
	for rows.Next() {
		var room string
		if err := rows.Scan(&room); err != nil {
			return nil, err
		}
		rooms = append(rooms, room)
	}
	return rooms, rows.Err()
}

func (s *Store) reviseRooms(ctx context.Context, tx *sql.Tx, revs []Revision) error {
	rooms := map[string]int64{}
	for _, r := range revs {
		rooms[r.Room] = 0
	}
	var names []string
	for room := range rooms {
		names = append(names, room)
	}
	sort.Strings(names)
	for _, room := range names {
		revision, err := s.bumpRoomRevision(ctx, tx, room, true)
		if err != nil {
			return err
		}
		rooms[room] = revision
	}
	for i := range revs {
		revs[i].RoomRevision = rooms[revs[i].Room]
	}
	return nil
}

// RoomRevisions reads current durable revisions in one query. A missing room is
// absent from the result, so a search holding a deleted room's index is rejected.
func (s *Store) RoomRevisions(rooms []string, recipe ...string) (map[string]int64, error) {
	return s.roomRevisions(rooms, "revision", recipe)
}

// RoomEvidenceRevisions returns the earliest snapshot safe for evidence: new
// captures leave earlier evidence valid, whereas an edit or deletion advances it.
func (s *Store) RoomEvidenceRevisions(rooms []string, recipe string) (map[string]int64, error) {
	return s.roomRevisions(rooms, "evidence_revision", []string{recipe})
}

func (s *Store) roomRevisions(rooms []string, column string, recipe []string) (map[string]int64, error) {
	out := map[string]int64{}
	if len(rooms) == 0 {
		return out, nil
	}
	q := `SELECT room,` + column + ` FROM memory_rooms WHERE room IN (` + strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",") + `)`
	args := toAny(rooms)
	if len(recipe) > 0 {
		q += ` AND active_recipe=?`
		args = append(args, recipe[0])
	}
	rows, err := s.DB.Reader().Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var room string
		var revision int64
		if err := rows.Scan(&room, &revision); err != nil {
			return nil, err
		}
		out[room] = revision
	}
	return out, rows.Err()
}

// activeLeaves excludes archived recipes and incomplete recipe builds.
const activeLeaves = `recipe=(SELECT active_recipe FROM memory_rooms WHERE room=memory_leaves.room)`
