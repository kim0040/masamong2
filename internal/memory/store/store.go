// Package store persists memory: raw observations (append-only, deduplicated), leaves with their
// summaries and float16 vectors, and a content-addressed embedding cache. It is the durable side of
// internal/memory/search, which is rebuilt in memory from these rows.
//
// Raw source files are never modified; bodies stay in the instance DB and are never logged.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/storage"
)

// MessageHit is one exact source message matched by SearchMessages. Actor account IDs are
// deliberately omitted: operator search needs the visible label and provenance, not platform IDs.
type MessageHit struct {
	Message corpus.Message
	Score   int
}

var sqliteDDL = []string{
	`CREATE TABLE IF NOT EXISTS memory_observations (
		observation_id TEXT PRIMARY KEY,
		platform TEXT NOT NULL,
		collection TEXT NOT NULL,
		source_room TEXT NOT NULL,
		room TEXT NOT NULL,
		native_message_id TEXT,
		actor_label TEXT NOT NULL,
		actor_account TEXT NOT NULL DEFAULT '',
		content TEXT NOT NULL,
		parsed_time TEXT NOT NULL,
		record_kind TEXT NOT NULL,
		imported_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_memory_obs_room ON memory_observations(room, parsed_time)`,
	`CREATE UNIQUE INDEX IF NOT EXISTS idx_memory_obs_native ON memory_observations(platform, native_message_id)`,
	`CREATE TABLE IF NOT EXISTS memory_leaves (
		leaf_id TEXT PRIMARY KEY,
		room TEXT NOT NULL,
		recipe TEXT NOT NULL,
		first_observation_id TEXT NOT NULL,
		observation_ids TEXT NOT NULL,
		n INTEGER NOT NULL,
		body TEXT NOT NULL,
		body_hash TEXT NOT NULL,
		speakers TEXT NOT NULL,
		end_time TEXT NOT NULL DEFAULT '',
		closed INTEGER NOT NULL DEFAULT 0,
		summary TEXT NOT NULL DEFAULT '',
		embed_space TEXT NOT NULL DEFAULT '',
		leaf_vec BLOB,
		summary_vec BLOB,
		updated_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_memory_leaves_room ON memory_leaves(room)`,
	// the leaves that end at or after a time, in one room: LeavesOf and the recent-summary reads
	`CREATE INDEX IF NOT EXISTS idx_memory_leaves_room_end ON memory_leaves(room, end_time)`,
	`CREATE TABLE IF NOT EXISTS memory_embed_cache (
		cache_key TEXT PRIMARY KEY,
		vec BLOB NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS chat_turns (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		channel_key TEXT NOT NULL,
		role TEXT NOT NULL,
		speaker TEXT NOT NULL DEFAULT '',
		content TEXT NOT NULL,
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_chat_turns_channel ON chat_turns(channel_key, id)`,
	`CREATE TABLE IF NOT EXISTS memory_spend (
		day TEXT NOT NULL,
		kind TEXT NOT NULL,
		usd REAL NOT NULL,
		calls INTEGER NOT NULL,
		PRIMARY KEY(day, kind)
	)`,
	`CREATE TABLE IF NOT EXISTS memory_rooms (
		room TEXT PRIMARY KEY,
		collection TEXT NOT NULL,
		platform TEXT NOT NULL,
		label TEXT NOT NULL DEFAULT '',
		naive_timezone TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL
	)`,
}

var tidbDDL = []string{
	`CREATE TABLE IF NOT EXISTS memory_observations (
		observation_id VARCHAR(80) NOT NULL PRIMARY KEY,
		platform VARCHAR(32) NOT NULL,
		collection VARCHAR(64) NOT NULL,
		source_room VARCHAR(160) NOT NULL,
		room VARCHAR(160) NOT NULL,
		native_message_id VARCHAR(80),
		actor_label VARCHAR(255) NOT NULL,
		actor_account VARCHAR(80) NOT NULL DEFAULT '',
		content MEDIUMTEXT NOT NULL,
		parsed_time VARCHAR(64) NOT NULL,
		record_kind VARCHAR(32) NOT NULL,
		imported_at VARCHAR(64) NOT NULL,
		KEY idx_memory_obs_room (room, parsed_time),
		UNIQUE KEY idx_memory_obs_native (platform, native_message_id)
	)`,
	`CREATE TABLE IF NOT EXISTS memory_leaves (
		leaf_id VARCHAR(64) NOT NULL PRIMARY KEY,
		room VARCHAR(160) NOT NULL,
		recipe VARCHAR(96) NOT NULL,
		first_observation_id VARCHAR(80) NOT NULL,
		observation_ids MEDIUMTEXT NOT NULL,
		n INT NOT NULL,
		body MEDIUMTEXT NOT NULL,
		body_hash VARCHAR(64) NOT NULL,
		speakers TEXT NOT NULL,
		end_time VARCHAR(64) NOT NULL DEFAULT '',
		closed TINYINT NOT NULL DEFAULT 0,
		summary TEXT NOT NULL,
		embed_space VARCHAR(160) NOT NULL DEFAULT '',
		leaf_vec MEDIUMBLOB,
		summary_vec MEDIUMBLOB,
		updated_at VARCHAR(64) NOT NULL,
		KEY idx_memory_leaves_room (room)
	)`,
	`CREATE TABLE IF NOT EXISTS memory_embed_cache (
		cache_key VARCHAR(64) NOT NULL PRIMARY KEY,
		vec MEDIUMBLOB NOT NULL,
		created_at VARCHAR(64) NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS chat_turns (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		channel_key VARCHAR(160) NOT NULL,
		role VARCHAR(16) NOT NULL,
		speaker VARCHAR(255) NOT NULL DEFAULT '',
		content MEDIUMTEXT NOT NULL,
		created_at VARCHAR(64) NOT NULL,
		KEY idx_chat_turns_channel (channel_key, id)
	)`,
	`CREATE TABLE IF NOT EXISTS memory_spend (
		day VARCHAR(16) NOT NULL,
		kind VARCHAR(32) NOT NULL,
		usd DOUBLE NOT NULL,
		calls INT NOT NULL,
		PRIMARY KEY(day, kind)
	)`,
	`CREATE TABLE IF NOT EXISTS memory_rooms (
		room VARCHAR(160) NOT NULL PRIMARY KEY,
		collection VARCHAR(64) NOT NULL,
		platform VARCHAR(32) NOT NULL,
		label VARCHAR(255) NOT NULL DEFAULT '',
		naive_timezone VARCHAR(64) NOT NULL DEFAULT '',
		updated_at VARCHAR(64) NOT NULL
	)`,
}

// Store wraps the instance DB with memory tables.
type Store struct {
	DB     *storage.Store
	syncMu sync.Mutex
	// WHY immutable after Open: optional migration failure disables only the
	// optimization; source writes and full cuts must remain available.
	leafSyncEnabled bool
}

// Open migrates the memory tables (idempotent).
func Open(db *storage.Store) (*Store, error) {
	stmts := append(append([]string{}, sqliteDDL...), messageVectorDDL...)
	if db.Backend == storage.BackendTiDB {
		stmts = append(append([]string{}, tidbDDL...), tidbMessageVectorDDL...)
	}
	for i, q := range stmts {
		if _, err := db.DB.Exec(q); err != nil {
			return nil, fmt.Errorf("memory migration %d: %w", i, err)
		}
	}
	if err := migrateRoomState(db); err != nil {
		return nil, err
	}
	// kind separates question embeddings ('query', pruned by age) from leaf and summary embeddings
	// ('' — kept: they save paid re-embedding when rooms are re-cut or re-imported)
	if !db.HasColumn("memory_embed_cache", "kind") {
		q := `ALTER TABLE memory_embed_cache ADD COLUMN kind TEXT NOT NULL DEFAULT ''`
		if db.Backend == storage.BackendTiDB {
			q = `ALTER TABLE memory_embed_cache ADD COLUMN kind VARCHAR(16) NOT NULL DEFAULT ''`
		}
		if _, err := db.DB.Exec(q); err != nil {
			return nil, fmt.Errorf("memory migration: embed cache kind: %w", err)
		}
	}
	// message vectors made before the label column existed get an empty label, so they are embedded
	// again once and carry the label from then on; the format column marks float16 rows of the first
	// version, which load as they are
	if err := migrateMessageVectors(db); err != nil {
		return nil, err
	}
	if _, err := db.DB.Exec(`CREATE INDEX IF NOT EXISTS idx_memory_embed_cache_kind ON memory_embed_cache(kind, created_at)`); err != nil {
		return nil, fmt.Errorf("memory migration: embed cache index: %w", err)
	}
	// a leaf re-cut with the same text (another recipe, a re-import) takes the stored vector over
	if _, err := db.DB.Exec(`CREATE INDEX IF NOT EXISTS idx_memory_leaves_hash ON memory_leaves(body_hash)`); err != nil {
		return nil, fmt.Errorf("memory migration: leaf hash index: %w", err)
	}
	if _, err := db.DB.Exec(`CREATE INDEX IF NOT EXISTS idx_memory_leaves_room_recipe ON memory_leaves(room,recipe)`); err != nil {
		return nil, fmt.Errorf("memory migration: active leaf index: %w", err)
	}
	// edited_at / deleted_at: a live Discord message that its author edited or deleted (EditMessage,
	// DeleteMessages). native_id ties a stored chat turn to the Discord message it came from.
	for _, c := range []struct{ table, col, sqlite, tidb string }{
		{"memory_observations", "edited_at", `TEXT NOT NULL DEFAULT ''`, `VARCHAR(64) NOT NULL DEFAULT ''`},
		{"memory_observations", "deleted_at", `TEXT NOT NULL DEFAULT ''`, `VARCHAR(64) NOT NULL DEFAULT ''`},
		{"chat_turns", "native_id", `TEXT NOT NULL DEFAULT ''`, `VARCHAR(80) NOT NULL DEFAULT ''`},
		{"chat_turns", "speaker_id", `TEXT NOT NULL DEFAULT ''`, `VARCHAR(80) NOT NULL DEFAULT ''`},
		{"chat_turns", "evidence_state", `INTEGER NOT NULL DEFAULT 0`, `TINYINT NOT NULL DEFAULT 0`},
	} {
		if db.HasColumn(c.table, c.col) {
			continue
		}
		def := c.sqlite
		if db.Backend == storage.BackendTiDB {
			def = c.tidb
		}
		if _, err := db.DB.Exec(`ALTER TABLE ` + c.table + ` ADD COLUMN ` + c.col + ` ` + def); err != nil {
			return nil, fmt.Errorf("memory migration: %s.%s: %w", c.table, c.col, err)
		}
	}
	if _, err := db.DB.Exec(`CREATE INDEX IF NOT EXISTS idx_chat_turns_native ON chat_turns(native_id)`); err != nil {
		return nil, fmt.Errorf("memory migration: chat turn index: %w", err)
	}
	if _, err := db.DB.Exec(`CREATE INDEX IF NOT EXISTS idx_chat_turns_speaker ON chat_turns(speaker_id)`); err != nil {
		return nil, fmt.Errorf("memory migration: chat turn speaker index: %w", err)
	}
	// one person's messages: withdrawing consent deletes them, the console names them
	if _, err := db.DB.Exec(`CREATE INDEX IF NOT EXISTS idx_memory_obs_account ON memory_observations(platform, actor_account)`); err != nil {
		return nil, fmt.Errorf("memory migration: account index: %w", err)
	}
	// the console's per-room counts and spans (Rooms, RoomSpans, Counts) read only this index, not the
	// rows and their text: at 100,000 messages on one core they took about 2 s per memory.blocks.list and
	// 0.1 s with it. SQLite only: TiDB's text columns cannot be indexed whole, and it is not in use.
	if db.Backend != storage.BackendTiDB {
		if _, err := db.DB.Exec(`CREATE INDEX IF NOT EXISTS idx_memory_obs_room_live ON memory_observations(room, deleted_at, parsed_time, actor_label)`); err != nil {
			return nil, fmt.Errorf("memory migration: live room index: %w", err)
		}
	}
	st := &Store{DB: db}
	if err := migrateLeafSync(db); err != nil {
		// WHY nonfatal: a checkpoint saves work but is not required for memory
		// correctness. Warn once at Open, then bypass all checkpoint hooks.
		slog.Warn("memory leaf checkpoint unavailable; using full cuts", "err", err.Error()) // SQLite errors name tables/columns, never row values
	} else {
		st.leafSyncEnabled = !st.tidb()
	}
	if err := MigratePrivacy(db); err != nil {
		return nil, err
	}
	if err := st.migrateSummaryCache(); err != nil {
		return nil, err
	}
	if err := st.migrateFallbackSummaries(); err != nil {
		return nil, err
	}
	if err := st.migrateAliases(); err != nil {
		return nil, err
	}
	if err := migrateEpisodes(db); err != nil {
		return nil, err
	}
	if err := migrateBatchPlans(db); err != nil {
		return nil, fmt.Errorf("memory migration: batch plans: %w", err)
	}
	return st, nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

func (s *Store) tidb() bool { return s.DB.Backend == storage.BackendTiDB }

// Room is a registered source room.
type Room struct {
	Room          string `json:"room"`
	Collection    string `json:"collection"`
	Platform      string `json:"platform"`
	Label         string `json:"label"`
	NaiveTimezone string `json:"naive_timezone,omitempty"`
}

// UpsertRoom registers a room (label and naive timezone declaration).
func (s *Store) UpsertRoom(tx *sql.Tx, r Room) error {
	q := `INSERT INTO memory_rooms(room, collection, platform, label, naive_timezone, updated_at) VALUES(?,?,?,?,?,?)
		ON CONFLICT(room) DO UPDATE SET collection=excluded.collection, label=CASE WHEN excluded.label<>'' THEN excluded.label ELSE memory_rooms.label END,
		naive_timezone=CASE WHEN excluded.naive_timezone<>'' THEN excluded.naive_timezone ELSE memory_rooms.naive_timezone END, updated_at=excluded.updated_at`
	if s.tidb() {
		q = `INSERT INTO memory_rooms(room, collection, platform, label, naive_timezone, updated_at) VALUES(?,?,?,?,?,?)
		ON DUPLICATE KEY UPDATE collection=VALUES(collection), label=IF(VALUES(label)<>'', VALUES(label), label),
		naive_timezone=IF(VALUES(naive_timezone)<>'', VALUES(naive_timezone), naive_timezone), updated_at=VALUES(updated_at)`
	}
	if _, err := tx.Exec(q, r.Room, r.Collection, r.Platform, r.Label, r.NaiveTimezone, now()); err != nil {
		return err
	}
	_, err := s.bumpRoomRevision(context.Background(), tx, r.Room, false)
	return err
}

// Rooms lists registered rooms with message counts.
func (s *Store) Rooms() ([]RoomStats, error) {
	// the leaf numbers come from one pass over the leaves grouped by room and recipe, not three
	// correlated counts per room: on a large memory each of those reads every leaf of the room again
	type leafCounts struct{ leaves, embedded, open int }
	byRoom := map[[2]string]leafCounts{}
	// both reads in one read transaction: the counts and the rooms are of the same moment
	tx, err := s.DB.Reader().BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	lrows, err := tx.Query(`SELECT room, recipe, COUNT(*), COALESCE(SUM(CASE WHEN leaf_vec IS NOT NULL THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN closed=0 THEN 1 ELSE 0 END),0) FROM memory_leaves GROUP BY room, recipe`)
	if err != nil {
		return nil, err
	}
	for lrows.Next() {
		var room, recipe string
		var c leafCounts
		if err := lrows.Scan(&room, &recipe, &c.leaves, &c.embedded, &c.open); err != nil {
			lrows.Close()
			return nil, err
		}
		byRoom[[2]string{room, recipe}] = c
	}
	if err := lrows.Err(); err != nil {
		lrows.Close()
		return nil, err
	}
	lrows.Close()
	rows, err := tx.Query(`SELECT r.room, r.collection, r.platform, r.label, r.naive_timezone, r.updated_at, r.revision, r.active_recipe,
		(SELECT COUNT(*) FROM memory_observations o WHERE o.room=r.room AND o.deleted_at='')
		FROM memory_rooms r ORDER BY r.room`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoomStats
	for rows.Next() {
		var r RoomStats
		if err := rows.Scan(&r.Room.Room, &r.Collection, &r.Platform, &r.Label, &r.NaiveTimezone, &r.UpdatedAt, &r.Revision, &r.ActiveRecipe, &r.Messages); err != nil {
			return nil, err
		}
		c := byRoom[[2]string{r.Room.Room, r.ActiveRecipe}]
		r.Leaves, r.Embedded, r.Open = c.leaves, c.embedded, c.open
		out = append(out, r)
	}
	return out, rows.Err()
}

// RoomStats is a room with counts.
type RoomStats struct {
	Room
	Messages int `json:"messages"`
	Leaves   int `json:"leaves"`
	Embedded int `json:"embedded"`
	// Open counts leaves still open (the tail of a room that was active within the gap). They are not
	// embedded until they close, by design, so they do not count against a room being ready.
	Open int `json:"open"`
	// UpdatedAt changes whenever messages are added to the room (and on label changes)
	UpdatedAt    string `json:"-"`
	Revision     int64  `json:"-"` // durable identity of the room's message snapshot
	ActiveRecipe string `json:"-"`
}

// RoomSpan is when a room's stored messages start and end and how many people wrote them.
type RoomSpan struct {
	First    string `json:"first"`
	Last     string `json:"last"`
	Speakers int    `json:"speakers"`
}

// RoomSpans returns the time span and speaker count of every room with messages.
func (s *Store) RoomSpans() (map[string]RoomSpan, error) {
	rows, err := s.DB.Reader().Query(`SELECT room, MIN(parsed_time), MAX(parsed_time), COUNT(DISTINCT actor_label) FROM memory_observations WHERE deleted_at='' GROUP BY room`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RoomSpan{}
	for rows.Next() {
		var room string
		var sp RoomSpan
		if err := rows.Scan(&room, &sp.First, &sp.Last, &sp.Speakers); err != nil {
			return nil, err
		}
		out[room] = sp
	}
	return out, rows.Err()
}

// ErrNoRoom means the room is not registered.
var ErrNoRoom = errors.New("memory: no such room")

// SetRoomLabel renames a room; evidence and the console show the new label.
func (s *Store) SetRoomLabel(room, label string) error {
	return s.DB.WithWriter(func(tx *sql.Tx) error {
		res, err := tx.Exec(`UPDATE memory_rooms SET label=?, updated_at=? WHERE room=?`, label, now(), room)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 {
			return ErrNoRoom
		}
		_, err = s.bumpRoomRevision(context.Background(), tx, room, false)
		return err
	})
}

// DeleteRoom removes a room's stored messages, leaves and registration from the product database.
// The source files it was imported from are not touched; importing them again restores the room.
func (s *Store) DeleteRoom(room string) (int, error) {
	var removed int
	err := s.DB.WithWriter(func(tx *sql.Tx) error {
		if !s.tidb() {
			if _, err := tx.Exec(`DELETE FROM memory_observation_aliases WHERE observation_id IN (SELECT observation_id FROM memory_observations WHERE room=?)`, room); err != nil {
				return err
			}
		}
		if !s.tidb() {
			// cached embeddings of the room's message texts (message vectors) are tied to the messages
			if _, err := tx.Exec(`DELETE FROM memory_embed_cache WHERE cache_key IN (SELECT holder_id FROM evidence_refs WHERE holder_kind='cache'
				AND source_id IN (SELECT observation_id FROM memory_observations WHERE room=?))`, room); err != nil {
				return err
			}
		}
		res, err := tx.Exec(`DELETE FROM memory_observations WHERE room=?`, room)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		removed = int(n)
		if _, err := tx.Exec(`DELETE FROM memory_leaves WHERE room=?`, room); err != nil {
			return err
		}
		if s.leafSyncEnabled {
			// A replacement room must never inherit the old cut's trusted prefix.
			if _, err := tx.Exec(`DELETE FROM memory_leaf_sync WHERE room=?`, room); err != nil {
				return err
			}
		}
		if _, err := tx.Exec(`DELETE FROM memory_message_vectors WHERE room=?`, room); err != nil {
			return err
		}
		// an in-flight batch plan names the room's leaves and messages (ids and text hashes)
		if _, err := tx.Exec(`DELETE FROM memory_batch_plans WHERE room=?`, room); err != nil {
			return err
		}
		if !s.tidb() {
			if err := DeleteRoomMemory(tx, room); err != nil {
				return err
			}
		}
		res, err = tx.Exec(`DELETE FROM memory_rooms WHERE room=?`, room)
		if err != nil {
			return err
		}
		if n, _ := res.RowsAffected(); n == 0 && removed == 0 {
			return ErrNoRoom
		}
		return nil
	})
	return removed, err
}

// Revision is one stored message changed by its author after it was captured.
type Revision struct {
	Room          string `json:"room"`
	ObservationID string `json:"observation_id"`
	Deleted       bool   `json:"deleted"`
	RoomRevision  int64  `json:"-"`
}

// EditMessage replaces the text of a stored message (platform + native message ID) with what its author
// changed it to, and the chat turn that quoted it. ok is false when the message is not stored, is
// deleted, or already has this text. An edit to empty text is a deletion.
func (s *Store) EditMessage(platform, nativeID, content, at string) (Revision, bool, error) {
	return s.EditMessageContext(context.Background(), platform, nativeID, content, at)
}

func (s *Store) EditMessageContext(ctx context.Context, platform, nativeID, content, at string) (Revision, bool, error) {
	if strings.TrimSpace(content) == "" {
		revs, err := s.DeleteMessagesContext(ctx, platform, []string{nativeID}, at)
		if err != nil || len(revs) == 0 {
			return Revision{}, false, err
		}
		return revs[0], true, nil
	}
	var rev Revision
	changed := false
	err := s.DB.WithWriterContext(ctx, func(tx *sql.Tx) error {
		if !s.tidb() {
			current, err := currentMessageRevision(tx, nativeID, at)
			if err != nil || !current {
				return err
			}
			if err := Tombstone(tx, "message", nativeID, at); err != nil {
				return err
			}
		}
		// the chat turn follows the edit whether or not the channel stores its messages (a channel the
		// bot answers in without capture still keeps the message as conversation context)
		if _, err := tx.Exec(`UPDATE chat_turns SET content=? WHERE native_id=? AND role='user'`, content, nativeID); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM chat_turns WHERE native_id=? AND role<>'user'`, nativeID); err != nil {
			return err
		}
		var old string
		err := tx.QueryRow(`SELECT observation_id, room, content FROM memory_observations
			WHERE platform=? AND native_message_id=? AND deleted_at=''`, platform, nativeID).Scan(&rev.ObservationID, &rev.Room, &old)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		if err != nil || old == content {
			return err
		}
		if _, err := tx.Exec(`UPDATE memory_observations SET content=?, edited_at=? WHERE observation_id=?`, content, at, rev.ObservationID); err != nil {
			return err
		}
		changed = true
		if err := invalidateLeaves(tx, rev.ObservationID); err != nil {
			return err
		}
		// the room's signature changes, so the next index build reads its messages again
		rev.RoomRevision, err = s.bumpRoomRevision(ctx, tx, rev.Room, true)
		return err
	})
	return rev, changed, err
}

// DeleteMessages removes the text of stored messages whose authors deleted them. The row stays as a
// tombstone (empty content, deleted_at set) so an archive import of the same channel later cannot bring
// the message back: imports skip native IDs that are already stored. Every read of messages leaves
// tombstones out. Chat turns that quoted the messages are removed.
func (s *Store) DeleteMessages(platform string, nativeIDs []string, at string) ([]Revision, error) {
	return s.DeleteMessagesContext(context.Background(), platform, nativeIDs, at)
}

func (s *Store) DeleteMessagesContext(ctx context.Context, platform string, nativeIDs []string, at string) ([]Revision, error) {
	var out []Revision
	err := s.DB.WithWriterContext(ctx, func(tx *sql.Tx) error {
		for _, id := range nativeIDs {
			if !s.tidb() {
				if err := Tombstone(tx, "message", id, at); err != nil {
					return err
				}
			}
			// the chat turn goes whether or not the message was stored (see EditMessage)
			if _, err := tx.Exec(`DELETE FROM chat_turns WHERE native_id=?`, id); err != nil {
				return err
			}
			var rev Revision
			err := tx.QueryRow(`SELECT observation_id, room FROM memory_observations
				WHERE platform=? AND native_message_id=? AND deleted_at=''`, platform, id).Scan(&rev.ObservationID, &rev.Room)
			if errors.Is(err, sql.ErrNoRows) {
				continue
			}
			if err != nil {
				return err
			}
			if _, err := tx.Exec(`UPDATE memory_observations SET content='', deleted_at=? WHERE observation_id=?`, at, rev.ObservationID); err != nil {
				return err
			}
			rev.Deleted = true
			out = append(out, rev)
		}
		// one pass over the leaves for the whole batch, not one per message
		ids := make([]string, len(out))
		for i, r := range out {
			ids[i] = r.ObservationID
		}
		if err := invalidateLeaves(tx, ids...); err != nil {
			return err
		}
		return s.reviseRooms(ctx, tx, out)
	})
	return out, err
}

// Remove persisted leaf bodies/summaries immediately; the delayed resync builds fresh leaves.
//
// A few messages (an edit, a deletion) are found by a substring scan of each leaf's ID list, which is
// cheap even for a large history; a bulk change (a person withdrawing) is found with one JSON pass per
// chunk of messages instead of one pass per message.
func invalidateLeaves(tx *sql.Tx, observationIDs ...string) error {
	if len(observationIDs) == 0 {
		return nil
	}
	raw, _ := json.Marshal(observationIDs)
	if _, err := tx.Exec(`INSERT INTO privacy_tombstones(kind,source_id,at) SELECT 'source',value,? FROM json_each(?) WHERE true ON CONFLICT(kind,source_id) DO UPDATE SET at=excluded.at`, now(), string(raw)); err != nil {
		return err
	}
	if _, err := tx.Exec(`INSERT INTO privacy_tombstones(kind,source_id,at) SELECT DISTINCT 'source',l.leaf_id,? FROM memory_leaves l,json_each(l.observation_ids) j WHERE j.value IN (SELECT value FROM json_each(?)) ON CONFLICT(kind,source_id) DO UPDATE SET at=excluded.at`, now(), string(raw)); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM memory_embed_cache WHERE cache_key IN (SELECT holder_id FROM evidence_refs WHERE holder_kind='cache' AND source_id IN (SELECT value FROM json_each(?)))`, string(raw)); err != nil {
		return err
	}
	if err := deleteMessageVectors(tx, string(raw)); err != nil {
		return err
	}
	const clear = `UPDATE memory_leaves SET body='', body_hash='', speakers='[]', summary='', leaf_vec=NULL, summary_vec=NULL WHERE `
	const few, chunk = 8, 500
	for lo := 0; lo < len(observationIDs); lo += chunk {
		ids := observationIDs[lo:min(len(observationIDs), lo+chunk)]
		ph := strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")
		if len(ids) > few {
			if _, err := tx.Exec(clear+`leaf_id IN (SELECT l.leaf_id FROM memory_leaves l, json_each(l.observation_ids) j WHERE j.value IN (`+ph+`))`, toAny(ids)...); err != nil {
				return err
			}
			continue
		}
		// the ID list is stored as JSON, so an ID occurs in it as its JSON string; json_each then
		// confirms it is a whole element
		var cond []string
		var args []any
		for _, id := range ids {
			quoted, _ := json.Marshal(id)
			cond = append(cond, `instr(memory_leaves.observation_ids, ?)>0`)
			args = append(args, string(quoted))
		}
		args = append(args, toAny(ids)...)
		if _, err := tx.Exec(clear+`(`+strings.Join(cond, " OR ")+`) AND EXISTS (SELECT 1 FROM json_each(memory_leaves.observation_ids) WHERE value IN (`+ph+`))`, args...); err != nil {
			return err
		}
	}
	return nil
}

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

// AccountLabels is the name each platform account last wrote under (the console's People page).
func (s *Store) AccountLabels(platform string, accounts []string) (map[string]string, error) {
	out := map[string]string{}
	for _, a := range accounts {
		var label string
		err := s.DB.DB.QueryRow(`SELECT actor_label FROM memory_observations WHERE platform=? AND actor_account=?
			ORDER BY parsed_time DESC LIMIT 1`, platform, a).Scan(&label)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return out, err
		}
		out[a] = label
	}
	return out, nil
}

// DeleteAccountMessages deletes every stored message of one platform account (a person who withdrew
// consent to being remembered), as DeleteMessages does for messages deleted one by one, and the
// person's chat turns. Imported
// messages without a native ID (KakaoTalk) have no account to match and are not touched.
func (s *Store) DeleteAccountMessages(platform, account, at string) ([]Revision, error) {
	if strings.TrimSpace(account) == "" {
		return nil, nil
	}
	// the person's lines in the bot's conversations, wherever they were said (a channel without
	// capture has no stored message to find them by)
	if err := s.DB.WithWriter(func(tx *sql.Tx) error {
		if !s.tidb() {
			if err := Tombstone(tx, "account", account, at); err != nil {
				return err
			}
		}
		_, err := tx.Exec(`DELETE FROM chat_turns WHERE speaker_id=?`, account)
		return err
	}); err != nil {
		return nil, err
	}
	rows, err := s.DB.DB.Query(`SELECT native_message_id FROM memory_observations
		WHERE platform=? AND actor_account=? AND deleted_at='' AND native_message_id IS NOT NULL`, platform, account)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	rows.Close()
	var out []Revision
	for lo := 0; lo < len(ids); lo += insertBatch {
		revs, err := s.DeleteMessages(platform, ids[lo:min(len(ids), lo+insertBatch)], at)
		out = append(out, revs...)
		if err != nil {
			return out, err
		}
	}
	// A bounded batch covers native-less account rows in one leaf pass. deleted_at is resumable
	// progress: a failed or cancelled withdrawal repeats only rows not yet erased.
	for {
		var revisions []Revision
		err = s.DB.WithWriter(func(tx *sql.Tx) error {
			rows, err := tx.Query(`SELECT observation_id,room FROM memory_observations WHERE platform=? AND actor_account=? AND deleted_at='' AND (native_message_id IS NULL OR native_message_id='') ORDER BY observation_id LIMIT 400`, platform, account)
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
				revisions = append(revisions, r)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			var ids []string
			for _, r := range revisions {
				if _, err := tx.Exec(`UPDATE memory_observations SET content='',deleted_at=? WHERE observation_id=?`, at, r.ObservationID); err != nil {
					return err
				}
				ids = append(ids, r.ObservationID)
			}
			if err := invalidateLeaves(tx, ids...); err != nil {
				return err
			}
			return s.reviseRooms(context.Background(), tx, revisions)
		})
		if err != nil {
			return out, err
		}
		out = append(out, revisions...)
		if len(revisions) < 400 {
			break
		}
	}
	return out, err
}

// TimelineEntry is one closed leaf placed on a timeline.
type TimelineEntry struct {
	LeafID   string   `json:"doc_id"`
	Room     string   `json:"room"`
	Time     string   `json:"time"` // when the leaf's last message was written
	Summary  string   `json:"summary,omitempty"`
	Excerpt  string   `json:"excerpt"`
	Speakers []string `json:"speakers"`
}

// Timeline lists the closed leaves of rooms between from and to (RFC 3339 prefixes, "" = open),
// newest first. Excerpt is the leaf's first lines without its "[room · date]" prefix.
func (s *Store) Timeline(rooms []string, from, to string, limit int) ([]TimelineEntry, error) {
	if len(rooms) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",")
	args := make([]any, 0, len(rooms)+3)
	for _, r := range rooms {
		args = append(args, r)
	}
	q := `SELECT leaf_id, room, end_time, summary, body, speakers FROM memory_leaves WHERE room IN (` + ph + `) AND closed=1`
	if from != "" {
		q += ` AND end_time >= ?`
		args = append(args, from)
	}
	if to != "" {
		q += ` AND end_time < ?`
		args = append(args, to)
	}
	q += ` ORDER BY end_time DESC LIMIT ?`
	args = append(args, max(1, min(limit, 500)))
	rows, err := s.DB.Reader().Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TimelineEntry
	for rows.Next() {
		var e TimelineEntry
		var body, sp string
		if err := rows.Scan(&e.LeafID, &e.Room, &e.Time, &e.Summary, &body, &sp); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(sp), &e.Speakers)
		lines := strings.Split(body, "\n")
		if len(lines) > 1 && strings.HasPrefix(lines[0], "[") {
			lines = lines[1:]
		}
		e.Excerpt = strings.Join(lines[:min(3, len(lines))], "\n")
		out = append(out, e)
	}
	return out, rows.Err()
}

// LatestNativeID is the newest stored native message ID of a room (deleted ones included: they were
// seen). Discord snowflakes grow with time, so it is where a catch-up read continues from.
func (s *Store) LatestNativeID(room string) (string, error) {
	var id sql.NullString
	// snowflakes are compared as numbers: by length first, then as text
	err := s.DB.DB.QueryRow(`SELECT native_message_id FROM memory_observations WHERE room=? AND native_message_id IS NOT NULL
		ORDER BY LENGTH(native_message_id) DESC, native_message_id DESC LIMIT 1`, room).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return id.String, err
}

// InsertResult counts one import batch.
type InsertResult struct {
	Seen      int `json:"seen"`
	Inserted  int `json:"inserted"`
	Duplicate int `json:"duplicate"`
	Skipped   int `json:"skipped_ineligible"`
}

// InsertObservations stores eligible observations (human text with a time). Existing observation IDs
// and already-known native message IDs are skipped, so re-import and live-then-archive are no-ops.
// A large import (a KakaoTalk export of several years) is committed in batches: under TiDB's
// transaction size limit, and without holding the writer long enough to stall live capture. A failed
// import can simply be run again (duplicates are skipped).
func (s *Store) InsertObservations(obs []corpus.Observation, merge map[string]string) (InsertResult, error) {
	return s.InsertObservationsContext(context.Background(), obs, merge)
}

func (s *Store) InsertObservationsContext(ctx context.Context, obs []corpus.Observation, merge map[string]string) (InsertResult, error) {
	var res InsertResult
	counts := corpus.NewKakaoOccurrences()
	for i := range obs {
		if i%256 == 0 {
			if err := ctx.Err(); err != nil {
				return res, err
			}
		}
		o := &obs[i]
		if o.Platform == "kakao" && o.IdentityID == "" {
			n, err := counts.Next(o.SourceRoom, o.ParsedTime, o.ActorLabel, o.Content)
			if err != nil {
				return res, err
			}
			o.IdentityID = corpus.KakaoRowIdentity(o.SourceRoom, o.ParsedTime, o.ActorLabel, o.Content, n)
		}
	}
	for lo := 0; lo < len(obs); lo += insertBatch {
		r, err := s.insertObservations(ctx, obs[lo:min(len(obs), lo+insertBatch)], merge)
		res.Seen += r.Seen
		res.Inserted += r.Inserted
		res.Duplicate += r.Duplicate
		res.Skipped += r.Skipped
		if err != nil {
			return res, err
		}
	}
	return res, nil
}

const insertBatch = 2000

func (s *Store) insertObservations(ctx context.Context, obs []corpus.Observation, merge map[string]string) (InsertResult, error) {
	var res InsertResult
	err := s.DB.WithWriterContext(ctx, func(tx *sql.Tx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		ins := `INSERT INTO memory_observations(observation_id, platform, collection, source_room, room, native_message_id, actor_label,
			actor_account, content, parsed_time, record_kind, imported_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?) ON CONFLICT DO NOTHING`
		if s.tidb() {
			ins = `INSERT IGNORE INTO memory_observations(observation_id, platform, collection, source_room, room, native_message_id, actor_label,
			actor_account, content, parsed_time, record_kind, imported_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`
		}
		stmt, err := tx.PrepareContext(ctx, ins)
		if err != nil {
			return err
		}
		defer stmt.Close()
		// The gateway's early opt-out check can race a withdrawal. Check again under the same
		// writer transaction as insertion, including archive imports with known Discord accounts.
		var consentTable int
		if !s.tidb() {
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name='consent_state'`).Scan(&consentTable); err != nil {
				return err
			}
		}
		aliases := map[string]string{}
		var pendingAliases []observationAlias
		if !s.tidb() {
			aliases, err = loadObservationAliases(ctx, tx, obs)
			if err != nil {
				return err
			}
		}
		withdrawn := map[string]bool{}
		ts := now()
		rooms := map[string]corpus.Observation{}
		appends := map[string]leafAppend{}
		for _, o := range obs {
			res.Seen++
			if !o.Eligible() {
				res.Skipped++
				continue
			}
			if !s.tidb() && o.Platform == "discord" {
				blocked, err := MessageBlocked(tx, o.NativeMessageID, o.ActorAccount)
				if err != nil {
					return err
				}
				if blocked {
					res.Skipped++
					continue
				}
			}
			if consentTable > 0 && o.Platform == "discord" && o.ActorAccount != "" {
				denied, known := withdrawn[o.ActorAccount]
				if !known {
					var status string
					err := tx.QueryRowContext(ctx, `SELECT status FROM consent_state WHERE user_id=? AND scope='memory'`, o.ActorAccount).Scan(&status)
					if err != nil && !errors.Is(err, sql.ErrNoRows) {
						return err
					}
					denied = status == "withdrawn"
					withdrawn[o.ActorAccount] = denied
				}
				if denied {
					res.Skipped++
					continue
				}
			}
			if !s.tidb() && o.Platform == "kakao" {
				id := ""
				for _, key := range []string{o.ObservationID, o.LegacyID, o.IdentityID} {
					if aliases[key] != "" {
						id = aliases[key]
						break
					}
				}
				if id != "" {
					pendingAliases = append(pendingAliases, observationAliases(o, id, aliases)...)
					res.Duplicate++
					continue
				}
			}
			m := o.Message(merge)
			var native any
			if o.NativeMessageID != "" {
				native = o.NativeMessageID
			}
			r, err := stmt.ExecContext(ctx, o.ObservationID, o.Platform, o.Collection, o.SourceRoom, m.Room, native, m.ActorLabel,
				m.ActorAccount, o.Content, o.ParsedTime, o.RecordKind, ts)
			if err != nil {
				return err
			}
			if n, _ := r.RowsAffected(); n > 0 {
				res.Inserted++
				a := appends[m.Room]
				key := messageKey(m)
				if a.n == 0 || key.before(a.first) {
					a.first = key
				}
				a.n++
				appends[m.Room] = a
				if !s.tidb() && o.Platform == "kakao" {
					pendingAliases = append(pendingAliases, observationAliases(o, o.ObservationID, aliases)...)
				}
				rooms[m.Room] = o
			} else {
				res.Duplicate++
			}
		}
		if err := saveObservationAliases(ctx, tx, pendingAliases); err != nil {
			return err
		}
		// rooms in a fixed order: two concurrent imports on TiDB never lock them in opposite orders
		names := make([]string, 0, len(rooms))
		for room := range rooms {
			names = append(names, room)
		}
		sort.Strings(names)
		for _, room := range names {
			o := rooms[room]
			if err := s.UpsertRoom(tx, Room{Room: room, Collection: o.Collection, Platform: o.Platform}); err != nil {
				return err
			}
			// Commit the append watermark with the source rows; wall-clock import
			// times can tie or arrive late and cannot prove an unchanged prefix.
			if err := s.recordLeafAppends(ctx, tx, room, appends[room]); err != nil {
				return err
			}
		}
		return ctx.Err()
	})
	if err != nil {
		return InsertResult{}, err
	} // this batch rolled back
	return res, err
}

// Messages returns every stored message of the rooms, room-ordered (corpus.OrderByRoom order).
func (s *Store) Messages(rooms []string) ([]corpus.Message, error) {
	return s.LoadMessages(rooms, 0)
}

// LoadMessages uses the already-read room count only as a capacity hint. Reserving
// once avoids repeatedly copying the whole message history during a cold build;
// concurrent inserts/deletes still determine the actual number of returned rows.
func (s *Store) LoadMessages(rooms []string, capacity int) ([]corpus.Message, error) {
	if len(rooms) == 0 {
		return nil, nil
	}
	return s.loadMessageInputs(rooms, capacity, false)
}

// LoadIdentityInputs reads ordering and native identity fields only. Eligibility is
// selected by the engine using the same metadata/placement path as a full build;
// raw text DISTINCT and message bodies are deliberately absent.
func (s *Store) LoadIdentityInputs(rooms []string) ([]corpus.Message, error) {
	return s.loadMessageInputs(rooms, 0, true)
}

func (s *Store) loadMessageInputs(rooms []string, capacity int, identityOnly bool) ([]corpus.Message, error) {
	if len(rooms) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",")
	args := make([]any, len(rooms))
	for i, r := range rooms {
		args[i] = r
	}
	content := "content"
	if identityOnly {
		content = "''"
	}
	rows, err := s.DB.Reader().Query(`SELECT observation_id, room, source_room, actor_label, actor_account, `+content+`, parsed_time
		FROM memory_observations WHERE room IN (`+ph+`) AND deleted_at=''`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]corpus.Message, 0, max(0, capacity))
	for rows.Next() {
		var m corpus.Message
		if err := rows.Scan(&m.ObservationID, &m.Room, &m.RoomOriginal, &m.ActorLabel, &m.ActorAccount, &m.Content, &m.ParsedTime); err != nil {
			return nil, err
		}
		m.Content = strings.TrimSpace(m.Content)
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Store owns this slice: sorting it in place avoids OrderByRoom's additional
	// per-room slices and final history copy. Use exactly its four string keys
	// (not parsed timestamps) so naive times, merges and ties retain parity.
	// WHY unstable sorting is safe: the unique observation_id makes these keys
	// a total order, so stable sorting only adds work on shuffled cold histories.
	slices.SortFunc(out, func(a, b corpus.Message) int {
		if a.Room != b.Room {
			return strings.Compare(a.Room, b.Room)
		}
		if a.ParsedTime != b.ParsedTime {
			return strings.Compare(a.ParsedTime, b.ParsedTime)
		}
		if a.RoomOriginal != b.RoomOriginal {
			return strings.Compare(a.RoomOriginal, b.RoomOriginal)
		}
		return strings.Compare(a.ObservationID, b.ObservationID)
	})
	return out, nil
}

// RecentMessages returns the last n messages of one room in time order (chat context).
func (s *Store) RecentMessages(room string, n int) ([]corpus.Message, error) {
	rows, err := s.DB.DB.Query(`SELECT observation_id, room, source_room, actor_label, actor_account, content, parsed_time
		FROM memory_observations WHERE room=? AND deleted_at='' ORDER BY parsed_time DESC, observation_id DESC LIMIT ?`, room, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []corpus.Message
	for rows.Next() {
		var m corpus.Message
		if err := rows.Scan(&m.ObservationID, &m.Room, &m.RoomOriginal, &m.ActorLabel, &m.ActorAccount, &m.Content, &m.ParsedTime); err != nil {
			return nil, err
		}
		m.Content = strings.TrimSpace(m.Content)
		out = append(out, m)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// LiveLine is one stored message of a room with its platform ID, for rebuilding a channel's recent
// conversation after a restart (application/live.go).
type LiveLine struct {
	ObservationID, NativeID, Account, Label, Content, ParsedTime string
}

// RecentLines returns the last n messages of one room in time order, with their platform message IDs.
func (s *Store) RecentLines(room string, n int) ([]LiveLine, error) {
	rows, err := s.DB.DB.Query(`SELECT observation_id, COALESCE(native_message_id,''), actor_account, actor_label, content, parsed_time
		FROM memory_observations WHERE room=? AND deleted_at='' ORDER BY parsed_time DESC, observation_id DESC LIMIT ?`, room, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiveLine
	for rows.Next() {
		var l LiveLine
		if err := rows.Scan(&l.ObservationID, &l.NativeID, &l.Account, &l.Label, &l.Content, &l.ParsedTime); err != nil {
			return nil, err
		}
		l.Content = strings.TrimSpace(l.Content)
		out = append(out, l)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// SearchMessages performs bounded lexical search over exact source messages. It is intentionally
// separate from semantic memory search: results are raw observations, use no model, and cost zero.
func (s *Store) SearchMessages(rooms []string, query string, limit int) ([]MessageHit, error) {
	if len(rooms) == 0 || strings.TrimSpace(query) == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	limit = min(limit, 100)
	seen := map[string]bool{}
	var terms []string
	for _, term := range strings.Fields(strings.ToLower(query)) {
		if term != "" && !seen[term] {
			seen[term] = true
			terms = append(terms, term)
			if len(terms) == 8 {
				break
			}
		}
	}
	if len(terms) == 0 {
		terms = []string{strings.ToLower(strings.TrimSpace(query))}
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",")
	args := make([]any, 0, len(rooms)+len(terms)+1)
	for _, room := range rooms {
		args = append(args, room)
	}
	like := make([]string, 0, len(terms))
	for _, term := range terms {
		like = append(like, "LOWER(content) LIKE ? ESCAPE '!'")
		args = append(args, "%"+escapeLike(term)+"%")
	}
	scanLimit := min(5000, max(200, limit*100))
	args = append(args, scanLimit)
	rows, err := s.DB.DB.Query(`SELECT observation_id, room, source_room, actor_label, actor_account, content, parsed_time
		FROM memory_observations WHERE room IN (`+ph+`) AND deleted_at='' AND (`+strings.Join(like, " OR ")+`)
		ORDER BY parsed_time DESC, observation_id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	phrase := strings.ToLower(strings.TrimSpace(query))
	var out []MessageHit
	for rows.Next() {
		var m corpus.Message
		if err := rows.Scan(&m.ObservationID, &m.Room, &m.RoomOriginal, &m.ActorLabel, &m.ActorAccount, &m.Content, &m.ParsedTime); err != nil {
			return nil, err
		}
		body := strings.ToLower(m.Content)
		score := 0
		for _, term := range terms {
			score += strings.Count(body, term)
		}
		if phrase != "" && strings.Contains(body, phrase) {
			score += 10
		}
		m.ActorAccount = ""
		out = append(out, MessageHit{Message: m, Score: score})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		if out[i].Message.ParsedTime != out[j].Message.ParsedTime {
			return out[i].Message.ParsedTime > out[j].Message.ParsedTime
		}
		return out[i].Message.ObservationID < out[j].Message.ObservationID
	})
	return out[:min(limit, len(out))], nil
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "!", "!!")
	s = strings.ReplaceAll(s, "%", "!%")
	return strings.ReplaceAll(s, "_", "!_")
}

// LeafID is the stable leaf key: hash(recipe version, room, first observation id). Appending to a
// room's open tail keeps the ID; the body hash says whether its vector must be recomputed.
func LeafID(recipe, room, firstObservation string) string {
	sum := sha256.Sum256([]byte(recipe + "\x1f" + room + "\x1f" + firstObservation))
	return "lf_" + hex.EncodeToString(sum[:])[:32]
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// StoredLeaf is one persisted leaf.
type StoredLeaf struct {
	LeafID         string
	Room           string
	ObservationIDs []string
	Body           string
	BodyHash       string
	Speakers       []string
	EndTime        string
	Closed         bool
	Summary        string
	EmbedSpace     string
	LeafVec        []byte
	SummaryVec     []byte
	// LeafLen and SummaryLen are the vectors' sizes in bytes (LoadLeafMeta leaves the vectors out)
	LeafLen, SummaryLen int
}

// SyncStats reports one room's leaf synchronization.
type SyncStats struct {
	Room          string `json:"room"`
	Leaves        int    `json:"leaves"`
	New           int    `json:"new"`
	Changed       int    `json:"changed"`
	Removed       int    `json:"removed"`
	Closed        int    `json:"closed"`
	Messages      int    `json:"messages"`
	RecipeChanged bool   `json:"recipe_changed,omitempty"`
}

// SyncLeaves recuts a room's leaves with the recipe and reconciles them with stored rows: new leaves
// are inserted, leaves whose body changed lose their vectors (recomputed later), vanished leaves are
// deleted. The last leaf is "closed" only when full or idle past the gap — open tails are stored but
// not embedded, so a live channel does not re-embed on every message. A Kakao export's tail is closed
// at once (leafRoomState.archived).
//
// WHY incremental: re-reading and sorting a large room on every small capture
// wastes SD-card I/O and a room-sized allocation even when embeddings are reused.
// A durable checkpoint proves an unchanged prefix only for strict appends (the
// exact corpus tuple order), then the same cutter restarts at the penultimate
// leaf. That boundary preserves fixed-window overlap, time gaps and a previously
// idle-closed or archived tail; IDs still use the first observation, not offsets.
// Late arrivals, edits/deletions, recipe/rendering or declared timezone changes, first
// syncs and interrupted batches use a full cut. Correctness takes priority over
// optimizing those rarer cases. No message bodies or identifiers are logged.
func (s *Store) SyncLeaves(room string, r leaves.Recipe, nowT time.Time, naiveTZ *time.Location) (SyncStats, error) {
	s.syncMu.Lock()
	defer s.syncMu.Unlock()
	var snapshot leafRoomState
	if err := s.DB.Reader().QueryRow(leafRoomSnapshotQuery, room).Scan(&snapshot.revision, &snapshot.recipe, &snapshot.platform, &snapshot.evidenceRevision, &snapshot.naiveTimezone); err != nil {
		return SyncStats{}, err
	}
	msgs, plan, err := s.leafSyncMessages(room, r, naiveTZ, snapshot)
	if err != nil {
		return SyncStats{}, err
	}
	return s.syncLeavesFrom(room, r, nowT, naiveTZ, msgs, plan, snapshot)
}

type leafRoomState struct {
	revision         int64
	recipe           string
	platform         string
	evidenceRevision int64
	naiveTimezone    string
}

const leafRoomSnapshotQuery = `SELECT revision,active_recipe,platform,evidence_revision,naive_timezone FROM memory_rooms WHERE room=?`

// archived reports a room imported from a finished export (KakaoTalk): nothing arrives later to close
// its tail by idling, so the tail closes now. A later, longer export changes the tail's body, and a
// changed leaf is embedded again.
func (l leafRoomState) archived() bool { return l.platform == "kakao" }

func (s *Store) syncLeaves(room string, r leaves.Recipe, nowT time.Time, naiveTZ *time.Location, msgs []corpus.Message, expected ...leafRoomState) (SyncStats, error) {
	return s.syncLeavesFrom(room, r, nowT, naiveTZ, msgs, leafSyncPlan{}, expected...)
}

func (s *Store) syncLeavesFrom(room string, r leaves.Recipe, nowT time.Time, naiveTZ *time.Location, msgs []corpus.Message, plan leafSyncPlan, expected ...leafRoomState) (SyncStats, error) {
	var err error
	st := SyncStats{Room: room, Messages: plan.prefixMessages + len(msgs), Closed: plan.prefixLeaves}
	if len(expected) == 0 {
		var snapshot leafRoomState
		if err := s.DB.Reader().QueryRow(leafRoomSnapshotQuery, room).Scan(&snapshot.revision, &snapshot.recipe, &snapshot.platform, &snapshot.evidenceRevision, &snapshot.naiveTimezone); err != nil {
			return st, err
		}
		expected = []leafRoomState{snapshot}
	}
	cut := leaves.SlicePerRoom(msgs, r, 1<<31-1, "leaf")
	existing := map[string]string{} // leaf_id → body_hash
	// Only the checkpoint's suffix participates in incremental reconciliation.
	q, args := leafSyncExistingQuery(room, r.Version(), plan)
	rows, err := s.DB.Reader().Query(q, args...)
	if err != nil {
		return st, err
	}
	for rows.Next() {
		var id, h string
		if err := rows.Scan(&id, &h); err != nil {
			rows.Close()
			return st, err
		}
		existing[id] = h
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return st, err
	}
	if err := s.DB.WithWriter(func(tx *sql.Tx) error { return s.beginLeafSync(tx, room, expected[0], plan) }); err != nil {
		return st, err
	}
	keep := map[string]bool{}
	// A room's first sync after a large import writes a leaf per 8 messages: committed in batches, it
	// stays under TiDB's transaction size limit and never holds the writer for long. A sync interrupted
	// between batches is completed by the next one (every write here is idempotent).
	for lo := 0; lo < len(cut); lo += syncBatch {
		hi := min(len(cut), lo+syncBatch)
		err = s.DB.WithWriter(func(tx *sql.Tx) error {
			for i := lo; i < hi; i++ {
				l := cut[i]
				for _, m := range msgs[l.Start:l.End] {
					var text, label, at, deleted string
					if err := tx.QueryRow(`SELECT content,actor_label,parsed_time,deleted_at FROM memory_observations WHERE observation_id=?`, m.ObservationID).Scan(&text, &label, &at, &deleted); err != nil {
						return err
					}
					if deleted != "" || strings.TrimSpace(text) != m.Content || label != m.ActorLabel || at != m.ParsedTime {
						return errors.New("memory source changed during leaf sync; retry")
					}
				}
				id := LeafID(r.Version(), room, l.ObservationIDs[0])
				keep[id] = true
				h := hashText(l.Body)
				last := msgs[l.End-1]
				closed := i < len(cut)-1 || l.N >= r.Size || expected[0].archived()
				if !closed && r.Split == leaves.SplitTimeGap {
					closed = idleFor(last.ParsedTime, nowT, naiveTZ) >= time.Duration(r.GapMinutes*float64(time.Minute))
				}
				if closed {
					st.Closed++
				}
				ids, _ := json.Marshal(l.ObservationIDs)
				sp, _ := json.Marshal(l.Speakers)
				old, had := existing[id]
				switch {
				case !had:
					st.New++
					if _, err := tx.Exec(`INSERT INTO memory_leaves(leaf_id, room, recipe, first_observation_id, observation_ids, n, body, body_hash, speakers,
						end_time, closed, summary, updated_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
						id, room, r.Version(), l.ObservationIDs[0], string(ids), l.N, l.Body, h, string(sp), last.ParsedTime, b2i(closed), "", now()); err != nil {
						return err
					}
				case old != h:
					st.Changed++
					if _, err := tx.Exec(`UPDATE memory_leaves SET observation_ids=?, n=?, body=?, body_hash=?, speakers=?, end_time=?, closed=?,
						summary='', leaf_vec=NULL, summary_vec=NULL, updated_at=? WHERE leaf_id=?`,
						string(ids), l.N, l.Body, h, string(sp), last.ParsedTime, b2i(closed), now(), id); err != nil {
						return err
					}
				default:
					// Equal rendered text need not mean equal provenance (identical
					// messages can replace each other). Refresh membership and time
					// without discarding vectors whose embedding text is unchanged.
					if _, err := tx.Exec(`UPDATE memory_leaves SET observation_ids=?,n=?,speakers=?,end_time=?,closed=? WHERE leaf_id=?
						AND (observation_ids<>? OR n<>? OR speakers<>? OR end_time<>? OR closed<>?)`,
						string(ids), l.N, string(sp), last.ParsedTime, b2i(closed), id,
						string(ids), l.N, string(sp), last.ParsedTime, b2i(closed)); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			return st, err
		}
	}
	var gone []string
	for id := range existing {
		if !keep[id] {
			gone = append(gone, id)
		}
	}
	for lo := 0; lo < len(gone); lo += syncBatch {
		hi := min(len(gone), lo+syncBatch)
		err = s.DB.WithWriter(func(tx *sql.Tx) error {
			for _, id := range gone[lo:hi] {
				if _, err := tx.Exec(`DELETE FROM memory_leaves WHERE leaf_id=?`, id); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return st, err
		}
		st.Removed += hi - lo
	}
	// Other recipes remain archived for content-addressed vector reuse. The
	// replacement is invisible until all batches succeed and this pointer swaps.
	err = s.DB.WithWriter(func(tx *sql.Tx) error {
		var revision int64
		var active string
		if err := tx.QueryRow(`SELECT revision,active_recipe FROM memory_rooms WHERE room=?`, room).Scan(&revision, &active); err != nil {
			return err
		}
		if revision != expected[0].revision || active != expected[0].recipe {
			return errors.New("memory source changed during leaf sync; retry")
		}
		if _, err := tx.Exec(`UPDATE memory_rooms SET pending_sync=0 WHERE room=?`, room); err != nil {
			return err
		}
		st.RecipeChanged = active != r.Version()
		if !st.RecipeChanged {
			return s.finishLeafSync(tx, room, r, naiveTZ, expected[0], plan, msgs, cut)
		}
		res, err := tx.Exec(`UPDATE memory_rooms SET active_recipe=? WHERE room=? AND revision=? AND active_recipe=?`, r.Version(), room, revision, active)
		if err != nil {
			return err
		}
		if n, err := res.RowsAffected(); err != nil {
			return err
		} else if n != 1 {
			return errors.New("memory source changed during leaf sync; retry")
		}
		// WHY publish the checkpoint last: the recipe guard must see valid=0
		// throughout our own activation, just as it does throughout leaf writes.
		return s.finishLeafSync(tx, room, r, naiveTZ, expected[0], plan, msgs, cut)
	})
	st.Leaves = plan.prefixLeaves + len(cut)
	return st, err
}

// syncBatch is how many leaf writes SyncLeaves commits per transaction.
const syncBatch = 1000

// idleFor is how long ago a record was written. Naive times are placed in the room's declared zone;
// without one they get 14h of slack (the widest zone offset), so a tail closes late rather than early.
func idleFor(parsed string, nowT time.Time, naiveTZ *time.Location) time.Duration {
	t, aware, ok := corpus.ParseTime(parsed)
	if !ok {
		return 0
	}
	if !aware {
		if naiveTZ != nil {
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), naiveTZ)
		} else {
			return nowT.Sub(t) - 14*time.Hour
		}
	}
	return nowT.Sub(t)
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// PendingLeaf is a closed leaf still missing a summary and/or vectors.
type PendingLeaf struct {
	LeafID     string
	Body       string
	Summary    string
	NeedLeaf   bool
	NeedSumm   bool
	NeedSumVec bool
	// RefreshLeaf bypasses same-space caches while repairing a changed provider width.
	RefreshLeaf bool
}

// Pending lists closed leaves of the rooms that still need work for this embedding space.
func (s *Store) Pending(rooms []string, space string, withSummaries bool, limit int, recipe ...string) ([]PendingLeaf, error) {
	return s.PendingAtWidth(rooms, space, withSummaries, limit, 0, recipe...)
}

// PendingAtWidth also queues vectors unlike the target leaf width. Zero preserves usual pending rules.
func (s *Store) PendingAtWidth(rooms []string, space string, withSummaries bool, limit, dims int, recipe ...string) ([]PendingLeaf, error) {
	return s.pendingAtWidth(rooms, space, withSummaries, limit, dims, nil, recipe...)
}

// PendingAmong is PendingAtWidth restricted to the leaves named in among, without a limit: the pending
// work of a set chosen earlier (a batch preparation's workset). Other leaves are skipped as they are read.
func (s *Store) PendingAmong(rooms []string, space string, withSummaries bool, among map[string]bool, dims int, recipe ...string) ([]PendingLeaf, error) {
	if among == nil {
		among = map[string]bool{}
	}
	return s.pendingAtWidth(rooms, space, withSummaries, 0, dims, among, recipe...)
}

func (s *Store) pendingAtWidth(rooms []string, space string, withSummaries bool, limit, dims int, among map[string]bool, recipe ...string) ([]PendingLeaf, error) {
	if len(rooms) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",")
	needLeaf := `leaf_vec IS NULL OR embed_space<>?`
	args := []any{space}
	refresh := `0`
	if dims > 0 {
		needLeaf += ` OR length(leaf_vec)<>?`
		args = append(args, 2*dims)
		refresh = `leaf_vec IS NOT NULL AND embed_space=? AND length(leaf_vec)<>?`
		args = append(args, space, 2*dims)
	}
	for _, r := range rooms {
		args = append(args, r)
	}
	// a summary vector of another length than the leaf's is from an earlier dimensionality
	q := `SELECT leaf_id, body, summary, ` + needLeaf + `, ` + refresh + `,
		CASE WHEN summary_vec IS NULL OR length(summary_vec)<>length(leaf_vec) THEN 1 ELSE 0 END
		FROM memory_leaves WHERE ` + activeLeaves + ` AND closed=1 AND room IN (` + ph + `) AND (` + needLeaf
	args = append(args, space)
	if dims > 0 {
		args = append(args, 2*dims)
	}
	if withSummaries {
		q += ` OR summary='' OR summary_vec IS NULL OR length(summary_vec)<>length(leaf_vec)`
	}
	q += `)`
	if len(recipe) > 0 {
		q += ` AND recipe=?`
		args = append(args, recipe[0])
	}
	q += ` ORDER BY room, leaf_id`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	rows, err := s.DB.DB.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingLeaf
	for rows.Next() {
		var p PendingLeaf
		var needLeaf, needSumVec bool
		if err := rows.Scan(&p.LeafID, &p.Body, &p.Summary, &needLeaf, &p.RefreshLeaf, &needSumVec); err != nil {
			return nil, err
		}
		if among != nil && !among[p.LeafID] {
			continue
		}
		p.NeedLeaf = needLeaf
		p.NeedSumm = withSummaries && p.Summary == ""
		p.NeedSumVec = withSummaries && (needSumVec || p.NeedSumm)
		out = append(out, p)
	}
	return out, rows.Err()
}

// SaveLeafVector stores a leaf vector (float16) for the space. A vector from another space is moved to
// the embedding cache first (switching the model back reuses it without paying again), and the
// leaf's summary vector, which belongs to the old space, is dropped so the next pass re-embeds it.
func (s *Store) SaveLeafVector(id, space string, vec []byte, expectedBody ...string) error {
	return s.DB.WithWriter(func(tx *sql.Tx) error {
		_, err := s.saveLeafVectorTx(tx, id, space, vec, expectedBody)
		return err
	})
}

// saveLeafVectorTx is SaveLeafVector in tx; written is false when the leaf is gone or its text changed.
func (s *Store) saveLeafVectorTx(tx *sql.Tx, id, space string, vec []byte, expectedBody []string) (written bool, err error) {
	if len(expectedBody) > 0 {
		ok, err := leafSnapshotCurrent(tx, id, expectedBody)
		if err != nil || !ok {
			return false, err
		}
	}
	var old, body, summary string
	var oldVec, oldSum []byte
	q := `SELECT embed_space, body, summary, leaf_vec, summary_vec FROM memory_leaves WHERE leaf_id=?`
	if s.tidb() {
		q += ` FOR UPDATE`
	}
	err = tx.QueryRow(q, id).
		Scan(&old, &body, &summary, &oldVec, &oldSum)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if old != space {
		archive := map[string][]byte{}
		if old != "" && len(oldVec) > 0 {
			archive[CacheKey(old, "document", body)] = oldVec
		}
		if old != "" && summary != "" && len(oldSum) > 0 {
			archive[CacheKey(old, "document", summary)] = oldSum
		}
		if err := s.putCachedTx(tx, archive, ""); err != nil {
			return false, err
		}
		oldSum = nil
	}
	if _, err = tx.Exec(`UPDATE memory_leaves SET leaf_vec=?, summary_vec=?, embed_space=?, updated_at=? WHERE leaf_id=?`, vec, oldSum, space, now(), id); err != nil {
		return false, err
	}
	return true, nil
}

// SharedLeaf is another leaf with the same text that already has vectors in the space.
type SharedLeaf struct {
	LeafVec    []byte
	Summary    string
	SummaryVec []byte
}

// LeafWithBody finds a leaf of the same text embedded in the space (a leaf re-cut by another recipe or
// re-imported): its vectors are reused instead of embedding the text again.
func (s *Store) LeafWithBody(body, space, except string) (SharedLeaf, bool) {
	var l SharedLeaf
	err := s.DB.DB.QueryRow(`SELECT leaf_vec, summary, summary_vec FROM memory_leaves
		WHERE body_hash=? AND embed_space=? AND leaf_vec IS NOT NULL AND leaf_id<>? LIMIT 1`, hashText(body), space, except).
		Scan(&l.LeafVec, &l.Summary, &l.SummaryVec)
	return l, err == nil && len(l.LeafVec) > 0
}

// ShrinkSpace moves leaves embedded in a larger space of the same model to the target space by
// keeping each vector's first components (Matryoshka/MRL, the same truncation fitDims applies to fresh
// embeddings), so lowering models.embed.dimensions needs no paid re-embedding. It returns the leaves
// moved.
func (s *Store) ShrinkSpace(from, to string, shrink func([]byte) []byte) (int, error) {
	if from == to {
		return 0, nil
	}
	moved := 0
	for {
		rows, err := s.DB.DB.Query(`SELECT leaf_id, leaf_vec, summary_vec FROM memory_leaves
			WHERE embed_space=? AND leaf_vec IS NOT NULL LIMIT 500`, from)
		if err != nil {
			return moved, err
		}
		type upd struct {
			id       string
			leaf, sm []byte
		}
		var batch []upd
		for rows.Next() {
			var u upd
			if err := rows.Scan(&u.id, &u.leaf, &u.sm); err != nil {
				rows.Close()
				return moved, err
			}
			u.leaf = shrink(u.leaf)
			if len(u.sm) > 0 {
				u.sm = shrink(u.sm)
			}
			batch = append(batch, u)
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return moved, err
		}
		if len(batch) == 0 {
			return moved, nil
		}
		err = s.DB.WithWriter(func(tx *sql.Tx) error {
			for _, u := range batch {
				if _, err := tx.Exec(`UPDATE memory_leaves SET leaf_vec=?, summary_vec=?, embed_space=?, updated_at=? WHERE leaf_id=? AND embed_space=?`,
					u.leaf, u.sm, to, now(), u.id, from); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return moved, err
		}
		moved += len(batch)
	}
}

// Spaces lists the embedding spaces leaves are stored in.
func (s *Store) Spaces() ([]string, error) {
	rows, err := s.DB.Reader().Query(`SELECT DISTINCT embed_space FROM memory_leaves WHERE embed_space<>''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sp string
		if err := rows.Scan(&sp); err != nil {
			return nil, err
		}
		out = append(out, sp)
	}
	return out, rows.Err()
}

// SaveSummary stores a summary and (optionally) its vector.
func (s *Store) SaveSummary(id, summary string, vec []byte, expectedBody ...string) error {
	return s.DB.WithWriter(func(tx *sql.Tx) error {
		_, err := saveSummaryTx(tx, id, summary, vec, expectedBody)
		return err
	})
}

// saveSummaryTx is SaveSummary in tx; written is false when the leaf is gone or its text changed.
func saveSummaryTx(tx *sql.Tx, id, summary string, vec []byte, expectedBody []string) (written bool, err error) {
	ok, err := leafSnapshotCurrent(tx, id, expectedBody)
	if err != nil || !ok {
		return false, err
	}
	if _, err = tx.Exec(`UPDATE memory_leaves SET summary=?, summary_vec=?, updated_at=? WHERE leaf_id=?`, summary, vec, now(), id); err != nil {
		return false, err
	}
	return true, nil
}

// LoadLeaves returns every leaf of the rooms that has a vector in the space (the searchable set).
func (s *Store) LoadLeaves(rooms []string, space string, recipe ...string) ([]StoredLeaf, error) {
	if len(rooms) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",")
	args := []any{space}
	for _, r := range rooms {
		args = append(args, r)
	}
	filter := activeLeaves
	if len(recipe) > 0 {
		filter += ` AND recipe=?`
		args = append(args, recipe[0])
	}
	rows, err := s.DB.Reader().Query(`SELECT leaf_id, room, observation_ids, body, body_hash, speakers, end_time, closed, summary, embed_space, leaf_vec, summary_vec
		FROM memory_leaves WHERE leaf_vec IS NOT NULL AND embed_space=? AND room IN (`+ph+`) AND `+filter, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredLeaf
	for rows.Next() {
		var l StoredLeaf
		var ids, sp string
		var closed int
		if err := rows.Scan(&l.LeafID, &l.Room, &ids, &l.Body, &l.BodyHash, &sp, &l.EndTime, &closed, &l.Summary, &l.EmbedSpace, &l.LeafVec, &l.SummaryVec); err != nil {
			return nil, err
		}
		l.Closed = closed == 1
		_ = json.Unmarshal([]byte(ids), &l.ObservationIDs)
		_ = json.Unmarshal([]byte(sp), &l.Speakers)
		out = append(out, l)
	}
	return out, rows.Err()
}

// LoadLeafMeta is LoadLeaves without the vectors: rebuilding the index fetches only the vectors it
// does not already hold (LeafVectors), instead of reading every vector of the history again, which
// over a network (TiDB) is hundreds of MB per rebuild for a long history.
func (s *Store) LoadLeafMeta(rooms []string, space string, recipe ...string) ([]StoredLeaf, error) {
	if len(rooms) == 0 {
		return nil, nil
	}
	ph := strings.TrimSuffix(strings.Repeat("?,", len(rooms)), ",")
	args := []any{space}
	for _, r := range rooms {
		args = append(args, r)
	}
	filter := activeLeaves
	if len(recipe) > 0 {
		filter += ` AND recipe=?`
		args = append(args, recipe[0])
	}
	rows, err := s.DB.Reader().Query(`SELECT leaf_id, room, observation_ids, body, body_hash, speakers, end_time, closed, summary, embed_space,
		length(leaf_vec), COALESCE(length(summary_vec), 0)
		FROM memory_leaves WHERE leaf_vec IS NOT NULL AND embed_space=? AND room IN (`+ph+`) AND `+filter, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StoredLeaf
	for rows.Next() {
		var l StoredLeaf
		var ids, sp string
		var closed int
		if err := rows.Scan(&l.LeafID, &l.Room, &ids, &l.Body, &l.BodyHash, &sp, &l.EndTime, &closed, &l.Summary, &l.EmbedSpace,
			&l.LeafLen, &l.SummaryLen); err != nil {
			return nil, err
		}
		l.Closed = closed == 1
		_ = json.Unmarshal([]byte(ids), &l.ObservationIDs)
		_ = json.Unmarshal([]byte(sp), &l.Speakers)
		out = append(out, l)
	}
	return out, rows.Err()
}

// LeafVectors returns the leaf and summary vectors of the given leaves, keyed by leaf ID.
func (s *Store) LeafVectors(ids []string) (map[string][2][]byte, error) {
	out := make(map[string][2][]byte, len(ids))
	for lo := 0; lo < len(ids); lo += 500 {
		chunk := ids[lo:min(len(ids), lo+500)]
		args := make([]any, len(chunk))
		for i, id := range chunk {
			args[i] = id
		}
		ph := strings.TrimSuffix(strings.Repeat("?,", len(chunk)), ",")
		rows, err := s.DB.Reader().Query(`SELECT leaf_id, leaf_vec, summary_vec FROM memory_leaves WHERE leaf_id IN (`+ph+`)`, args...)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			var lv, sv []byte
			if err := rows.Scan(&id, &lv, &sv); err != nil {
				rows.Close()
				return nil, err
			}
			out[id] = [2][]byte{lv, sv}
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

// CacheKey is the embedding cache key for (model, kind, text).
func CacheKey(model, kind, text string) string {
	return hashText(model + "\x1f" + kind + "\x1f" + text)
}

// CachedVector returns a cached float16 vector.
func (s *Store) CachedVector(key string) ([]byte, bool) {
	var v []byte
	if err := s.DB.DB.QueryRow(`SELECT vec FROM memory_embed_cache WHERE cache_key=?`, key).Scan(&v); err != nil {
		return nil, false
	}
	return v, true
}

// PutCachedVectors stores float16 leaf/summary vectors in the cache (existing keys kept).
func (s *Store) PutCachedVectors(items map[string][]byte) error { return s.putCached(items, "") }

// PutQueryVectors caches question embeddings. They grow with every new question, so retention prunes
// them by age (Retention.QueryCacheDays).
func (s *Store) PutQueryVectors(items map[string][]byte) error { return s.putCached(items, "query") }

func (s *Store) putCached(items map[string][]byte, kind string) error {
	if len(items) == 1 { // one question's vector: a single statement, no transaction round trips
		q := `INSERT INTO memory_embed_cache(cache_key, vec, created_at, kind) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`
		if s.tidb() {
			q = `INSERT IGNORE INTO memory_embed_cache(cache_key, vec, created_at, kind) VALUES(?,?,?,?)`
		}
		for k, v := range items {
			_, err := s.DB.DB.Exec(q, k, v, now(), kind)
			return err
		}
	}
	return s.DB.WithWriter(func(tx *sql.Tx) error { return s.putCachedTx(tx, items, kind) })
}

func (s *Store) putCachedTx(tx *sql.Tx, items map[string][]byte, kind string) error {
	if len(items) == 0 {
		return nil
	}
	q := `INSERT INTO memory_embed_cache(cache_key, vec, created_at, kind) VALUES(?,?,?,?) ON CONFLICT DO NOTHING`
	if s.tidb() {
		q = `INSERT IGNORE INTO memory_embed_cache(cache_key, vec, created_at, kind) VALUES(?,?,?,?)`
	}
	stmt, err := tx.Prepare(q)
	if err != nil {
		return err
	}
	defer stmt.Close()
	ts := now()
	for k, v := range items {
		if _, err := stmt.Exec(k, v, ts, kind); err != nil {
			return err
		}
	}
	return nil
}

// DropCachedDuplicates removes embedding-cache entries that only repeat a vector a leaf already
// stores. Earlier versions cached every leaf and summary vector as well as storing it on the leaf,
// which doubled the vectors on disk; model(space) names the model an entry was keyed by. It works
// through the leaves in batches, calling pause between them, and returns the entries removed.
func (s *Store) DropCachedDuplicates(model func(space string) string, pause func() error) (int64, error) {
	var removed int64
	after := ""
	for {
		rows, err := s.DB.DB.Query(`SELECT leaf_id, embed_space, body, summary, length(leaf_vec), COALESCE(length(summary_vec), 0)
			FROM memory_leaves WHERE leaf_vec IS NOT NULL AND leaf_id > ? ORDER BY leaf_id LIMIT 1000`, after)
		if err != nil {
			return removed, err
		}
		type dup struct {
			key string
			n   int
		}
		var dups []dup
		n := 0
		for rows.Next() {
			var space, body, summary string
			var ln, sn int
			if err := rows.Scan(&after, &space, &body, &summary, &ln, &sn); err != nil {
				rows.Close()
				return removed, err
			}
			n++
			m := model(space)
			dups = append(dups, dup{CacheKey(m, "document", body), ln})
			if summary != "" && sn > 0 {
				dups = append(dups, dup{CacheKey(m, "document", summary), sn})
			}
		}
		rows.Close()
		if err := rows.Err(); err != nil {
			return removed, err
		}
		if n == 0 {
			return removed, nil
		}
		err = s.DB.WithWriter(func(tx *sql.Tx) error {
			for _, d := range dups {
				// same length: the entry is this vector, not one kept from another dimensionality
				res, err := tx.Exec(`DELETE FROM memory_embed_cache WHERE cache_key=? AND kind='' AND length(vec)=?`, d.key, d.n)
				if err != nil {
					return err
				}
				k, _ := res.RowsAffected()
				removed += k
			}
			return nil
		})
		if err != nil {
			return removed, err
		}
		if err := pause(); err != nil {
			return removed, err
		}
	}
}

// Counts summarizes the memory tables. The leaf numbers come from one pass over the leaves: the
// dashboard and the doctor both ask, and each pass reads every leaf row.
func (s *Store) Counts() (map[string]int, error) {
	out := map[string]int{}
	var n, closed, embedded, summaries int
	if err := s.DB.Reader().QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN closed=1 THEN 1 ELSE 0 END),0),
		COALESCE(SUM(CASE WHEN leaf_vec IS NOT NULL THEN 1 ELSE 0 END),0), COALESCE(SUM(CASE WHEN summary<>'' THEN 1 ELSE 0 END),0)
		FROM memory_leaves WHERE `+activeLeaves).Scan(&n, &closed, &embedded, &summaries); err != nil {
		return nil, err
	}
	out["leaves"], out["leaves_closed"], out["leaves_embedded"], out["summaries"] = n, closed, embedded, summaries
	for name, q := range map[string]string{
		"observations": `SELECT COUNT(*) FROM memory_observations WHERE deleted_at=''`,
		"embed_cache":  `SELECT COUNT(*) FROM memory_embed_cache`,
	} {
		if err := s.DB.Reader().QueryRow(q).Scan(&n); err != nil {
			return nil, err
		}
		out[name] = n
	}
	return out, nil
}

// SpaceCounts returns how many leaves have a vector in total and how many of them are in space (the
// embedding space search reads). After the embedding model changes, the rest wait for re-indexing.
func (s *Store) SpaceCounts(space string) (total, inSpace int, err error) {
	err = s.DB.Reader().QueryRow(`SELECT COUNT(*), COALESCE(SUM(CASE WHEN embed_space=? THEN 1 ELSE 0 END),0) FROM memory_leaves WHERE leaf_vec IS NOT NULL AND `+activeLeaves, space).
		Scan(&total, &inSpace)
	return total, inSpace, err
}

// RoomText is how much embedded text a room holds.
type RoomText struct {
	Leaves int
	Chars  int64
}

// EmbeddedRooms returns, per room, the leaves that have a vector and the characters of their bodies
// and summaries: what another embedding model would have to embed again.
func (s *Store) EmbeddedRooms() (map[string]RoomText, error) {
	rows, err := s.DB.Reader().Query(`SELECT room, COUNT(*), COALESCE(SUM(LENGTH(body)+LENGTH(summary)),0) FROM memory_leaves WHERE leaf_vec IS NOT NULL AND ` + activeLeaves + ` GROUP BY room`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]RoomText{}
	for rows.Next() {
		var room string
		var t RoomText
		if err := rows.Scan(&room, &t.Leaves, &t.Chars); err != nil {
			return nil, err
		}
		out[room] = t
	}
	return out, rows.Err()
}

// AddSpend records provider spend (USD, as reported or estimated) under a kind (chat, embed, rerank,
// summary) for today (UTC).
func (s *Store) AddSpend(kind string, usd float64, calls int) error {
	day := time.Now().UTC().Format("2006-01-02")
	// one atomic statement: no transaction needed (each would cost two more round trips on TiDB)
	q := `INSERT INTO memory_spend(day, kind, usd, calls) VALUES(?,?,?,?) ON CONFLICT(day, kind) DO UPDATE SET usd=memory_spend.usd+excluded.usd, calls=memory_spend.calls+excluded.calls`
	if s.tidb() {
		q = `INSERT INTO memory_spend(day, kind, usd, calls) VALUES(?,?,?,?) ON DUPLICATE KEY UPDATE usd=usd+VALUES(usd), calls=calls+VALUES(calls)`
	}
	_, err := s.DB.DB.Exec(q, day, kind, usd, calls)
	return err
}

// SpendSince sums spend from a day (YYYY-MM-DD, inclusive) by kind.
func (s *Store) SpendSince(day string) (map[string]float64, float64, error) {
	rows, err := s.DB.Reader().Query(`SELECT kind, SUM(usd) FROM memory_spend WHERE day>=? GROUP BY kind`, day)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	out := map[string]float64{}
	total := 0.0
	for rows.Next() {
		var k string
		var v float64
		if err := rows.Scan(&k, &v); err != nil {
			return nil, 0, err
		}
		out[k] = v
		total += v
	}
	return out, total, rows.Err()
}

// ChatTurn is one message of the bot's conversation in a channel (the bot's own replies are not
// memory observations; they are kept here only as conversation context).
type ChatTurn struct {
	Role, Speaker, Content, CreatedAt string
	// NativeID is the Discord request a user or assistant turn came from (empty for lab turns).
	// An edit or deletion of that message changes or removes the turn (EditMessage).
	NativeID string
	// SpeakerID is the account a user turn came from: withdrawing memory consent removes that
	// person's turns (DeleteAccountMessages), in channels that store messages and in those that don't.
	SpeakerID string
	// SourceIDs are the messages and earlier replies read to produce this turn.
	SourceIDs []string
	// DerivedFrom are other messages part of this turn's text was made from (the description of a
	// picture in the message a user turn replied to). Deleting or editing one of them removes the turn,
	// user turns included (holder kind "chat_from").
	DerivedFrom []string
}

// AppendChatTurns commits turns and their source index together, then trims to the last keep turns.
// A trim interrupted by a crash is repeated by the next append.
func (s *Store) AppendChatTurns(channelKey string, turns []ChatTurn, keep int) error {
	if len(turns) > 0 {
		err := s.DB.WithWriter(func(tx *sql.Tx) error {
			for _, t := range turns {
				if !s.tidb() {
					blocked, err := MessageBlocked(tx, t.NativeID, t.SpeakerID)
					if !blocked && err == nil {
						blocked, err = SourcesBlocked(tx, uniqueSources(t.SourceIDs, t.DerivedFrom))
					}
					if err != nil {
						return err
					}
					if blocked {
						continue
					}
				}
				result, err := tx.Exec(`INSERT INTO chat_turns(channel_key,role,speaker,content,created_at,native_id,speaker_id,evidence_state) VALUES(?,?,?,?,?,?,?,1)`, channelKey, t.Role, t.Speaker, t.Content, now(), t.NativeID, t.SpeakerID)
				if err != nil {
					return err
				}
				if !s.tidb() {
					id, err := result.LastInsertId()
					if err != nil {
						return err
					}
					if err := IndexSourceRefs(tx, "chat", "chat:"+fmt.Sprint(id), uniqueSources(t.SourceIDs, []string{t.NativeID, PersonRef(t.SpeakerID)})); err != nil {
						return err
					}
					if len(t.DerivedFrom) > 0 {
						if err := IndexSourceRefs(tx, "chat_from", "chat:"+fmt.Sprint(id), t.DerivedFrom); err != nil {
							return err
						}
					}
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
	}
	if keep > 0 {
		// the derived table lets MySQL/TiDB read the table it deletes from
		if _, err := s.DB.DB.Exec(`DELETE FROM chat_turns WHERE channel_key=? AND id <= (SELECT id FROM (SELECT id FROM chat_turns
			WHERE channel_key=? ORDER BY id DESC LIMIT 1 OFFSET ?) AS cutoff)`, channelKey, channelKey, keep); err != nil {
			return err
		}
	}
	return nil
}

// RecentChatTurns returns the last n turns of a channel in order.
func (s *Store) RecentChatTurns(channelKey string, n int) ([]ChatTurn, error) {
	if s.DB.HasPendingEvents("source-change") {
		return nil, nil
	}

	rows, err := s.DB.DB.Query(`SELECT role,speaker,content,created_at,native_id,speaker_id,COALESCE((SELECT json_group_array(source_id) FROM evidence_refs WHERE holder_kind='chat' AND holder_id='chat:'||t.id),'[]') FROM chat_turns t WHERE channel_key=? ORDER BY id DESC LIMIT ?`, channelKey, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatTurn
	for rows.Next() {
		var t ChatTurn
		var refs string
		if err := rows.Scan(&t.Role, &t.Speaker, &t.Content, &t.CreatedAt, &t.NativeID, &t.SpeakerID, &refs); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(refs), &t.SourceIDs)
		t.SourceIDs = append(t.SourceIDs, t.NativeID, PersonRef(t.SpeakerID))
		out = append(out, t)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// LeafVectorWidths reads the range of active float16 widths without fetching vector BLOBs or
// allocating the history's metadata. Both are zero before the first embedded leaf.
func (s *Store) LeafVectorWidths(space, recipe string) (lo, hi int, err error) {
	err = s.DB.Reader().QueryRow(`SELECT COALESCE(MIN(length(leaf_vec)),0), COALESCE(MAX(length(leaf_vec)),0)
		FROM memory_leaves WHERE embed_space=? AND recipe=? AND `+activeLeaves+`
		AND leaf_vec IS NOT NULL AND length(leaf_vec)>0 AND length(leaf_vec)%2=0`, space, recipe).Scan(&lo, &hi)
	return lo / 2, hi / 2, err
}

// LeafVectorMode chooses the most common active width; ties choose the smaller width consistently.
func (s *Store) LeafVectorMode(space, recipe string) (int, error) {
	var bytes int
	err := s.DB.Reader().QueryRow(`SELECT length(leaf_vec) FROM memory_leaves
		WHERE embed_space=? AND recipe=? AND `+activeLeaves+`
		AND leaf_vec IS NOT NULL AND length(leaf_vec)>0 AND length(leaf_vec)%2=0
		GROUP BY length(leaf_vec) ORDER BY COUNT(*) DESC, length(leaf_vec) LIMIT 1`, space, recipe).Scan(&bytes)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return bytes / 2, err
}

// LeafWidthRepairRooms reads only room names, so background passes can repair quiet rooms too.
func (s *Store) LeafWidthRepairRooms(space, recipe string, dims int) ([]string, error) {
	rows, err := s.DB.Reader().Query(`SELECT DISTINCT room FROM memory_leaves
		WHERE embed_space=? AND recipe=? AND `+activeLeaves+`
		AND closed=1 AND leaf_vec IS NOT NULL AND length(leaf_vec)<>? ORDER BY room`, space, recipe, 2*dims)
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

// LeafVectorDims is the uniform active leaf width. With mixed widths no message space is comparable
// to all leaves, so it returns zero rather than choosing a differently ordered first row.
func (s *Store) LeafVectorDims(space, recipe string) (int, error) {
	lo, hi, err := s.LeafVectorWidths(space, recipe)
	if lo != hi {
		return 0, err
	}
	return lo, err
}
