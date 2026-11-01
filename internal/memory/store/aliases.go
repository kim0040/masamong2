package store

import (
	"context"
	"database/sql"
	"encoding/json"

	"masamong/internal/memory/corpus"
)

func (s *Store) migrateAliases() error {
	if s.tidb() {
		return nil
	}
	for _, q := range []string{
		`CREATE TABLE IF NOT EXISTS memory_observation_aliases(alias_id TEXT PRIMARY KEY, observation_id TEXT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_memory_alias_observation ON memory_observation_aliases(observation_id)`,
		`CREATE TABLE IF NOT EXISTS memory_identity_migration(id INTEGER PRIMARY KEY, done INTEGER NOT NULL)`,
	} {
		if _, err := s.DB.DB.Exec(q); err != nil {
			return err
		}
	}
	var done int
	if s.DB.DB.QueryRow(`SELECT done FROM memory_identity_migration WHERE id=1`).Scan(&done) == nil && done == 1 {
		return nil
	}
	// Existing IDs remain the canonical IDs: leaves and reviews keep their references. Rowid preserves
	// the original file order for identical timestamps, including interleaved identical messages.
	rows, err := s.DB.Reader().Query(`SELECT observation_id, source_room, parsed_time, actor_label, content FROM memory_observations WHERE platform='kakao' ORDER BY source_room, parsed_time, rowid`)
	if err != nil {
		return err
	}
	defer rows.Close()
	counts := corpus.NewKakaoOccurrences()
	type alias struct{ key, id string }
	var batch []alias
	flush := func() error {
		return s.DB.WithWriter(func(tx *sql.Tx) error {
			for _, a := range batch {
				if _, err := tx.Exec(`INSERT OR IGNORE INTO memory_observation_aliases VALUES(?,?)`, a.key, a.id); err != nil {
					return err
				}
			}
			return nil
		})
	}
	for rows.Next() {
		var id, room, at, speaker, text string
		if err := rows.Scan(&id, &room, &at, &speaker, &text); err != nil {
			return err
		}
		n, err := counts.Next(room, at, speaker, text)
		if err != nil {
			return err
		}
		batch = append(batch, alias{corpus.KakaoRowIdentity(room, at, speaker, text, n), id})
		if len(batch) == 300 {
			if err := flush(); err != nil {
				return err
			}
			batch = nil
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if err := flush(); err != nil {
		return err
	}
	_, err = s.DB.DB.Exec(`INSERT OR REPLACE INTO memory_identity_migration VALUES(1,1)`)
	return err
}

// Import a whole batch's aliases in one indexed read. The in-memory map also sees aliases queued
// earlier in the same transaction; lookup and insertion add no per-row cancellation or DB overhead.
func loadObservationAliases(ctx context.Context, tx *sql.Tx, obs []corpus.Observation) (map[string]string, error) {
	keys := []string{}
	for _, o := range obs {
		if o.Platform == "kakao" {
			keys = append(keys, o.ObservationID, o.LegacyID, o.IdentityID)
		}
	}
	out := map[string]string{}
	if len(keys) == 0 {
		return out, nil
	}
	raw, _ := json.Marshal(keys)
	rows, err := tx.QueryContext(ctx, `SELECT observation_id,observation_id FROM memory_observations WHERE observation_id IN (SELECT value FROM json_each(?)) UNION ALL SELECT alias_id,observation_id FROM memory_observation_aliases WHERE alias_id IN (SELECT value FROM json_each(?))`, string(raw), string(raw))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var key, id string
		if err := rows.Scan(&key, &id); err != nil {
			return nil, err
		}
		out[key] = id
	}
	return out, rows.Err()
}

type observationAlias struct {
	Key string `json:"key"`
	ID  string `json:"id"`
}

func observationAliases(o corpus.Observation, id string, known map[string]string) []observationAlias {
	var out []observationAlias
	for _, key := range []string{o.ObservationID, o.LegacyID, o.IdentityID} {
		if key != "" && known[key] == "" {
			known[key] = id
			out = append(out, observationAlias{key, id})
		}
	}
	return out
}

func saveObservationAliases(ctx context.Context, tx *sql.Tx, aliases []observationAlias) error {
	if len(aliases) == 0 {
		return nil
	}
	raw, err := json.Marshal(aliases)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO memory_observation_aliases(alias_id,observation_id) SELECT json_extract(value,'$.key'),json_extract(value,'$.id') FROM json_each(?)`, string(raw))
	return err
}
