// Package store persists memory: raw observations (append-only, deduplicated), leaves with their
// summaries and float16 vectors, and a content-addressed embedding cache. It is the durable side of
// internal/memory/search, which is rebuilt in memory from these rows.
//
// Raw source files are never modified; bodies stay in the instance DB and are never logged.
package store

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"masamong/internal/storage"
)

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
	// Now is the clock the stored times come from; nil is the wall clock. The
	// product sets it from the service clock so a fixed clock lands in the rows.
	Now func() time.Time
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
	// message vectors made before the label column existed get an empty label and stay as they are (no
	// start sends the history to the models again); the format column marks float16 rows of the first
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
		{"chat_turns", "spoken_at", `TEXT NOT NULL DEFAULT ''`, `VARCHAR(64) NOT NULL DEFAULT ''`},
		{"chat_turns", "delivery_turn", `TEXT NOT NULL DEFAULT ''`, `VARCHAR(64) NOT NULL DEFAULT ''`},
		{"chat_turns", "delivery_spans", `TEXT`, `TEXT`},
		{"chat_turns", "call_address", `INTEGER`, `TINYINT`},
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
	if err := migratePersonAliases(db); err != nil {
		return nil, fmt.Errorf("memory migration: person aliases: %w", err)
	}
	return st, nil
}

func now() string { return time.Now().UTC().Format(time.RFC3339) }

// nowT is the store's clock (Now when set, else the wall clock). Stored times
// are UTC RFC3339; day values (memory_spend) are UTC YYYY-MM-DD.
func (s *Store) nowT() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Store) now() string { return s.nowT().UTC().Format(time.RFC3339) }

func (s *Store) day() string { return s.nowT().UTC().Format("2006-01-02") }

func (s *Store) tidb() bool { return s.DB.Backend == storage.BackendTiDB }

func toAny(ss []string) []any {
	out := make([]any, len(ss))
	for i, s := range ss {
		out[i] = s
	}
	return out
}

func escapeLike(s string) string {
	s = strings.ReplaceAll(s, "!", "!!")
	s = strings.ReplaceAll(s, "%", "!%")
	return strings.ReplaceAll(s, "_", "!_")
}

func hashText(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
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

// AddSpend records provider spend (USD, as reported or estimated) under a kind (chat, embed, rerank,
// summary) for today (UTC).
func (s *Store) AddSpend(kind string, usd float64, calls int) error {
	day := s.day()
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
