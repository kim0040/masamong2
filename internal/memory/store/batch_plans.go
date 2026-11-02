package store

import (
	"database/sql"
	"errors"

	"masamong/internal/storage"
)

// A batch plan is the durable record of a room's provider batch jobs (memory preparation in batch mode,
// engine/batch.go): the submitted batch references and, for each request, the leaves or messages it
// covers by id and a short text hash. It holds no message text. One row per source room; the engine
// writes it after every submit, so a restart polls the batches it has instead of submitting them again.
//
// state is "waiting" while results are outstanding (the room's pending leaves are paid for already and
// the background index pass leaves the room alone) and "applied" once they are saved and only the live
// remainder is left. plan is the engine's JSON; the other columns let a list skip reading it.

func migrateBatchPlans(db *storage.Store) error {
	q := `CREATE TABLE IF NOT EXISTS memory_batch_plans (
		room TEXT PRIMARY KEY,
		state TEXT NOT NULL,
		items INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		plan TEXT NOT NULL)`
	if db.Backend == storage.BackendTiDB {
		q = `CREATE TABLE IF NOT EXISTS memory_batch_plans (
		room VARCHAR(160) NOT NULL PRIMARY KEY,
		state VARCHAR(16) NOT NULL,
		items INT NOT NULL DEFAULT 0,
		created_at VARCHAR(64) NOT NULL,
		updated_at VARCHAR(64) NOT NULL,
		plan LONGTEXT NOT NULL)`
	}
	_, err := db.DB.Exec(q)
	return err
}

// BatchPlanRow is a stored plan's summary, without the plan itself.
type BatchPlanRow struct {
	Room      string
	State     string
	Items     int
	CreatedAt string
}

// SaveBatchPlan stores (or replaces) the room's plan.
func (s *Store) SaveBatchPlan(room, state string, items int, createdAt string, plan []byte) error {
	q := `INSERT INTO memory_batch_plans(room, state, items, created_at, updated_at, plan) VALUES(?,?,?,?,?,?)
		ON CONFLICT(room) DO UPDATE SET state=excluded.state, items=excluded.items, created_at=excluded.created_at,
		updated_at=excluded.updated_at, plan=excluded.plan`
	if s.tidb() {
		q = `INSERT INTO memory_batch_plans(room, state, items, created_at, updated_at, plan) VALUES(?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE state=VALUES(state), items=VALUES(items), created_at=VALUES(created_at),
		updated_at=VALUES(updated_at), plan=VALUES(plan)`
	}
	return s.DB.WithWriter(func(tx *sql.Tx) error {
		_, err := tx.Exec(q, room, state, items, createdAt, now(), string(plan))
		return err
	})
}

// BatchPlan returns the room's stored plan; ok is false when it has none.
func (s *Store) BatchPlan(room string) ([]byte, bool, error) {
	var raw string
	err := s.DB.DB.QueryRow(`SELECT plan FROM memory_batch_plans WHERE room=?`, room).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return []byte(raw), true, nil
}

// DeleteBatchPlan removes the room's plan (no error when it has none).
func (s *Store) DeleteBatchPlan(room string) error {
	return s.DB.WithWriter(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM memory_batch_plans WHERE room=?`, room)
		return err
	})
}

// BatchPlans lists every stored plan, by room.
func (s *Store) BatchPlans() ([]BatchPlanRow, error) {
	rows, err := s.DB.DB.Query(`SELECT room, state, items, created_at FROM memory_batch_plans ORDER BY room`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BatchPlanRow
	for rows.Next() {
		var r BatchPlanRow
		if err := rows.Scan(&r.Room, &r.State, &r.Items, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
