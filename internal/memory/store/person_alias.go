package store

import (
	"database/sql"
	"strings"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/storage"
)

func savePersonAlias(tx *sql.Tx, room, account, label, at string) error {
	rest, ok := strings.CutPrefix(room, "discord:")
	guild, channel, server := strings.Cut(rest, ":")
	label = strings.TrimSpace(label)
	if !ok || !server || guild == "" || guild == "0" || channel == "" || account == "" || label == "" || label == "?" {
		return nil
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil // not a dated observation
	}
	at = t.UTC().Format("2006-01-02T15:04:05.000000000Z")
	_, err = tx.Exec(`INSERT INTO person_alias(guild_id,person_ref,label,first_seen,last_seen) VALUES(?,?,?,?,?)
		ON CONFLICT(guild_id,person_ref,label) DO UPDATE SET
		first_seen=min(first_seen,excluded.first_seen),last_seen=max(last_seen,excluded.last_seen)`,
		guild, PersonRef(account), label, at, at)
	return err
}

// Backfill only observation metadata, one committed rowid range at a time. New
// captures maintain the index themselves; -1 means no future startup scan.
func migratePersonAliases(db *storage.Store) error {
	if !db.IsSQLite() {
		return nil
	}
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS person_alias(guild_id TEXT NOT NULL,person_ref TEXT NOT NULL,label TEXT NOT NULL,
			first_seen TEXT NOT NULL,last_seen TEXT NOT NULL,PRIMARY KEY(guild_id,person_ref,label)) WITHOUT ROWID`,
		`CREATE TABLE IF NOT EXISTS person_alias_fill(id INTEGER PRIMARY KEY,cursor INTEGER NOT NULL)`,
	} {
		if _, err := db.DB.Exec(ddl); err != nil {
			return err
		}
	}
	const batch = 512
	done := false
	for !done {
		err := db.WithWriter(func(tx *sql.Tx) error {
			var cursor int64
			err := tx.QueryRow(`SELECT cursor FROM person_alias_fill WHERE id=1`).Scan(&cursor)
			if err != nil && err != sql.ErrNoRows {
				return err
			}
			if cursor < 0 {
				done = true
				return nil
			}
			var consent bool
			if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='consent_state')`).Scan(&consent); err != nil {
				return err
			}
			where := `platform='discord' AND record_kind=? AND actor_account<>'' AND actor_label<>'' AND deleted_at=''
				AND NOT EXISTS(SELECT 1 FROM privacy_tombstones p WHERE (p.kind='account' AND p.source_id=o.actor_account)
					OR (p.kind='message' AND p.source_id=o.native_message_id))`
			if consent {
				where += ` AND NOT EXISTS(SELECT 1 FROM consent_state c WHERE c.user_id=o.actor_account AND c.scope='memory' AND c.status='withdrawn')`
			}
			var end sql.NullInt64
			if err := tx.QueryRow(`SELECT max(rowid) FROM (SELECT rowid FROM memory_observations WHERE rowid>? ORDER BY rowid LIMIT ?)`, cursor, batch).Scan(&end); err != nil {
				return err
			}
			if !end.Valid {
				done, cursor = true, -1
			} else {
				rows, err := tx.Query(`SELECT source_room,actor_account,actor_label,parsed_time FROM memory_observations o
					WHERE rowid>? AND rowid<=? AND `+where, cursor, end.Int64, corpus.HumanText)
				if err != nil {
					return err
				}
				defer rows.Close()
				for rows.Next() {
					var room, id, label, at string
					if err := rows.Scan(&room, &id, &label, &at); err != nil {
						return err
					}
					if err := savePersonAlias(tx, room, id, label, at); err != nil {
						return err
					}
				}
				if err := rows.Err(); err != nil {
					return err
				}
				cursor = end.Int64
			}
			_, err = tx.Exec(`INSERT INTO person_alias_fill VALUES(1,?) ON CONFLICT(id) DO UPDATE SET cursor=excluded.cursor`, cursor)
			return err
		})
		if err != nil {
			return err
		}
	}
	return nil
}
