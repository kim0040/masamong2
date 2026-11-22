package application

import (
	"database/sql"
	"encoding/json"
	"errors"
	"time"

	"masamong/internal/storage"
)

// kvUpsertSQL writes one admin_state value (kvSet, kvUpdate).
func kvUpsertSQL(backend string) string {
	if backend == storage.BackendTiDB {
		return `INSERT INTO admin_state(k, v, updated_at) VALUES(?,?,?) ON DUPLICATE KEY UPDATE v=VALUES(v), updated_at=VALUES(updated_at)`
	}
	return `INSERT INTO admin_state(k, v, updated_at) VALUES(?,?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v, updated_at=excluded.updated_at`
}

// kvUpdate reads one admin_state value, changes it and writes it back in one write transaction. The
// store's writer orders the transactions, so changes from different callers are all kept without a
// process lock held while the store works (code standard R20). A missing or unreadable value reaches
// change as the zero value.
func kvUpdate[T any](s *Service, key string, change func(T) T) error {
	db, err := s.adminDB()
	if err != nil {
		return err
	}
	sel := `SELECT v FROM admin_state WHERE k=?`
	if db.Backend == storage.BackendTiDB {
		sel += ` FOR UPDATE` // TiDB runs writers side by side: the row lock orders them
	}
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	return db.WithWriter(func(tx *sql.Tx) error {
		var v T
		var raw string
		switch err := tx.QueryRow(sel, key).Scan(&raw); {
		case errors.Is(err, sql.ErrNoRows):
		case err != nil:
			return err
		default:
			if json.Unmarshal([]byte(raw), &v) != nil {
				var zero T
				v = zero
			}
		}
		next, err := json.Marshal(change(v))
		if err != nil {
			return err
		}
		if _, err := tx.Exec(kvUpsertSQL(db.Backend), key, string(next), now); err != nil {
			return err
		}
		// cached inside the transaction, so the cache ends with the value of the last transaction (a
		// commit that fails after this leaves it wrong for at most kvFresh)
		s.kv.put(key, kvEntry{raw: string(next), ok: true})
		return nil
	})
}
