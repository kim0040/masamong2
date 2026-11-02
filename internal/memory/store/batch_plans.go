package store

import (
	"database/sql"
	"errors"
	"time"

	"masamong/internal/storage"
)

// A batch plan is the durable record of a room's provider batch jobs (memory preparation in batch mode,
// engine/batch.go): the submitted batch references and, for each request, the leaves or messages it
// covers by id and a short text hash. It holds no message text. One row per source room; the engine
// writes it before and after every submit, so a restart polls the batches it has instead of submitting
// them again.
//
// generation is the plan's immutable id: every write after the first compares it, so a run (or an engine)
// still holding an older plan of the room cannot overwrite, apply to or delete a newer one.
//
// state is "waiting" while results are outstanding, "draining" after a cancel until every batch has
// ended at the host, "submit_unknown" when a submission's outcome could not be settled (the background
// index pass leaves the room alone in all three), and "applied" once results are saved and only the live
// remainder is left. plan is the engine's JSON; the other columns let a list skip reading it.

// ErrBatchPlanExists is returned when a room already has a plan (of another generation).
var ErrBatchPlanExists = errors.New("memory: the room already has a batch plan")

// ErrBatchPlanStale is returned when the room's plan is not the generation written to (replaced or deleted).
var ErrBatchPlanStale = errors.New("memory: the room's batch plan is another generation")

func migrateBatchPlans(db *storage.Store) error {
	q := `CREATE TABLE IF NOT EXISTS memory_batch_plans (
		room TEXT PRIMARY KEY,
		generation TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL,
		items INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL,
		plan TEXT NOT NULL)`
	if db.Backend == storage.BackendTiDB {
		q = `CREATE TABLE IF NOT EXISTS memory_batch_plans (
		room VARCHAR(160) NOT NULL PRIMARY KEY,
		generation VARCHAR(64) NOT NULL DEFAULT '',
		state VARCHAR(16) NOT NULL,
		items INT NOT NULL DEFAULT 0,
		created_at VARCHAR(64) NOT NULL,
		updated_at VARCHAR(64) NOT NULL,
		plan LONGTEXT NOT NULL)`
	}
	if _, err := db.DB.Exec(q); err != nil {
		return err
	}
	if !db.HasColumn("memory_batch_plans", "generation") {
		def := `TEXT NOT NULL DEFAULT ''`
		if db.Backend == storage.BackendTiDB {
			def = `VARCHAR(64) NOT NULL DEFAULT ''`
		}
		if _, err := db.DB.Exec(`ALTER TABLE memory_batch_plans ADD COLUMN generation ` + def); err != nil {
			return err
		}
	}
	return nil
}

// BatchPlanRow is a stored plan's summary, without the plan itself.
type BatchPlanRow struct {
	Room       string
	Generation string
	State      string
	Items      int
	CreatedAt  string
}

// BatchSpend is one spend ledger entry written with a plan checkpoint (AddSpend's kind, usd and calls).
type BatchSpend struct {
	Kind  string
	USD   float64
	Calls int
}

// SaveBatchPlan stores the room's plan of generation gen: it creates the row when the room has none and
// replaces it when the stored plan is the same generation. A plan of another generation is left alone
// (ErrBatchPlanStale).
func (s *Store) SaveBatchPlan(room, gen, state string, items int, createdAt string, plan []byte) error {
	return s.DB.WithWriter(func(tx *sql.Tx) error {
		return saveBatchPlanTx(tx, room, gen, state, items, createdAt, plan, true)
	})
}

// CommitBatchApply replaces the room's plan of generation gen (it must exist) and adds the spend entries
// in one transaction: an applied batch's checkpoint and its cost are written together or not at all, so
// a crash between them neither loses the cost nor counts it twice when the batch is applied again.
func (s *Store) CommitBatchApply(room, gen, state string, items int, createdAt string, plan []byte, spends []BatchSpend) error {
	day := time.Now().UTC().Format("2006-01-02")
	q := `INSERT INTO memory_spend(day, kind, usd, calls) VALUES(?,?,?,?) ON CONFLICT(day, kind) DO UPDATE SET usd=memory_spend.usd+excluded.usd, calls=memory_spend.calls+excluded.calls`
	if s.tidb() {
		q = `INSERT INTO memory_spend(day, kind, usd, calls) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE usd=usd+VALUES(usd), calls=calls+VALUES(calls)`
	}
	return s.DB.WithWriter(func(tx *sql.Tx) error {
		if err := saveBatchPlanTx(tx, room, gen, state, items, createdAt, plan, false); err != nil {
			return err
		}
		for _, sp := range spends {
			if sp.USD == 0 && sp.Calls == 0 {
				continue
			}
			if _, err := tx.Exec(q, day, sp.Kind, sp.USD, sp.Calls); err != nil {
				return err
			}
		}
		return nil
	})
}

func saveBatchPlanTx(tx *sql.Tx, room, gen, state string, items int, createdAt string, plan []byte, create bool) error {
	var stored string
	err := tx.QueryRow(`SELECT generation FROM memory_batch_plans WHERE room=?`, room).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		if !create {
			return ErrBatchPlanStale
		}
		_, err = tx.Exec(`INSERT INTO memory_batch_plans(room, generation, state, items, created_at, updated_at, plan) VALUES(?,?,?,?,?,?,?)`,
			room, gen, state, items, createdAt, now(), string(plan))
		return err
	case err != nil:
		return err
	case stored != gen:
		if create {
			return ErrBatchPlanExists
		}
		return ErrBatchPlanStale
	}
	_, err = tx.Exec(`UPDATE memory_batch_plans SET state=?, items=?, created_at=?, updated_at=?, plan=? WHERE room=? AND generation=?`,
		state, items, createdAt, now(), string(plan), room, gen)
	return err
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

// DeleteBatchPlan removes the room's plan whatever its generation (no error when it has none): for a plan
// nothing can read, and for the room's own deletion.
func (s *Store) DeleteBatchPlan(room string) error {
	return s.DB.WithWriter(func(tx *sql.Tx) error {
		_, err := tx.Exec(`DELETE FROM memory_batch_plans WHERE room=?`, room)
		return err
	})
}

// DeleteBatchPlanGeneration removes the room's plan only when it is generation gen; deleted is false when
// the room has no plan or a plan of another generation (which stays).
func (s *Store) DeleteBatchPlanGeneration(room, gen string) (deleted bool, err error) {
	err = s.DB.WithWriter(func(tx *sql.Tx) error {
		res, err := tx.Exec(`DELETE FROM memory_batch_plans WHERE room=? AND generation=?`, room, gen)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		deleted = n > 0
		return nil
	})
	return deleted, err
}

// BatchPlans lists every stored plan, by room.
func (s *Store) BatchPlans() ([]BatchPlanRow, error) {
	rows, err := s.DB.DB.Query(`SELECT room, generation, state, items, created_at FROM memory_batch_plans ORDER BY room`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BatchPlanRow
	for rows.Next() {
		var r BatchPlanRow
		if err := rows.Scan(&r.Room, &r.Generation, &r.State, &r.Items, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
