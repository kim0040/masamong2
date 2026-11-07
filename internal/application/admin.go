package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/catalog"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// The admin layer backs the management console: per-admin UI preferences, the global pause, the turn
// log (what the bot did with each message), the change log (who changed what), time series for charts,
// the RAG lab's review set and a JSON view of the config document that the console edits by path.
// Everything is an Action, so the CLI, the console and tests share one contract.

var adminSQLite = []string{
	snapshotSQLite,
	activitySQLite,
	memoryReviewSQLite,
	evalSQLite,
	`CREATE TABLE IF NOT EXISTS admin_state (
		k TEXT PRIMARY KEY,
		v TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS audit_log (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at TEXT NOT NULL,
		action TEXT NOT NULL,
		via TEXT NOT NULL,
		ok INTEGER NOT NULL,
		code TEXT NOT NULL DEFAULT '',
		rev_before INTEGER NOT NULL DEFAULT 0,
		rev_after INTEGER NOT NULL DEFAULT 0,
		summary TEXT NOT NULL DEFAULT '',
		actor TEXT NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS turn_log (
		id TEXT PRIMARY KEY,
		at TEXT NOT NULL,
		day TEXT NOT NULL,
		hour INTEGER NOT NULL,
		guild_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		outcome TEXT NOT NULL,
		tools TEXT NOT NULL DEFAULT '',
		hits INTEGER NOT NULL DEFAULT 0,
		cost_usd REAL NOT NULL DEFAULT 0,
		latency_ms INTEGER NOT NULL DEFAULT 0,
		lab INTEGER NOT NULL DEFAULT 0,
		body TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_turn_log_at ON turn_log(at)`,
	`CREATE INDEX IF NOT EXISTS idx_turn_log_day ON turn_log(day, outcome)`,
	`CREATE TABLE IF NOT EXISTS lab_reviews (
		id TEXT PRIMARY KEY,
		at TEXT NOT NULL,
		channel_key TEXT NOT NULL,
		question TEXT NOT NULL,
		answer TEXT NOT NULL,
		verdict TEXT NOT NULL,
		tags TEXT NOT NULL DEFAULT '',
		note TEXT NOT NULL DEFAULT '',
		meta TEXT NOT NULL DEFAULT '{}'
	)`,
}

var adminTiDB = []string{
	snapshotTiDB,
	activityTiDB,
	memoryReviewTiDB,
	evalTiDB,
	`CREATE TABLE IF NOT EXISTS admin_state (
		k VARCHAR(128) NOT NULL PRIMARY KEY,
		v MEDIUMTEXT NOT NULL,
		updated_at VARCHAR(64) NOT NULL
	)`,
	`CREATE TABLE IF NOT EXISTS audit_log (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		at VARCHAR(64) NOT NULL,
		action VARCHAR(128) NOT NULL,
		via VARCHAR(32) NOT NULL,
		ok TINYINT NOT NULL,
		code VARCHAR(64) NOT NULL DEFAULT '',
		rev_before INT NOT NULL DEFAULT 0,
		rev_after INT NOT NULL DEFAULT 0,
		summary VARCHAR(1024) NOT NULL DEFAULT '',
		actor VARCHAR(128) NOT NULL DEFAULT ''
	)`,
	`CREATE TABLE IF NOT EXISTS turn_log (
		id VARCHAR(64) NOT NULL PRIMARY KEY,
		at VARCHAR(64) NOT NULL,
		day VARCHAR(10) NOT NULL,
		hour INT NOT NULL,
		guild_id VARCHAR(64) NOT NULL,
		channel_id VARCHAR(64) NOT NULL,
		outcome VARCHAR(32) NOT NULL,
		tools VARCHAR(512) NOT NULL DEFAULT '',
		hits INT NOT NULL DEFAULT 0,
		cost_usd DOUBLE NOT NULL DEFAULT 0,
		latency_ms INT NOT NULL DEFAULT 0,
		lab TINYINT NOT NULL DEFAULT 0,
		body MEDIUMTEXT NOT NULL,
		KEY idx_turn_log_at (at),
		KEY idx_turn_log_day (day, outcome)
	)`,
	`CREATE TABLE IF NOT EXISTS lab_reviews (
		id VARCHAR(64) NOT NULL PRIMARY KEY,
		at VARCHAR(64) NOT NULL,
		channel_key VARCHAR(160) NOT NULL,
		question MEDIUMTEXT NOT NULL,
		answer MEDIUMTEXT NOT NULL,
		verdict VARCHAR(16) NOT NULL,
		tags VARCHAR(512) NOT NULL DEFAULT '',
		note TEXT NOT NULL,
		meta MEDIUMTEXT NOT NULL
	)`,
}

var adminReady sync.Map // *storage.Store → error

// adminDB migrates the admin tables once per store and returns the store (nil before setup).
func (s *Service) adminDB() (*storage.Store, error) {
	if s.Store == nil {
		return nil, errors.New("run setup first")
	}
	if v, ok := adminReady.Load(s.Store); ok {
		if v == nil {
			return s.Store, nil
		}
		return nil, v.(error)
	}
	if err := migrateAdmin(s.Store); err != nil {
		adminReady.Store(s.Store, err)
		return nil, err
	}
	adminReady.Store(s.Store, nil)
	return s.Store, nil
}

func migrateAdmin(st *storage.Store) error {
	stmts := adminSQLite
	if st.Backend == storage.BackendTiDB {
		stmts = adminTiDB
	}
	for _, q := range stmts {
		if _, err := st.DB.Exec(q); err != nil {
			return fmt.Errorf("admin tables: %w", err)
		}
	}
	if err := migrateFeatures(st); err != nil {
		return err
	}
	// columns added after the first release of a table
	for _, c := range []struct{ table, col, sqlite, tidb string }{
		{"audit_log", "actor", "TEXT NOT NULL DEFAULT ''", "VARCHAR(128) NOT NULL DEFAULT ''"},
		{"audit_log", "actor_id", "TEXT NOT NULL DEFAULT ''", "VARCHAR(128) NOT NULL DEFAULT ''"},
		{"audit_log", "guild_id", "TEXT NOT NULL DEFAULT ''", "VARCHAR(64) NOT NULL DEFAULT ''"},
		// whose message a turn answered or a candidate was, as personRef (withdrawn consent)
		{"turn_log", "speaker_ref", "TEXT NOT NULL DEFAULT ''", "VARCHAR(32) NOT NULL DEFAULT ''"},
		{"participation_log", "author_ref", "TEXT NOT NULL DEFAULT ''", "VARCHAR(32) NOT NULL DEFAULT ''"},
		// 0 = not looked at yet, 1 = its sources are in evidence_refs, 2 = it kept copied text (evidence_index.go)
		{"turn_log", "evidence_state", "INTEGER NOT NULL DEFAULT 0", "TINYINT NOT NULL DEFAULT 0"},
		{"participation_log", "evidence_state", "INTEGER NOT NULL DEFAULT 0", "TINYINT NOT NULL DEFAULT 0"},
		{"traces", "evidence_state", "INTEGER NOT NULL DEFAULT 0", "TINYINT NOT NULL DEFAULT 0"},
		{"channel_summary", "evidence_state", "INTEGER NOT NULL DEFAULT 0", "TINYINT NOT NULL DEFAULT 0"},
		// one memory-index ledger row may summarize several provider calls for a job and role
		{"llm_calls", "call_count", "INTEGER NOT NULL DEFAULT 1", "INT NOT NULL DEFAULT 1"},
		// which service answered a tool call (a web search: the one of web_search.order that found the pages)
		{"tool_calls", "provider", "TEXT NOT NULL DEFAULT ''", "VARCHAR(128) NOT NULL DEFAULT ''"},
		// repeating reminders (reminders_repeat.go): the rule as JSON ('' = once), its end, how many were leased,
		// and the occurrence being sent
		{"reminders", "repeat_rule", "TEXT NOT NULL DEFAULT ''", "VARCHAR(512) NOT NULL DEFAULT ''"},
		{"reminders", "repeat_until", "TEXT NOT NULL DEFAULT ''", "VARCHAR(64) NOT NULL DEFAULT ''"},
		{"reminders", "repeat_max", "INTEGER NOT NULL DEFAULT 0", "INT NOT NULL DEFAULT 0"},
		{"reminders", "occurrences", "INTEGER NOT NULL DEFAULT 0", "INT NOT NULL DEFAULT 0"},
		{"reminders", "sending_at", "TEXT NOT NULL DEFAULT ''", "VARCHAR(64) NOT NULL DEFAULT ''"},
		// scheduled tasks (reminders_task.go): kind 'task' runs the stored request as a bot turn when due
		{"reminders", "kind", "TEXT NOT NULL DEFAULT ''", "VARCHAR(16) NOT NULL DEFAULT ''"},
		{"reminders", "task", "TEXT NOT NULL DEFAULT ''", "VARCHAR(2048) NOT NULL DEFAULT ''"},
	} {
		if _, err := st.DB.Exec("SELECT " + c.col + " FROM " + c.table + " LIMIT 0"); err != nil {
			def := c.sqlite
			if st.Backend == storage.BackendTiDB {
				def = c.tidb
			}
			if _, err := st.DB.Exec("ALTER TABLE " + c.table + " ADD COLUMN " + c.col + " " + def); err != nil {
				return fmt.Errorf("admin tables: add %s.%s: %w", c.table, c.col, err)
			}
		}
	}
	for _, q := range []string{`CREATE INDEX IF NOT EXISTS idx_turn_log_speaker ON turn_log(speaker_ref)`,
		`CREATE INDEX IF NOT EXISTS idx_participation_log_author ON participation_log(author_ref)`} {
		if _, err := st.DB.Exec(q); err != nil {
			return fmt.Errorf("admin tables: %w", err)
		}
	}
	return migrateEvidence(st)
}

// kvCache keeps admin_state values read or written within kvFresh, so checks made for every message
// or model call (the pause switch, provider health) do not each cost a database round trip, which on
// a TiDB cluster in another region adds up to seconds per reply. This process's writes are seen at
// once; another process's (a CLI run against the same database) within kvFresh.
type kvCache struct {
	mu sync.Mutex
	m  map[string]kvEntry
}

type kvEntry struct {
	raw string
	ok  bool
	at  time.Time
}

const kvFresh = 2 * time.Second

func (c *kvCache) get(key string) (kvEntry, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.m[key]
	if !ok || time.Since(e.at) > kvFresh {
		return kvEntry{}, false
	}
	return e, true
}

func (c *kvCache) put(key string, e kvEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m == nil || len(c.m) > 4096 {
		c.m = map[string]kvEntry{}
	}
	e.at = time.Now()
	c.m[key] = e
}

func (s *Service) kvGet(key string, into any) (bool, error) {
	if e, ok := s.kv.get(key); ok {
		if !e.ok {
			return false, nil
		}
		return true, json.Unmarshal([]byte(e.raw), into)
	}
	db, err := s.adminDB()
	if err != nil {
		return false, err
	}
	var raw string
	err = db.DB.QueryRow(`SELECT v FROM admin_state WHERE k=?`, key).Scan(&raw)
	if errors.Is(err, sql.ErrNoRows) {
		s.kv.put(key, kvEntry{})
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s.kv.put(key, kvEntry{raw: raw, ok: true})
	return true, json.Unmarshal([]byte(raw), into)
}

func (s *Service) kvDelete(key string) error {
	db, err := s.adminDB()
	if err != nil {
		return err
	}
	_, err = db.DB.Exec(`DELETE FROM admin_state WHERE k=?`, key)
	if err == nil {
		s.kv.put(key, kvEntry{})
	}
	return err
}

func (s *Service) kvSet(key string, v any) error {
	db, err := s.adminDB()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	q := `INSERT INTO admin_state(k, v, updated_at) VALUES(?,?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v, updated_at=excluded.updated_at`
	if db.Backend == storage.BackendTiDB {
		q = `INSERT INTO admin_state(k, v, updated_at) VALUES(?,?,?) ON DUPLICATE KEY UPDATE v=VALUES(v), updated_at=VALUES(updated_at)`
	}
	_, err = db.DB.Exec(q, key, string(raw), now) // one atomic statement: no transaction needed
	if err == nil {
		s.kv.put(key, kvEntry{raw: string(raw), ok: true})
	}
	return err
}

func (s *Service) adminActions(ctx context.Context, req action.Request) action.Result {
	if _, err := s.adminDB(); err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	switch req.Action {
	case "admin.prefs", "admin.prefs.set":
		return s.adminPrefs(req)
	case "runtime.pause", "runtime.status":
		return s.runtimePause(req)
	case "runtime.restart", "runtime.stop":
		return s.runtimeRestart(req)
	case "dashboard.overview":
		return s.dashboardOverview(ctx, req)
	case "turns.list", "turns.get":
		return s.turnsAction(req)
	case "audit.list":
		return s.auditList(req)
	case "stats.series":
		return s.statsSeries(req)
	case "lab.reviews", "lab.review":
		return s.labReviews(req)
	case "config.document":
		return s.configDocument(req)
	case "config.patch":
		return s.configPatch(req)
	case "config.snapshots", "config.snapshot", "config.snapshot.delete", "config.snapshot.restore":
		return s.snapshotsAction(req)
	case "config.changes":
		return s.configChangesAction(req)
	}
	return action.Fail(req, errcode.ActionUnknown, "action.unknown", s.msg(req, "action.unknown"), nil)
}

// ---------------------------------------------------------------------------
// preferences
// ---------------------------------------------------------------------------

// adminID keys per-admin state (preferences): the signed-in console admin, else the owner.
func adminID(req action.Request) string {
	if req.Operator != nil && req.Operator.ID != "" {
		return req.Operator.ID
	}
	return "owner"
}

func (s *Service) nowString() string { return s.Clock.Now().UTC().Format(time.RFC3339) }

// actorName is how the change log names who did something.
func actorName(req action.Request) string {
	if req.Operator != nil {
		if req.Operator.Name != "" {
			return req.Operator.Name
		}
		return req.Operator.ID
	}
	if req.Invoker != nil && req.Invoker.UserID != "" {
		if req.Invoker.Name != "" {
			return req.Invoker.Name
		}
		return req.Invoker.UserID
	}
	return ""
}

// actorID is who made a change, for filtering the change log: a console admin's ID, or
// "discord:<user id>" for a change made from Discord. Empty = the installation owner.
func actorID(req action.Request) string {
	if req.Operator != nil {
		return req.Operator.ID
	}
	if req.Invoker != nil && req.Invoker.UserID != "" {
		return "discord:" + req.Invoker.UserID
	}
	return ""
}

func (s *Service) adminPrefs(req action.Request) action.Result {
	key := "prefs:" + adminID(req)
	prefs := map[string]any{}
	if _, err := s.kvGet(key, &prefs); err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	if req.Action == "admin.prefs.set" {
		in := inputMap(req)
		patch, _ := in["prefs"].(map[string]any)
		if patch == nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "prefs object required", nil)
		}
		raw, _ := json.Marshal(patch)
		if len(raw) > 64<<10 {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "prefs too large", nil)
		}
		for k, v := range patch {
			if v == nil {
				delete(prefs, k)
			} else {
				prefs[k] = v
			}
		}
		if err := s.kvSet(key, prefs); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
	}
	return action.OK(req, map[string]any{"prefs": prefs})
}

// ---------------------------------------------------------------------------
// pause
// ---------------------------------------------------------------------------

// PauseState stops replies and bot-initiated sends. The console, CLI and jobs keep working.
type PauseState struct {
	All      bool            `json:"all"`
	Guilds   map[string]bool `json:"guilds,omitempty"`
	Channels map[string]bool `json:"channels,omitempty"` // guild:channel
	Reason   string          `json:"reason,omitempty"`
	At       string          `json:"at,omitempty"`
}

// ErrChatPaused is returned while an operator has paused replies.
var ErrChatPaused = errors.New("chat: replies are paused (runtime.pause)")

func (s *Service) pauseState() PauseState {
	var st PauseState
	if s.Store == nil {
		return st
	}
	_, _ = s.kvGet("runtime:pause", &st)
	return st
}

// Paused reports whether the bot must stay quiet in this channel.
func (s *Service) Paused(guildID, channelID string) bool {
	st := s.pauseState()
	if st.All || st.Guilds[guildID] || st.Channels[guildID+":"+channelID] {
		return true
	}
	// a paused channel's threads are paused with it
	parent := s.threadParent(channelID)
	return parent != "" && st.Channels[guildID+":"+parent]
}

func (s *Service) runtimePause(req action.Request) action.Result {
	st := s.pauseState()
	if req.Action == "runtime.pause" {
		in := inputMap(req)
		paused := true
		if _, ok := in["paused"]; ok {
			paused = boolVal(in, "paused")
		}
		switch scope := strVal(in, "scope", "all"); scope {
		case "all":
			st.All = paused
		case "guild":
			g := strVal(in, "guild_id", "")
			if g == "" {
				return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id required", nil)
			}
			if st.Guilds == nil {
				st.Guilds = map[string]bool{}
			}
			if paused {
				st.Guilds[g] = true
			} else {
				delete(st.Guilds, g)
			}
		case "channel":
			g, c := strVal(in, "guild_id", ""), strVal(in, "channel_id", "")
			if g == "" || c == "" {
				return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id and channel_id required", nil)
			}
			if st.Channels == nil {
				st.Channels = map[string]bool{}
			}
			if paused {
				st.Channels[g+":"+c] = true
			} else {
				delete(st.Channels, g+":"+c)
			}
		default:
			return action.Fail(req, errcode.InvalidInput, "generic.error", "scope must be all, guild or channel", nil)
		}
		st.Reason = strVal(in, "reason", "")
		st.At = s.Clock.Now().UTC().Format(time.RFC3339)
		if err := s.kvSet("runtime:pause", st); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		s.emit("runtime", "pause", map[string]any{"all": st.All})
	}
	return action.OK(req, map[string]any{"pause": st, "started_at": startedAt.UTC().Format(time.RFC3339),
		"uptime_sec": int(time.Since(startedAt).Seconds()), "version": Version, "revision": s.Revision(),
		"restart_pending": s.restartPending()})
}

var startedAt = time.Now()

// ---------------------------------------------------------------------------
// restart and stop
// ---------------------------------------------------------------------------

// Lifecycle requests are read by the daemon: restart re-executes it (storage switch, restore, token
// change), stop ends it cleanly (the tray's "quit", the console's stop button).
type Lifecycle string

const (
	lifecycleRestart Lifecycle = "restart"
	lifecycleStop    Lifecycle = "stop"
)

var lifecycles sync.Map // *Service → chan Lifecycle

func (s *Service) lifecycleChan() chan Lifecycle {
	v, _ := lifecycles.LoadOrStore(s, make(chan Lifecycle, 1))
	return v.(chan Lifecycle)
}

// LifecycleRequests delivers operator restart/stop requests to the daemon.
func (s *Service) LifecycleRequests() <-chan Lifecycle { return s.lifecycleChan() }

func (s *Service) requestLifecycle(l Lifecycle) {
	select {
	case s.lifecycleChan() <- l:
	default:
	}
	s.emit("runtime", string(l), nil)
}

func (s *Service) restartPending() bool {
	if file, err := storage.LoadSettings(s.DataRoot); err == nil && s.Store != nil {
		if file.Backend != s.Store.Backend {
			return true
		}
		if file.Backend == storage.BackendSQLite && filepath.Join(s.DataRoot, orDefault(file.SQLiteFile, "masamong.db")) != s.Store.Path() {
			return true
		}
	}
	return false
}

func (s *Service) runtimeRestart(req action.Request) action.Result {
	if s.practice {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "not available in practice mode", nil)
	}
	if !s.Daemon {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "the service does not run in this process: use `masamong stop` or `masamong restart`", nil)
	}
	if req.Action == "runtime.stop" {
		s.requestLifecycle(lifecycleStop)
		return action.OK(req, map[string]any{"stopping": true, "message": s.msg(req, "runtime.stopping")})
	}
	s.requestLifecycle(lifecycleRestart)
	return action.OK(req, map[string]any{"restarting": true, "message": s.msg(req, "runtime.restarting")})
}

// ---------------------------------------------------------------------------
// turn log
// ---------------------------------------------------------------------------

// TurnRecord is one entry of the turn log.
type TurnRecord struct {
	ID        string   `json:"id"`
	At        string   `json:"at"`
	GuildID   string   `json:"guild_id"`
	ChannelID string   `json:"channel_id"`
	Outcome   string   `json:"outcome"`
	Tools     []string `json:"tools"`
	Hits      int      `json:"hits"`
	CostUSD   float64  `json:"cost_usd"`
	LatencyMS int      `json:"latency_ms"`
	Lab       bool     `json:"lab"`
	// ChannelName is the channel's current Discord name (lists only; empty when the gateway does not know it)
	ChannelName string         `json:"channel_name,omitempty"`
	Body        map[string]any `json:"body,omitempty"`
	// Feedback is what people said about the reply: {"good": n, "bad": n, "reasons": [...]} (feedback.go)
	Feedback map[string]any `json:"feedback,omitempty"`
	// Implicit counts what the person did after the reply, by kind ({"recall": 1, "thanks": 1}); hints for
	// choosing turns to review, apart from Feedback and never a score (implicit_feedback.go).
	Implicit map[string]int `json:"implicit,omitempty"`
	// SpeakerID is whom the turn answered; only its one-way hash is stored (speaker_ref), so a
	// withdrawn memory consent can find the person's turns (forgetPerson).
	SpeakerID string `json:"-"`
}

func (s *Service) recordTurn(t TurnRecord) {
	db, err := s.adminDB()
	if err != nil {
		return
	}
	now := s.Clock.Now()
	loc := s.instanceZone()
	local := now.In(loc)
	if t.ID == "" {
		t.ID = newID("turn_")
	}
	raw, _ := json.Marshal(t.Body)
	var body map[string]any
	_ = json.Unmarshal(raw, &body)
	s.referenceEvidence(body)
	keys, copied := evidenceOf(body)
	messageID, _ := body["message_id"].(string)
	lab := 0
	if t.Lab {
		lab = 1
	}
	state := evidenceIndexed
	if copied {
		state = evidenceLegacyText
	}
	// Only the consent check and the insert are serialized with a withdrawal: a turn recorded before
	// it is blanked by forgetPerson, one recorded after it sees the withdrawal and blanks itself.
	err = func() error {
		s.privacyMu.Lock()
		defer s.privacyMu.Unlock()
		if !s.MemoryAllowed(t.SpeakerID) {
			body["message"], body["speaker"], body["message_deleted"] = "", "", true
			body["answer"], body["answer_preview"] = "", ""
		}
		raw, _ = json.Marshal(body)
		return db.WithWriter(func(tx *sql.Tx) error {
			var err error
			keys, err = copySources(tx, keys)
			if err != nil {
				return err
			}
			blocked, err := store.MessageBlocked(tx, messageID, t.SpeakerID)
			if err != nil {
				return err
			}
			if blocked {
				body["message"], body["speaker"], body["message_deleted"] = "", "", true
				body["answer"], body["answer_preview"] = "", ""
			}
			for _, key := range keys {
				blocked, err := store.SourceBlocked(tx, key)
				if err != nil {
					return err
				}
				if blocked {
					body["hits"], body["tools"], body["plan"], body["context"], body["evidence_redacted"] = []any{}, []any{}, []any{}, []any{}, true
					body["answer"], body["answer_preview"] = "", ""
					keys = nil
					break
				}
			}
			raw, _ = json.Marshal(body)
			if _, err := tx.Exec(`INSERT INTO turn_log(id, at, day, hour, guild_id, channel_id, outcome, tools, hits, cost_usd, latency_ms, lab, body, speaker_ref, evidence_state)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, t.ID, now.UTC().Format(time.RFC3339Nano), local.Format("2006-01-02"), local.Hour(),
				t.GuildID, t.ChannelID, t.Outcome, strings.Join(t.Tools, ","), t.Hits, t.CostUSD, t.LatencyMS, lab, string(raw), personRef(t.SpeakerID), state); err != nil {
				return err
			}
			if db.IsSQLite() {
				return indexTurnRefs(tx, t.ID, keys, messageID)
			}
			return nil
		})
	}()
	if err != nil {
		s.Log.Debug("turn log write failed", "err", err.Error())
		return
	}
	s.emit("turn", t.ID, map[string]any{"outcome": t.Outcome, "lab": t.Lab})
	lvl := slog.LevelInfo
	if t.Outcome == "error" {
		lvl = slog.LevelWarn
	}
	s.Log.Log(context.Background(), lvl, "turn", "outcome", t.Outcome, "channel", t.ChannelID, "tools", strings.Join(t.Tools, ","),
		"hits", t.Hits, "latency_ms", t.LatencyMS, "cost_usd", fmt.Sprintf("%.4f", t.CostUSD), "lab", t.Lab)
}

func clip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n]) + "…"
}

func (s *Service) turnsAction(req action.Request) action.Result {
	db, _ := s.adminDB()
	in := inputMap(req)
	if req.Action == "turns.get" {
		id := strVal(in, "id", "")
		row := db.DB.QueryRow(`SELECT id, at, guild_id, channel_id, outcome, tools, hits, cost_usd, latency_ms, lab, body FROM turn_log WHERE id=?`, id)
		t, err := scanTurn(row.Scan, true)
		if err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "turn not found", nil)
		}
		rated, implicit := s.turnFeedback([]string{t.ID})
		t.Feedback, t.Implicit = rated[t.ID], implicit[t.ID]
		if t.Body["body_free"] == nil { // a channel's logging level keeps its text out of the history; do not rebuild it
			s.resolveEvidence(t.Body)
		}
		return action.OK(req, map[string]any{"turn": t, "ledger": s.turnLedgers(t.ID)})
	}
	where := []string{"1=1"}
	var args []any
	if v := strVal(in, "outcome", ""); v != "" {
		where, args = append(where, "outcome=?"), append(args, v)
	}
	if v := strVal(in, "channel_id", ""); v != "" {
		where, args = append(where, "channel_id=?"), append(args, v)
	}
	if v := strVal(in, "guild_id", ""); v != "" {
		where, args = append(where, "guild_id=?"), append(args, v)
	}
	if v := strVal(in, "tool", ""); v != "" {
		where, args = append(where, "(tools=? OR tools LIKE ? OR tools LIKE ? OR tools LIKE ?)"), append(args, v, v+",%", "%,"+v, "%,"+v+",%")
	}
	switch strVal(in, "lab", "false") {
	case "only":
		where = append(where, "lab=1")
	case "all":
	default:
		where = append(where, "lab=0")
	}
	if v := strVal(in, "before", ""); v != "" {
		where, args = append(where, "at<?"), append(args, v)
	}
	if v := strVal(in, "since", ""); v != "" {
		where, args = append(where, "at>=?"), append(args, v)
	}
	limit := min(max(intVal(in, "limit", 50), 1), 500)
	rows, err := db.Reader().Query(`SELECT id, at, guild_id, channel_id, outcome, tools, hits, cost_usd, latency_ms, lab, body FROM turn_log WHERE `+
		strings.Join(where, " AND ")+` ORDER BY at DESC LIMIT `+fmt.Sprint(limit), args...)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	defer rows.Close()
	list := []TurnRecord{}
	for rows.Next() {
		t, err := scanTurn(rows.Scan, false)
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		list = append(list, t)
	}
	ids := make([]string, len(list))
	for i, t := range list {
		ids[i] = t.ID
	}
	fb, implicit := s.turnFeedback(ids)
	names := s.turnChannelNames(list)
	botID, botName := "", ""
	if s.Discord != nil {
		botID, botName = s.Discord.BotUser()
	}
	for i := range list {
		list[i].Feedback, list[i].Implicit = fb[list[i].ID], implicit[list[i].ID]
		list[i].ChannelName = names[list[i].ChannelID]
		if msg, ok := list[i].Body["message"].(string); ok && botID != "" && botName != "" {
			// the preview reads "@bot …", not the raw mention markup
			list[i].Body["message"] = strings.NewReplacer("<@"+botID+">", "@"+botName, "<@!"+botID+">", "@"+botName).Replace(msg)
		}
	}
	out := map[string]any{"turns": list}
	if len(list) == limit {
		out["next_before"] = list[len(list)-1].At
	}
	return action.OK(req, out)
}

// turnChannelNames maps the listed turns' channels to their Discord names, one lookup per server.
func (s *Service) turnChannelNames(list []TurnRecord) map[string]string {
	out := map[string]string{}
	if s.Discord == nil || !s.Discord.Connected() {
		return out
	}
	seen := map[string]bool{}
	for _, t := range list {
		if t.GuildID == "" || seen[t.GuildID] {
			continue
		}
		seen[t.GuildID] = true
		for _, c := range s.Discord.Channels(t.GuildID) {
			out[c.ID] = c.Name
		}
	}
	return out
}

func scanTurn(scan func(...any) error, withBody bool) (TurnRecord, error) {
	var t TurnRecord
	var tools, body string
	var lab int64
	if err := scan(&t.ID, &t.At, &t.GuildID, &t.ChannelID, &t.Outcome, &tools, &t.Hits, &t.CostUSD, &t.LatencyMS, &lab, &body); err != nil {
		return t, err
	}
	t.Lab = lab == 1
	t.Tools = []string{}
	if tools != "" {
		t.Tools = strings.Split(tools, ",")
	}
	b := map[string]any{}
	_ = json.Unmarshal([]byte(body), &b)
	if withBody {
		t.Body = b
	} else {
		// the list shows short previews only
		t.Body = map[string]any{"message": b["message"], "answer": b["answer_preview"], "speaker": b["speaker"], "rag_state": b["rag_state"], "error": b["error"], "name_call": b["name_call"], "body_free": b["body_free"]}
	}
	return t, nil
}

// ---------------------------------------------------------------------------
// change log
// ---------------------------------------------------------------------------

// auditSafeKeys are input fields whose values are shown in the change log. Everything else is
// recorded by name only, so messages, YAML bodies and references never land in the log.
var auditSafeKeys = map[string]bool{
	"id": true, "guild_id": true, "channel_id": true, "respond": true, "persona_ref": true, "memory_use": true,
	"capture": true, "enabled": true, "chat": true, "commands": true, "role": true, "provider": true, "model": true,
	"scope": true, "paused": true, "to": true, "confirm": true, "name": true, "tool": true, "action": true,
	"logging_level": true, "switch": true, "dry_run": true, "verdict": true,
	"approve": true, "revoke": true, "block": true, "unblock": true, "targets": true, "server_approval": true, "guild_admins": true, "mode": true, "op": true,
}

func (s *Service) audit(req action.Request, desc catalog.Descriptor, res action.Result, revBefore int) {
	if desc.Effects == "read" || desc.Effects == "external_read" || desc.Effects == "none" || s.Store == nil {
		return
	}
	switch req.Action {
	case "admin.prefs.set", "lab.review", "chat.respond":
		return // personal preferences and lab notes are not configuration changes
	}
	if readForm(req) {
		return // the console's lists and status views of a writing Action change nothing
	}
	db, err := s.adminDB()
	if err != nil {
		return
	}
	in := inputMap(req)
	keys := make([]string, 0, len(in))
	for k := range in {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		if auditSafeKeys[k] {
			parts = append(parts, fmt.Sprintf("%s=%s", k, clip(fmt.Sprint(in[k]), 60)))
		} else if k == "ops" {
			parts = append(parts, "ops="+patchSummary(in[k]))
		} else {
			parts = append(parts, k)
		}
	}
	via := "api"
	switch {
	case req.Invoker != nil && req.Invoker.Via != "":
		via = req.Invoker.Via
	case strings.HasPrefix(req.RequestID, "system"):
		via = "system" // the service itself (approving the servers it was in when approval began)
	case strings.HasPrefix(req.RequestID, "dc:"):
		via = "discord" // made from Discord by the admin command
	case req.RequestID == "web" || strings.HasPrefix(req.RequestID, "web"):
		via = "gui"
	case req.RequestID == "cli":
		via = "cli"
	case req.RequestID == "tray":
		via = "tray"
	}
	guild := strVal(in, "guild_id", "")
	if guild == "" && req.Invoker != nil {
		guild = req.Invoker.GuildID
	}
	ok, code := 1, ""
	if !res.OK {
		ok = 0
		if res.Error != nil {
			code = res.Error.Code
		}
	}
	if ok == 1 {
		s.Log.Info("change", "action", req.Action, "via", via, "revision", s.Revision())
	} else {
		s.Log.Warn("change refused", "action", req.Action, "via", via, "code", code)
	}
	_, _ = db.DB.Exec(`INSERT INTO audit_log(at, action, via, ok, code, rev_before, rev_after, summary, actor, actor_id, guild_id) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
		s.Clock.Now().UTC().Format(time.RFC3339), req.Action, via, ok, code, revBefore, s.Revision(), clip(strings.Join(parts, " "), 900),
		clip(actorName(req), 120), actorID(req), clip(guild, 60))
	s.emit("audit", req.Action, nil)
}

// readForm reports a call of a writing Action that only reads: a list, a status or a plan. The
// console polls these, and each call would otherwise be a change log row.
func readForm(req action.Request) bool {
	in := inputMap(req)
	has := func(keys ...string) bool {
		for _, k := range keys {
			if _, ok := in[k]; ok {
				return true
			}
		}
		return false
	}
	op := strVal(in, "op", "")
	switch req.Action {
	case "participation.mode":
		return !has("mode", "review")
	case "reminders.update":
		return op == "" || op == "list"
	case "privacy.consent":
		return !has("decision")
	case "preferences.set":
		return !has("reset", "locale", "timezone", "quiet_start", "quiet_end", "weather_region")
	case "access.policy":
		if req.Invoker != nil && req.Invoker.UserID != "" {
			return op == "" || op == "status"
		}
		return !has("operators", "blocked_users", "guild_admins", "server_approval", "servers", "approve", "revoke", "block", "unblock")
	case "fortune.subscribe", "notices.subscribe":
		return op == "" || op == "status" || op == "list"
	case "memory.backfill":
		return boolVal(in, "status")
	case "models.recommend", "discord.check":
		return !boolVal(in, "apply")
	case "memory.blocks.prepare", "memory.episodes.extract", "room.style.build":
		return boolVal(in, "dry_run") // a cost estimate; starting the work is the change
	case "memory.mine.forget":
		return !has("confirm") // a preview; the confirmation is the change
	case "migration.apply", "channels.leave":
		return !boolVal(in, "confirm")
	}
	return false
}

func patchSummary(v any) string {
	ops, _ := v.([]any)
	var paths []string
	for _, o := range ops {
		m, _ := o.(map[string]any)
		p, _ := m["path"].([]any)
		var segs []string
		for _, x := range p {
			segs = append(segs, fmt.Sprint(x))
		}
		paths = append(paths, strings.Join(segs, "."))
	}
	return strings.Join(paths, ",")
}

// auditList is audit.list: the change log, newest first, with filters (action prefix, where it came
// from, server, who, result, date range) and paging by before_id. next_before_id is set when there is
// more.
func (s *Service) auditList(req action.Request) action.Result {
	db, err := s.adminDB()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	in := inputMap(req)
	where, args := []string{"1=1"}, []any{}
	if v := strVal(in, "action", ""); v != "" {
		where, args = append(where, "action LIKE ?"), append(args, v+"%")
	}
	if v := strVal(in, "via", ""); v != "" {
		vias := strings.Split(v, ",")
		ph := strings.TrimSuffix(strings.Repeat("?,", len(vias)), ",")
		where = append(where, "via IN ("+ph+")")
		for _, x := range vias {
			args = append(args, strings.TrimSpace(x))
		}
	}
	if v := strVal(in, "guild_id", ""); v != "" {
		where, args = append(where, "guild_id=?"), append(args, v)
	}
	if v := strVal(in, "actor", ""); v != "" {
		where, args = append(where, "(actor LIKE ? OR actor_id=?)"), append(args, "%"+v+"%", v)
	}
	switch strVal(in, "result", "") {
	case "ok":
		where = append(where, "ok=1")
	case "failed":
		where = append(where, "ok=0")
	}
	if v := strVal(in, "since", ""); v != "" {
		where, args = append(where, "at>=?"), append(args, v)
	}
	if v := strVal(in, "until", ""); v != "" {
		where, args = append(where, "at<?"), append(args, v)
	}
	if v := intVal(in, "before_id", 0); v > 0 {
		where, args = append(where, "id<?"), append(args, v)
	}
	limit := min(max(intVal(in, "limit", 50), 1), 5000)
	rows, err := db.DB.Query(`SELECT id, at, action, via, ok, code, rev_before, rev_after, summary, actor, actor_id, guild_id FROM audit_log WHERE `+
		strings.Join(where, " AND ")+` ORDER BY id DESC LIMIT `+fmt.Sprint(limit+1), args...)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	defer rows.Close()
	list := []map[string]any{}
	var next int64
	for rows.Next() {
		var id int64
		var at, act, via, code, summary, actor, aid, guild string
		var ok, rb, ra int
		if err := rows.Scan(&id, &at, &act, &via, &ok, &code, &rb, &ra, &summary, &actor, &aid, &guild); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		if len(list) == limit {
			next = list[len(list)-1]["id"].(int64)
			break
		}
		list = append(list, map[string]any{"id": id, "at": at, "action": act, "via": via, "ok": ok == 1, "code": code,
			"revision_before": rb, "revision_after": ra, "summary": summary, "actor": actor, "actor_id": aid, "guild_id": guild})
	}
	if err := rows.Err(); err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	out := map[string]any{"entries": list}
	if next > 0 {
		out["next_before_id"] = next
	}
	return action.OK(req, out)
}

// ---------------------------------------------------------------------------
// time series
// ---------------------------------------------------------------------------

func (s *Service) statsSeries(req action.Request) action.Result {
	db, _ := s.adminDB()
	in := inputMap(req)
	n := min(max(intVal(in, "days", 14), 1), 90)
	loc := s.instanceZone()
	today := s.Clock.Now().In(loc)
	days := make([]string, n)
	index := map[string]int{}
	for i := 0; i < n; i++ {
		d := today.AddDate(0, 0, i-n+1).Format("2006-01-02")
		days[i], index[d] = d, i
	}
	first := days[0]
	series := func() []float64 { return make([]float64, n) }

	outcomes := map[string][]float64{}
	var latency = series()
	var latencyN = series()
	rows, err := db.DB.Query(`SELECT day, outcome, COUNT(*), SUM(latency_ms) FROM turn_log WHERE lab=0 AND day>=? GROUP BY day, outcome`, first)
	if err == nil {
		for rows.Next() {
			var d, o string
			var c int
			var lat sql.NullFloat64
			if rows.Scan(&d, &o, &c, &lat) == nil {
				if i, ok := index[d]; ok {
					if outcomes[o] == nil {
						outcomes[o] = series()
					}
					outcomes[o][i] += float64(c)
					if o == "answered" && lat.Valid {
						latency[i] += lat.Float64
						latencyN[i] += float64(c)
					}
				}
			}
		}
		rows.Close()
	}
	for i := range latency {
		if latencyN[i] > 0 {
			latency[i] = latency[i] / latencyN[i]
		}
	}
	cost := map[string][]float64{}
	if rows, err := db.DB.Query(`SELECT day, kind, usd FROM memory_spend WHERE day>=?`, first); err == nil {
		for rows.Next() {
			var d, k string
			var usd float64
			if rows.Scan(&d, &k, &usd) == nil {
				if i, ok := index[d]; ok {
					if cost[k] == nil {
						cost[k] = series()
					}
					cost[k][i] += usd
				}
			}
		}
		rows.Close()
	}
	toolSeries := map[string][]float64{}
	if u := s.toolUsage(); u != nil {
		if rows, err := db.DB.Query(`SELECT day, tool, n FROM tool_usage WHERE scope='all' AND day>=?`, first); err == nil {
			for rows.Next() {
				var d, t string
				var c int
				if rows.Scan(&d, &t, &c) == nil {
					if i, ok := index[d]; ok {
						if toolSeries[t] == nil {
							toolSeries[t] = series()
						}
						toolSeries[t][i] += float64(c)
					}
				}
			}
			rows.Close()
		}
	}
	// weekday × hour activity for the last 28 days (answered and silent turns)
	heat := make([][]int, 7)
	for i := range heat {
		heat[i] = make([]int, 24)
	}
	since := today.AddDate(0, 0, -27).Format("2006-01-02")
	if rows, err := db.DB.Query(`SELECT day, hour, COUNT(*) FROM turn_log WHERE lab=0 AND day>=? GROUP BY day, hour`, since); err == nil {
		for rows.Next() {
			var d string
			var h, c int
			if rows.Scan(&d, &h, &c) == nil {
				if t, err := time.ParseInLocation("2006-01-02", d, loc); err == nil && h >= 0 && h < 24 {
					heat[int(t.Weekday())][h] += c
				}
			}
		}
		rows.Close()
	}
	return action.OK(req, map[string]any{"days": days, "turns": outcomes, "latency_ms": latency, "cost_usd": cost,
		"tools": toolSeries, "heatmap": heat, "timezone": loc.String()})
}

// ---------------------------------------------------------------------------
// dashboard
// ---------------------------------------------------------------------------

// toolProviderReady reports whether any provider of these kinds has what it needs (a key, or none).
func (s *Service) toolProviderReady(kinds []string) bool {
	for _, st := range s.Registry().Statuses() {
		if slices.Contains(kinds, st.Kind) && st.Configured && !st.Disabled {
			return true
		}
	}
	return false
}

type attention struct {
	Severity string `json:"severity"` // danger | warn | info
	Key      string `json:"key"`      // locale key under ui.attention.*
	Args     any    `json:"args,omitempty"`
	Route    string `json:"route"`
}

func (s *Service) dashboardOverview(ctx context.Context, req action.Request) action.Result {
	db, _ := s.adminDB()
	lap := s.laps(ctx, "dashboard")
	defer lap.done()
	doc := s.doctor(ctx, req)
	lap.mark("doctor")
	feats, _ := doc.Output["features"].(map[string]any)
	status := func(name string) map[string]any {
		if m, ok := feats[name].(map[string]any); ok {
			return m
		}
		return map[string]any{"status": "unknown"}
	}
	pingErr := s.Store.Ping(ctx)
	dbStatus := map[string]any{"status": "ready", "backend": s.Store.Backend}
	if pingErr != nil {
		dbStatus = map[string]any{"status": "error", "backend": s.Store.Backend, "reason": pingErr.Error()}
	}
	discord := status("discord")
	if s.Discord != nil {
		if s.Discord.Connected() {
			discord = map[string]any{"status": "ready", "guilds": len(s.Discord.Guilds())}
			if _, name := s.Discord.BotUser(); name != "" {
				discord["bot"] = name
			}
		} else if s.Doc().Discord.Enabled {
			discord = map[string]any{"status": "error", "reason": "gateway_disconnected"}
		}
	}
	lap.mark("ping_discord")
	mem := s.memoryStatus()
	lap.mark("memory_status")
	loc := s.instanceZone()
	today := s.Clock.Now().In(loc).Format("2006-01-02")

	counts := map[string]int{}
	if rows, err := db.DB.Query(`SELECT outcome, COUNT(*) FROM turn_log WHERE lab=0 AND day=? GROUP BY outcome`, today); err == nil {
		for rows.Next() {
			var o string
			var c int
			if rows.Scan(&o, &c) == nil {
				counts[o] = c
			}
		}
		rows.Close()
	}
	byKind, spent, _ := s.monthSpend()
	var spentToday float64 // memory_spend days are UTC (store.AddSpend)
	spendDay := s.Clock.Now().UTC().Format("2006-01-02")
	_ = db.DB.QueryRow(`SELECT COALESCE(SUM(usd),0) FROM memory_spend WHERE day=?`, spendDay).Scan(&spentToday)
	spentToday = s.spendAfterResetDay(spendDay, spentToday)
	budget := parseUSD(s.Doc().Limits.MonthlyBudgetUSD, 0)
	lap.mark("turns_spend")

	jobsRunning, jobsFailed := []map[string]any{}, 0
	if rows, err := db.DB.Query(`SELECT id, type, status, checkpoint, updated_at FROM jobs WHERE status IN ('pending','retry_wait','running','leased','paused','failed') ORDER BY updated_at DESC LIMIT 20`); err == nil {
		for rows.Next() {
			var id, typ, st, upd string
			var cp sql.NullString
			if rows.Scan(&id, &typ, &st, &cp, &upd) == nil {
				if st == "failed" {
					jobsFailed++
					continue
				}
				jobsRunning = append(jobsRunning, map[string]any{"id": id, "type": typ, "status": st, "progress": cp.String, "updated_at": upd})
			}
		}
		rows.Close()
	}

	type toolCount struct {
		Tool string `json:"tool"`
		N    int    `json:"n"`
	}
	topTools := []toolCount{}
	if s.toolUsage() != nil {
		day, _ := s.toolDay()
		if rows, err := db.DB.Query(`SELECT tool, n FROM tool_usage WHERE day=? AND scope='all' ORDER BY n DESC LIMIT 6`, day); err == nil {
			for rows.Next() {
				var tc toolCount
				if rows.Scan(&tc.Tool, &tc.N) == nil {
					topTools = append(topTools, tc)
				}
			}
			rows.Close()
		}
	}
	lap.mark("jobs_tools")
	recent := s.turnsAction(action.Request{Action: "turns.list", Input: json.RawMessage(`{"limit":6}`)}).Output["turns"]
	lap.mark("recent_turns")

	pause := s.pauseState()
	var att []attention
	if pause.All {
		att = append(att, attention{"warn", "paused", nil, "/"})
	}
	if pingErr != nil {
		att = append(att, attention{"danger", "db_error", nil, "/maintenance"})
	}
	if st, _ := status("llm")["status"].(string); st != "ready" {
		att = append(att, attention{"danger", "llm_unconfigured", nil, "/connections"})
	}
	if st, _ := discord["status"].(string); st == "error" {
		att = append(att, attention{"danger", "discord_down", nil, "/connections"})
	} else if st == "disabled" {
		att = append(att, attention{"info", "discord_disabled", nil, "/connections"})
	}
	if budget > 0 && spent >= budget {
		att = append(att, attention{"danger", "budget_reached", map[string]any{"spent": fmt.Sprintf("%.2f", spent), "budget": fmt.Sprintf("%.2f", budget)}, "/usage"})
	} else if budget > 0 && spent >= float64(s.Doc().Limits.AlertPercent())/100*budget {
		att = append(att, attention{"warn", "budget_near", map[string]any{"pct": int(100 * spent / budget)}, "/usage"})
	}
	att = append(att, s.creditAttention(spent, budget)...)
	att = append(att, s.opsAttention()...)
	lap.mark("ops_attention")
	att = append(att, s.permsAttention()...)
	att = append(att, s.serversAttention()...)
	lap.mark("discord_attention")
	att = append(att, s.reconciliationAttention()...) // quarantined Discord edits/deletes (gateway_health.go)
	if jobsFailed > 0 {
		att = append(att, attention{"warn", "jobs_failed", map[string]any{"n": jobsFailed}, "/jobs"})
	}
	// Only what stops the bot or loses data is an alert. A tool whose key is missing (disasters without
	// a KMA key) is optional: the tools page marks it, the dashboard does not nag about it.
	if roles := s.unpricedRoles(); len(roles) > 0 {
		att = append(att, attention{"warn", "price_unknown", map[string]any{"roles": strings.Join(roles, ", ")}, "/connections"})
	}
	if _, why := s.memoryDoctor(); why == "embed_model_changed" {
		att = append(att, attention{"warn", "embed_model_changed", nil, "/blocks"})
	}
	if s.restartPending() {
		att = append(att, attention{"warn", "restart_pending", nil, "/maintenance"})
	}
	lap.mark("other_attention")
	if att == nil {
		att = []attention{}
	}

	counts2 := map[string]int{}
	if c, ok := mem["counts"].(map[string]int); ok {
		counts2 = c
	}
	monthView := map[string]any{"spent_usd": spent, "budget_usd": budget, "by_kind": byKind}
	if credit := s.providerCredit(); len(credit) > 0 {
		monthView["key_credit"] = credit
	}
	if reset, ok := s.activeSpendReset(s.Clock.Now()); ok {
		monthView["reset"] = spendResetInfo(reset)
	}
	return action.OK(req, map[string]any{
		"instance": s.Doc().Instance.ID, "version": Version, "revision": s.Revision(),
		"uptime_sec": int(time.Since(startedAt).Seconds()), "pause": pause,
		"status": map[string]any{
			"daemon":  map[string]any{"status": "ready"},
			"discord": discord,
			"db":      dbStatus,
			"llm":     status("llm"),
			"memory":  status("memory"),
			"jobs":    map[string]any{"status": ternary(jobsFailed > 0, "degraded", "ready"), "running": len(jobsRunning), "failed": jobsFailed},
		},
		"attention": att,
		"checklist": s.checklist(counts2),
		"today":     map[string]any{"day": today, "turns": counts, "cost_usd": spentToday},
		"month":     monthView,
		"jobs":      jobsRunning,
		"top_tools": topTools,
		"recent":    recent,
		"memory":    counts2,
	})
}

func ternary[T any](c bool, a, b T) T {
	if c {
		return a
	}
	return b
}

// checklist items are computed from real state, never from "the button was pressed".
func (s *Service) checklist(memCounts map[string]int) []map[string]any {
	db, _ := s.adminDB()
	num := func(q string, args ...any) int {
		var n int
		_ = db.DB.QueryRow(q, args...).Scan(&n)
		return n
	}
	llmReady := false
	for _, role := range []string{"response", "planner"} {
		if m, ok := modelOf(s.Doc(), role); ok {
			if st, ok := s.Registry().Status(m.Provider); ok && st.Configured {
				llmReady = true
			}
		}
	}
	roomReady := false
	for _, g := range s.Doc().Guilds {
		for _, c := range g.Channels {
			if c.Respond != "" && c.Respond != "off" {
				roomReady = true
			}
		}
	}
	intOf := func(k string) int { return memCounts[k] }
	anyLimits := false
	for _, t := range s.Doc().Tools {
		if t.Limits.PerDay > 0 || t.Limits.PerUserPerDay > 0 || t.Limits.PerChannelPerDay > 0 || t.Limits.CooldownSec > 0 {
			anyLimits = true
		}
	}
	backups, _ := os.ReadDir(s.backupDir())
	budgetReviewed := s.Doc().Limits.MonthlyBudgetUSD != config.Default("", "").Limits.MonthlyBudgetUSD
	var ack map[string]bool
	_, _ = s.kvGet("checklist:ack", &ack)
	return []map[string]any{
		{"id": "model_key", "done": llmReady, "route": "/connections"},
		{"id": "discord", "done": s.Discord != nil && s.Discord.Connected(), "route": "/connections"},
		{"id": "first_room", "done": roomReady, "route": "/rooms"},
		{"id": "test_chat", "done": num(`SELECT COUNT(*) FROM turn_log WHERE outcome='answered'`) > 0, "route": "/lab"},
		{"id": "import", "done": intOf("observations") > 0, "route": "/blocks?import=1"},
		{"id": "index", "done": intOf("leaves_embedded") > 0, "route": "/blocks"},
		{"id": "tool_limits", "done": anyLimits || ack["tool_limits"], "route": "/tools"},
		{"id": "budget", "done": budgetReviewed || ack["budget"], "route": "/usage"},
		{"id": "backup", "done": len(backups) > 0, "route": "/maintenance"},
	}
}

// ---------------------------------------------------------------------------
// lab reviews
// ---------------------------------------------------------------------------

func (s *Service) labReviews(req action.Request) action.Result {
	db, _ := s.adminDB()
	in := inputMap(req)
	if req.Action == "lab.review" {
		if id := strVal(in, "delete", ""); id != "" {
			if _, err := db.DB.Exec(`DELETE FROM lab_reviews WHERE id=?`, id); err != nil {
				return s.evidenceWriteError(req, err)
			}
			return action.OK(req, map[string]any{"deleted": id})
		}
		verdict := strVal(in, "verdict", "")
		if verdict != "good" && verdict != "bad" && verdict != "unsure" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "verdict must be good, bad or unsure", nil)
		}
		q := strVal(in, "question", "")
		if q == "" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "question required", nil)
		}
		meta, _ := json.Marshal(in["meta"])
		id := newID("rv_")
		var v any
		_ = json.Unmarshal(meta, &v)
		err := db.WithWriter(func(tx *sql.Tx) error {
			keys, err := copySources(tx, v)
			if err != nil {
				return err
			}
			if err := indexCopy(tx, holderLab, id, keys, true); err != nil {
				return err
			}
			_, err = tx.Exec(`INSERT INTO lab_reviews(id, at, channel_key, question, answer, verdict, tags, note, meta) VALUES(?,?,?,?,?,?,?,?,?)`,
				id, s.Clock.Now().UTC().Format(time.RFC3339), strVal(in, "channel_key", ""), clip(q, 2000), clip(strVal(in, "answer", ""), 4000),
				verdict, strings.Join(strSlice(in, "tags"), ","), clip(strVal(in, "note", ""), 1000), string(meta))
			if err != nil {
				return err
			}
			_, err = tx.Exec(`UPDATE lab_reviews SET evidence_state=1 WHERE id=?`, id)
			return err
		})
		if err != nil {
			return s.evidenceWriteError(req, err)
		}
		return action.OK(req, map[string]any{"id": id})
	}
	limit := min(max(intVal(in, "limit", 100), 1), 1000)
	rows, err := db.DB.Query(`SELECT id, at, channel_key, question, answer, verdict, tags, note, meta FROM lab_reviews ORDER BY at DESC LIMIT ` + fmt.Sprint(limit))
	if err != nil {
		return s.evidenceWriteError(req, err)
	}
	defer rows.Close()
	list := []map[string]any{}
	counts := map[string]int{}
	for rows.Next() {
		var id, at, ck, q, a, v, tags, note, meta string
		if rows.Scan(&id, &at, &ck, &q, &a, &v, &tags, &note, &meta) != nil {
			continue
		}
		var m any
		_ = json.Unmarshal([]byte(meta), &m)
		tl := []string{}
		if tags != "" {
			tl = strings.Split(tags, ",")
		}
		counts[v]++
		list = append(list, map[string]any{"id": id, "at": at, "channel_key": ck, "question": q, "answer": a, "verdict": v, "tags": tl, "note": note, "meta": m})
	}
	return action.OK(req, map[string]any{"reviews": list, "counts": counts})
}

// ---------------------------------------------------------------------------
// config as a JSON document
// ---------------------------------------------------------------------------

func (s *Service) configDocument(req action.Request) action.Result {
	cur, err := docTree(s.Doc())
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	def, err := docTree(config.Default(s.Doc().Instance.ID, s.Doc().Instance.DataDir))
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	res := action.OK(req, map[string]any{"revision": s.Revision(), "document": cur, "defaults": def, "schema": config.Schema()})
	res.Revision = s.Revision()
	return res
}

// docTree is the document as the YAML file spells it (field names, omitted empties).
func docTree(d config.Document) (map[string]any, error) {
	raw, err := config.Marshal(d)
	if err != nil {
		return nil, err
	}
	return config.Tree(raw)
}

// configPatch applies path edits to the current document with the usual CAS, validation and revision.
// ops: [{"path": ["tools","finance.fx","limits","per_day"], "value": 100}, {"path": [...], "op": "unset"}]
func (s *Service) configPatch(req action.Request) action.Result {
	in := inputMap(req)
	ops, _ := in["ops"].([]any)
	if len(ops) == 0 {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "ops required", nil)
	}
	// the edits apply to one snapshot; the commit's revision check refuses them if another change
	// landed in between
	live := s.snapshot()
	base := live.rev
	if req.BaseRevision > 0 {
		base = req.BaseRevision
	} else if b := intVal(in, "base_revision", 0); b > 0 {
		base = b
	}
	if base != live.rev {
		return action.Fail(req, errcode.ConfigConflict, "config.conflict", s.msg(req, "config.conflict"), map[string]any{"expected": live.rev, "actual": base})
	}
	tree, err := docTree(live.doc)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	for _, o := range ops {
		m, _ := o.(map[string]any)
		rawPath, _ := m["path"].([]any)
		path := make([]string, 0, len(rawPath))
		for _, p := range rawPath {
			path = append(path, fmt.Sprint(p))
		}
		if len(path) == 0 {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "empty path", nil)
		}
		if strVal(m, "op", "set") == "unset" {
			config.TreeUnset(tree, path)
			continue
		}
		if err := config.TreeSet(tree, path, plainJSON(m["value"])); err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
		}
	}
	candidate, err := config.FromTree(tree)
	if err != nil {
		return action.Fail(req, errcode.ConfigInvalid, "generic.error", err.Error(), nil)
	}
	if boolVal(in, "dry_run") {
		cur, _ := config.Marshal(s.Doc())
		return action.OK(req, map[string]any{"dry_run": true, "valid": true, "changed": string(cur) != string(candidate)})
	}
	return s.applyCandidate(req, candidate, base)
}

// plainJSON turns json.Number (inputs are decoded with UseNumber) into int64 or float64, so the
// YAML encoder writes numbers, not quoted strings.
func plainJSON(v any) any {
	switch x := v.(type) {
	case json.Number:
		if i, err := x.Int64(); err == nil {
			return i
		}
		f, _ := x.Float64()
		return f
	case map[string]any:
		for k, e := range x {
			x[k] = plainJSON(e)
		}
		return x
	case []any:
		for i, e := range x {
			x[i] = plainJSON(e)
		}
		return x
	}
	return v
}
