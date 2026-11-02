package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
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
// generation is the plan's immutable id: a plan is inserted once (InsertBatchPlan) and every later write
// compares it (SaveBatchPlan, CommitBatchApply update only), so a run (or an engine) still holding an
// older plan of the room cannot overwrite, apply to, delete or recreate a newer or deleted one.
//
// state is "waiting" while results are outstanding, "draining" after a cancel until every batch has
// ended at the host, "submit_unknown" when a submission's outcome could not be settled (the background
// index pass leaves the room alone in all three), and "applied" once results are saved and only the live
// remainder is left. plan is the engine's JSON; the other columns let a list skip reading it.

// ErrBatchPlanExists is returned when a room already has a plan (of another generation).
var ErrBatchPlanExists = errors.New("memory: the room already has a batch plan")

// ErrBatchPlanStale is returned when the room's plan is not the generation written to (replaced).
var ErrBatchPlanStale = errors.New("memory: the room's batch plan is another generation")

// ErrBatchPlanMissing is returned when the room has no plan to update (deleted: discarded, finished).
var ErrBatchPlanMissing = errors.New("memory: the room's batch plan is no longer stored")

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
	return backfillBatchGenerations(db)
}

// backfillBatchGenerations gives a plan stored without a generation (written before plans had one, or
// left so by the release that added the column) one, in its column and its JSON together: the generation
// the JSON names, else a new one written to both. Without it every compare-and-set write of the plan
// fails, and the plan, which holds its room, could never finish, drain or be cancelled. A plan whose JSON
// cannot be read is left alone (the engine drops it when it loads it).
func backfillBatchGenerations(db *storage.Store) error {
	return db.WithWriter(func(tx *sql.Tx) error {
		rows, err := tx.Query(`SELECT room, plan FROM memory_batch_plans WHERE generation=''`)
		if err != nil {
			return err
		}
		type legacy struct{ room, plan string }
		var found []legacy
		for rows.Next() {
			var l legacy
			if err := rows.Scan(&l.room, &l.plan); err != nil {
				rows.Close()
				return err
			}
			found = append(found, l)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return err
		}
		for _, l := range found {
			var doc map[string]json.RawMessage
			if json.Unmarshal([]byte(l.plan), &doc) != nil || doc == nil {
				continue
			}
			var gen string
			_ = json.Unmarshal(doc["generation"], &gen)
			raw := l.plan
			if gen == "" {
				gen = newPlanGeneration()
				doc["generation"], _ = json.Marshal(gen)
				b, err := json.Marshal(doc)
				if err != nil {
					return err
				}
				raw = string(b)
			}
			if _, err := tx.Exec(`UPDATE memory_batch_plans SET generation=?, plan=? WHERE room=? AND generation=''`, gen, raw, l.room); err != nil {
				return err
			}
		}
		return nil
	})
}

// newPlanGeneration is a generation in the engine's form (32 hex characters).
func newPlanGeneration() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return hex.EncodeToString([]byte(time.Now().UTC().Format("20060102150405.000000")))[:32]
	}
	return hex.EncodeToString(b[:])
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

// BatchPlanRecord is a plan's stored form: the engine's JSON and the columns a list reads.
type BatchPlanRecord struct {
	State     string
	Items     int
	CreatedAt string
	Plan      []byte
}

// InsertBatchPlan stores a new plan of generation gen for a room that has none (ErrBatchPlanExists
// otherwise). Only a new preparation inserts; every later write updates (SaveBatchPlan), so an old run
// cannot recreate a plan that was deleted meanwhile.
func (s *Store) InsertBatchPlan(room, gen string, rec BatchPlanRecord) error {
	return s.DB.WithWriter(func(tx *sql.Tx) error {
		var stored string
		err := tx.QueryRow(`SELECT generation FROM memory_batch_plans WHERE room=?`, room).Scan(&stored)
		switch {
		case err == nil:
			return ErrBatchPlanExists
		case !errors.Is(err, sql.ErrNoRows):
			return err
		}
		_, err = tx.Exec(`INSERT INTO memory_batch_plans(room, generation, state, items, created_at, updated_at, plan) VALUES(?,?,?,?,?,?,?)`,
			room, gen, rec.State, rec.Items, rec.CreatedAt, now(), string(rec.Plan))
		return err
	})
}

// SaveBatchPlan replaces the room's plan of generation gen. A room whose plan is another generation is
// left alone (ErrBatchPlanStale), and a room without a plan gets none (ErrBatchPlanMissing).
func (s *Store) SaveBatchPlan(room, gen string, rec BatchPlanRecord) error {
	return s.DB.WithWriter(func(tx *sql.Tx) error {
		if err := planGenerationTx(tx, room, gen); err != nil {
			return err
		}
		return updateBatchPlanTx(tx, room, gen, rec)
	})
}

// BatchWrites are the results of one applied batch, written with its checkpoint (CommitBatchApply).
type BatchWrites struct {
	Leaves    []LeafVectorWrite
	Summaries []SummaryWrite
	Messages  []MessageVector
}

// LeafVectorWrite is a leaf vector for the space, kept only if the leaf still reads Body.
type LeafVectorWrite struct {
	ID, Space string
	Vec       []byte
	Body      string
}

// SummaryWrite is a leaf's summary (without its vector) and its summary-text cache entry, kept only if the
// leaf still reads Body.
type SummaryWrite struct {
	ID, Body, Summary, CacheKey string
}

// BatchWritten counts what CommitBatchApply stored; a result whose leaf or message changed meanwhile (or
// is gone) is not stored.
type BatchWritten struct {
	Leaves, Summaries, Messages int
}

// CommitBatchApply writes an applied batch's results, replaces the room's plan of generation gen (it must
// exist) with the record plan builds from what was written, and adds the spend entries, in one
// transaction: the results, the saved counts in the plan, its checkpoint and its cost are stored together
// or not at all. A crash before the commit leaves nothing of the apply, so applying the batch again saves
// and counts it once; a plan of another generation, or none, gets nothing (ErrBatchPlanStale,
// ErrBatchPlanMissing). An error plan returns rolls everything back.
func (s *Store) CommitBatchApply(room, gen string, w BatchWrites, spends []BatchSpend, plan func(BatchWritten) (BatchPlanRecord, error)) error {
	day := time.Now().UTC().Format("2006-01-02")
	q := `INSERT INTO memory_spend(day, kind, usd, calls) VALUES(?,?,?,?) ON CONFLICT(day, kind) DO UPDATE SET usd=memory_spend.usd+excluded.usd, calls=memory_spend.calls+excluded.calls`
	if s.tidb() {
		q = `INSERT INTO memory_spend(day, kind, usd, calls) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE usd=usd+VALUES(usd), calls=calls+VALUES(calls)`
	}
	return s.DB.WithWriter(func(tx *sql.Tx) error {
		if err := planGenerationTx(tx, room, gen); err != nil {
			return err
		}
		var got BatchWritten
		for _, l := range w.Leaves {
			ok, err := s.saveLeafVectorTx(tx, l.ID, l.Space, l.Vec, []string{l.Body})
			if err != nil {
				return err
			}
			if ok {
				got.Leaves++
			}
		}
		for _, sw := range w.Summaries {
			if err := putSummaryTextTx(tx, sw.CacheKey, sw.ID, sw.Body, sw.Summary); err != nil {
				return err
			}
			ok, err := saveSummaryTx(tx, sw.ID, sw.Summary, nil, []string{sw.Body})
			if err != nil {
				return err
			}
			if ok {
				got.Summaries++
			}
		}
		n, err := s.saveMessageVectorsTx(tx, w.Messages)
		if err != nil {
			return err
		}
		got.Messages = n
		rec, err := plan(got)
		if err != nil {
			return err
		}
		if err := updateBatchPlanTx(tx, room, gen, rec); err != nil {
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

// planGenerationTx checks that the room's stored plan is generation gen.
func planGenerationTx(tx *sql.Tx, room, gen string) error {
	var stored string
	err := tx.QueryRow(`SELECT generation FROM memory_batch_plans WHERE room=?`, room).Scan(&stored)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return ErrBatchPlanMissing
	case err != nil:
		return err
	case stored != gen:
		return ErrBatchPlanStale
	}
	return nil
}

func updateBatchPlanTx(tx *sql.Tx, room, gen string, rec BatchPlanRecord) error {
	_, err := tx.Exec(`UPDATE memory_batch_plans SET state=?, items=?, created_at=?, updated_at=?, plan=? WHERE room=? AND generation=?`,
		rec.State, rec.Items, rec.CreatedAt, now(), string(rec.Plan), room, gen)
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
