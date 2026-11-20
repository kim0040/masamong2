package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"sync"
	"time"
	"unicode"

	"masamong/internal/memory/corpus"
	"masamong/internal/storage"
)

func aliasValues(observation, room, account, label, at string) []string {
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
	return []string{observation, guild, PersonRef(account), label, room, at, strings.ToLower(label)}
}

// Startup creates metadata only. A fresh, empty store needs no historical fill.
func migratePersonAliases(db *storage.Store) error {
	if !db.IsSQLite() {
		return nil
	}
	return db.WithWriter(func(tx *sql.Tx) error {
		var old bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM pragma_table_info('person_alias')) AND NOT EXISTS(SELECT 1 FROM pragma_table_info('person_alias') WHERE name='folded')`).Scan(&old); err != nil {
			return err
		}
		if old { // A guild-only summary cannot prove that a label is public.
			if _, err := tx.Exec(`DROP TABLE IF EXISTS person_alias_source; DROP TABLE person_alias; DROP TABLE IF EXISTS person_alias_fill`); err != nil {
				return err
			}
		}
		for _, ddl := range []string{
			`CREATE TABLE IF NOT EXISTS person_alias(guild_id TEXT NOT NULL,person_ref TEXT NOT NULL,label TEXT NOT NULL,
			room TEXT NOT NULL,first_seen TEXT NOT NULL,last_seen TEXT NOT NULL,last_source TEXT NOT NULL,folded TEXT NOT NULL,PRIMARY KEY(guild_id,person_ref,label,room)) WITHOUT ROWID`,
			`CREATE INDEX IF NOT EXISTS idx_person_alias_name ON person_alias(guild_id,folded)`,
			`CREATE TABLE IF NOT EXISTS person_alias_source(observation TEXT PRIMARY KEY,guild_id TEXT NOT NULL,person_ref TEXT NOT NULL,
			label TEXT NOT NULL,room TEXT NOT NULL,at TEXT NOT NULL,folded TEXT NOT NULL) WITHOUT ROWID`,
			`CREATE INDEX IF NOT EXISTS idx_person_alias_source ON person_alias_source(guild_id,person_ref,label,room,at,observation)`,
			`CREATE TRIGGER IF NOT EXISTS person_alias_seen AFTER INSERT ON person_alias_source BEGIN
			INSERT INTO person_alias(guild_id,person_ref,label,room,first_seen,last_seen,last_source,folded)
			VALUES(new.guild_id,new.person_ref,new.label,new.room,new.at,new.at,new.observation,new.folded)
			ON CONFLICT(guild_id,person_ref,label,room) DO UPDATE SET
			last_source=CASE WHEN (new.at,new.observation)>(last_seen,last_source) THEN new.observation ELSE last_source END,
			first_seen=min(first_seen,new.at),last_seen=max(last_seen,new.at); END`,
			// Recompute only the affected label, seeking its two endpoints (not scanning the person's history).
			`CREATE TRIGGER IF NOT EXISTS person_alias_lost AFTER DELETE ON person_alias_source BEGIN
			DELETE FROM person_alias WHERE (guild_id,person_ref,label,room)=(old.guild_id,old.person_ref,old.label,old.room)
			AND NOT EXISTS(SELECT 1 FROM person_alias_source WHERE (guild_id,person_ref,label,room)=(old.guild_id,old.person_ref,old.label,old.room));
			UPDATE person_alias SET first_seen=(SELECT at FROM person_alias_source WHERE
			(guild_id,person_ref,label,room)=(old.guild_id,old.person_ref,old.label,old.room) ORDER BY at LIMIT 1),
			(last_seen,last_source)=(SELECT at,observation FROM person_alias_source WHERE
			(guild_id,person_ref,label,room)=(old.guild_id,old.person_ref,old.label,old.room) ORDER BY at DESC,observation DESC LIMIT 1)
			WHERE (guild_id,person_ref,label,room)=(old.guild_id,old.person_ref,old.label,old.room); END`,
			`CREATE TRIGGER IF NOT EXISTS person_alias_edit AFTER UPDATE OF deleted_at,edited_at,actor_label,actor_account,source_room,content ON memory_observations
			BEGIN DELETE FROM person_alias_source WHERE observation=old.observation_id; END`,
			`CREATE TRIGGER IF NOT EXISTS person_alias_delete AFTER DELETE ON memory_observations
			BEGIN DELETE FROM person_alias_source WHERE observation=old.observation_id; END`,
			`CREATE TABLE IF NOT EXISTS person_alias_fill(id INTEGER PRIMARY KEY,cursor INTEGER NOT NULL)`,
		} {
			if _, err := tx.Exec(ddl); err != nil {
				return err
			}
		}
		var stored bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM memory_observations)`).Scan(&stored); err != nil {
			return err
		}
		if !stored { // instant on an empty table; a stored history gets it from the background fill
			if _, err := tx.Exec(personNameIndexSQL); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`INSERT OR IGNORE INTO person_alias_fill SELECT 1,CASE WHEN EXISTS(SELECT 1 FROM memory_observations) THEN 0 ELSE -1 END`)
		return err
	})
}

// personNameExpr is a stored label as aliasValues folds it: the characters
// strings.TrimSpace removes (unicode.IsSpace) are trimmed, ASCII case is
// compared NOCASE and other case variants are enumerated by nameVariants.
const personNameExpr = `trim(actor_label,char(9,10,11,12,13,32,133,160,5760,8192,8193,8194,8195,8196,8197,8198,8199,8200,8201,8202,8232,8233,8239,8287,12288))`

// The name check reads every captured label, so it also sees accounts the
// alias index leaves out. Additive; built off the startup path for a stored history.
const personNameIndexSQL = `CREATE INDEX IF NOT EXISTS idx_memory_obs_person_name ON memory_observations(` + personNameExpr + ` COLLATE NOCASE,actor_account,source_room)`

// lowerFrom maps a lowercase rune to the other runes unicode.ToLower maps to it.
var lowerFrom = sync.OnceValue(func() map[rune][]rune {
	m := map[rune][]rune{}
	for _, cr := range unicode.CaseRanges {
		for r := rune(cr.Lo); r <= rune(cr.Hi); r++ {
			if l := unicode.ToLower(r); l != r {
				m[l] = append(m[l], r)
			}
		}
	}
	return m
})

const maxNameVariants = 16

// nameVariants are the trimmed raw labels, up to ASCII case, that fold to key
// (strings.ToLower maps rune by rune): "kim" also stands for "\u212aim" and
// "émile" for "Émile". false: too many to check.
func nameVariants(key string) ([]string, bool) {
	out := []string{""}
	for _, r := range key {
		var choices []rune
		if unicode.ToLower(r) == r {
			choices = append(choices, r)
		}
		for _, u := range lowerFrom()[r] {
			if u >= 0x80 { // an ASCII capital is the same label under NOCASE
				choices = append(choices, u)
			}
		}
		if len(out)*len(choices) > maxNameVariants {
			return nil, false
		}
		next := make([]string, 0, len(out)*len(choices))
		for _, prefix := range out {
			for _, c := range choices {
				next = append(next, prefix+string(c))
			}
		}
		out = next
	}
	return out, true
}

const maxNameAccounts = 16

// OtherPersonNamed reports whether an account other than account has a live
// Discord message in rooms (a JSON object keyed by room) under a label that
// folds to one of labels. It reads the captured messages, not person_alias, so
// accounts the index hides (profile withdrawal) or has not caught up with (a
// pending or failed alias write) still make a name ambiguous. Any input it
// cannot check completely, including a missing index, counts as taken.
func OtherPersonNamed(tx *sql.Tx, labels []string, account, rooms string) (bool, error) {
	var values []string
	for _, label := range labels {
		v, ok := nameVariants(label)
		if !ok {
			return true, nil
		}
		values = append(values, v...)
	}
	if len(values) > 16*maxNameVariants {
		return true, nil
	}
	if len(values) == 0 {
		return false, nil
	}
	next, err := tx.Prepare(`SELECT actor_account FROM memory_observations INDEXED BY idx_memory_obs_person_name
		WHERE ` + personNameExpr + `=? COLLATE NOCASE AND actor_account>? ORDER BY actor_account LIMIT 1`)
	if err != nil {
		return true, err
	}
	defer next.Close()
	here, err := tx.Prepare(`SELECT EXISTS(SELECT 1 FROM memory_observations INDEXED BY idx_memory_obs_person_name
		WHERE ` + personNameExpr + `=? COLLATE NOCASE AND actor_account=? AND source_room IN (SELECT key FROM json_each(?))
		AND platform='discord' AND record_kind=? AND deleted_at='')`)
	if err != nil {
		return true, err
	}
	defer here.Close()
	for _, v := range values {
		// Skip from account to account; a label shared by many accounts anywhere is not checked further.
		for cursor, n := "", 0; ; n++ {
			if n == maxNameAccounts {
				return true, nil
			}
			var other string
			if err := next.QueryRow(v, cursor).Scan(&other); err == sql.ErrNoRows {
				break
			} else if err != nil {
				return true, err
			}
			cursor = other
			if other == account {
				continue
			}
			var found bool
			if err := here.QueryRow(v, other, rooms, corpus.HumanText).Scan(&found); err != nil || found {
				return true, err
			}
		}
	}
	return false, nil
}

const aliasRangeSQL = `SELECT observation_id,source_room,actor_account,actor_label,parsed_time FROM memory_observations o NOT INDEXED WHERE rowid>? AND rowid<=? AND `
const eligibleAliasSQL = `platform='discord' AND record_kind=? AND actor_account<>'' AND actor_label<>'' AND deleted_at='' AND edited_at=''
				AND NOT EXISTS(SELECT 1 FROM privacy_tombstones p WHERE (p.kind='account' AND p.source_id=o.actor_account)
					OR (p.kind='message' AND p.source_id=o.native_message_id))`
const consentingAliasSQL = ` AND NOT EXISTS(SELECT 1 FROM consent_state c WHERE c.user_id=o.actor_account AND c.scope IN ('memory','profile') AND c.status='withdrawn')`

func indexAliasRows(tx *sql.Tx, query string, args ...any) error {
	var consent bool
	if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE name='consent_state')`).Scan(&consent); err != nil {
		return err
	}
	query += eligibleAliasSQL
	if consent {
		query += consentingAliasSQL
	}
	rows, err := tx.Query(query, append(args, corpus.HumanText)...)
	if err != nil {
		return err
	}
	defer rows.Close()
	var values [][]string
	for rows.Next() {
		var observation, room, id, label, at string
		if err := rows.Scan(&observation, &room, &id, &label, &at); err != nil {
			return err
		}
		if v := aliasValues(observation, room, id, label, at); v != nil {
			values = append(values, v)
		}
	}
	if err := rows.Err(); err != nil || len(values) == 0 {
		return err
	}
	// One bound argument per batch: the SQLite driver looks up positional
	// arguments linearly, making thousands of placeholders quadratic.
	raw, _ := json.Marshal(values)
	_, err = tx.Exec(`INSERT INTO person_alias_source(observation,guild_id,person_ref,label,room,at,folded)
		SELECT value->>0,value->>1,value->>2,value->>3,value->>4,value->>5,value->>6 FROM json_each(?) WHERE true
		ON CONFLICT(observation) DO NOTHING`, string(raw))
	return err
}

func indexCapturedAliases(ctx context.Context, db *storage.Store, ids []string) error {
	for len(ids) > 0 {
		n := min(len(ids), 64)
		raw, _ := json.Marshal(ids[:n])
		if err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
			return indexAliasRows(tx, `SELECT observation_id,source_room,actor_account,actor_label,parsed_time FROM memory_observations o
				WHERE observation_id IN (SELECT value FROM json_each(?)) AND `, string(raw))
		}); err != nil {
			return err
		}
		ids = ids[n:]
	}
	return nil
}

func fillAliasBatch(ctx context.Context, db *storage.Store) (bool, error) {
	done := false
	err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		var cursor int64
		err := tx.QueryRow(`SELECT cursor FROM person_alias_fill WHERE id=1`).Scan(&cursor)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		if cursor < 0 {
			done = true
			return nil
		}
		var end sql.NullInt64
		if err := tx.QueryRow(`SELECT max(rowid) FROM (SELECT rowid FROM memory_observations NOT INDEXED WHERE rowid>? ORDER BY rowid LIMIT 512)`, cursor).Scan(&end); err != nil {
			return err
		}
		if !end.Valid {
			done, cursor = true, -1
		} else {
			if err := indexAliasRows(tx, aliasRangeSQL, cursor, end.Int64); err != nil {
				return err
			}
			cursor = end.Int64
		}
		_, err = tx.Exec(`INSERT INTO person_alias_fill VALUES(1,?) ON CONFLICT(id) DO UPDATE SET cursor=excluded.cursor`, cursor)
		return err
	})
	return done, err
}

// FillPersonAliases runs outside startup: it builds the name check's index, then
// fills. Each batch releases the writer and gives competing captures time to
// acquire it; cancellation preserves the cursor.
func FillPersonAliases(ctx context.Context, db *storage.Store) error {
	if !db.IsSQLite() {
		return nil
	}
	// One writer transaction: captures wait for it (about a second per 200,000 rows).
	if err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, personNameIndexSQL)
		return err
	}); err != nil {
		return err
	}
	for {
		done, err := fillAliasBatch(ctx, db)
		if done || err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Millisecond):
		}
	}
}
