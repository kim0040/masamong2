package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"masamong/internal/errclass"
	"masamong/internal/errcode"
	"masamong/internal/observability"
	"masamong/internal/tools"
)

// Call ledgers: one row per model call (llm_calls) and per tool execution (tool_calls) of a chat turn,
// so "why was this answer slow, expensive or wrong" can be read from numbers instead of guessed. The
// console shows them with the turn (turns.get).
//
// Stored: turn and job ids, role, model, provider, token counts, timing, cost, the failure class (a
// short machine value), and for tools the parameter NAMES only. No message text, no parameter values,
// no Discord user id. Rows go after ledgerKeepDays (retention.go). Rows are written off the turn's
// path: a ledger write that fails or is slow must never delay or fail a reply, so it is dropped with a
// debug line.

const ledgerKeepDays = 90

var ledgerSQLite = []string{
	`CREATE TABLE IF NOT EXISTS llm_calls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		turn_id TEXT NOT NULL DEFAULT '',
		job_id TEXT NOT NULL DEFAULT '',
		role TEXT NOT NULL,
		call_count INTEGER NOT NULL DEFAULT 1,
		model TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '',
		prompt_tokens INTEGER NOT NULL DEFAULT 0,
		completion_tokens INTEGER NOT NULL DEFAULT 0,
		cached_tokens INTEGER NOT NULL DEFAULT 0,
		latency_ms INTEGER NOT NULL DEFAULT 0,
		cost_usd REAL NOT NULL DEFAULT 0,
		class TEXT NOT NULL DEFAULT '',
		fallback INTEGER NOT NULL DEFAULT 0,
		finish_reason TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_llm_calls_turn ON llm_calls(turn_id)`,
	`CREATE INDEX IF NOT EXISTS idx_llm_calls_created ON llm_calls(created_at)`,
	`CREATE TABLE IF NOT EXISTS tool_calls (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		turn_id TEXT NOT NULL DEFAULT '',
		tool TEXT NOT NULL,
		param_keys TEXT NOT NULL DEFAULT '[]',
		param_sources TEXT NOT NULL DEFAULT '{}',
		guard TEXT NOT NULL DEFAULT 'allowed',
		status TEXT NOT NULL DEFAULT '',
		latency_ms INTEGER NOT NULL DEFAULT 0,
		cost_usd REAL NOT NULL DEFAULT 0,
		result_chars INTEGER NOT NULL DEFAULT 0,
		class TEXT NOT NULL DEFAULT '',
		created_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_tool_calls_turn ON tool_calls(turn_id)`,
	`CREATE INDEX IF NOT EXISTS idx_tool_calls_created ON tool_calls(created_at)`,
}

var ledgerTiDB = []string{
	`CREATE TABLE IF NOT EXISTS llm_calls (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		turn_id VARCHAR(64) NOT NULL DEFAULT '',
		job_id VARCHAR(64) NOT NULL DEFAULT '',
		role VARCHAR(64) NOT NULL,
		call_count INT NOT NULL DEFAULT 1,
		model VARCHAR(255) NOT NULL DEFAULT '',
		provider VARCHAR(128) NOT NULL DEFAULT '',
		prompt_tokens INT NOT NULL DEFAULT 0,
		completion_tokens INT NOT NULL DEFAULT 0,
		cached_tokens INT NOT NULL DEFAULT 0,
		latency_ms INT NOT NULL DEFAULT 0,
		cost_usd DOUBLE NOT NULL DEFAULT 0,
		class VARCHAR(32) NOT NULL DEFAULT '',
		fallback TINYINT NOT NULL DEFAULT 0,
		finish_reason VARCHAR(64) NOT NULL DEFAULT '',
		created_at VARCHAR(64) NOT NULL,
		KEY idx_llm_calls_turn (turn_id),
		KEY idx_llm_calls_created (created_at)
	)`,
	`CREATE TABLE IF NOT EXISTS tool_calls (
		id BIGINT NOT NULL AUTO_INCREMENT PRIMARY KEY,
		turn_id VARCHAR(64) NOT NULL DEFAULT '',
		tool VARCHAR(128) NOT NULL,
		param_keys VARCHAR(1024) NOT NULL DEFAULT '[]',
		param_sources VARCHAR(1024) NOT NULL DEFAULT '{}',
		guard VARCHAR(16) NOT NULL DEFAULT 'allowed',
		status VARCHAR(64) NOT NULL DEFAULT '',
		latency_ms INT NOT NULL DEFAULT 0,
		cost_usd DOUBLE NOT NULL DEFAULT 0,
		result_chars INT NOT NULL DEFAULT 0,
		class VARCHAR(32) NOT NULL DEFAULT '',
		created_at VARCHAR(64) NOT NULL,
		KEY idx_tool_calls_turn (turn_id),
		KEY idx_tool_calls_created (created_at)
	)`,
}

func init() {
	adminSQLite = append(adminSQLite, ledgerSQLite...)
	adminTiDB = append(adminTiDB, ledgerTiDB...)
}

// llmCall is one model call. Tokens are what the provider reported (0 when it reported none).
type llmCall struct {
	Role, Model, Provider, FinishReason string
	PromptTokens, CompletionTokens      int
	CallCount                           int
	CachedTokens                        int
	Latency                             time.Duration
	CostUSD                             float64
	Class                               errclass.Class
	Fallback                            bool      // the other chat model standing in for the role's own
	since                               time.Time // first call of an index job's running aggregate
}

// noteLLMCall records one model call of the turn (or background job) ctx belongs to.
func (s *Service) noteLLMCall(ctx context.Context, c llmCall) {
	if s.Store == nil || s.practice {
		return
	}
	turn, job, now := observability.TurnID(ctx), observability.JobID(ctx), s.Clock.Now().UTC().Format(time.RFC3339)
	fb := 0
	if c.Fallback {
		fb = 1
	}
	callCount := max(1, c.CallCount)
	s.ledger.add(s, `INSERT INTO llm_calls(turn_id, job_id, role, call_count, model, provider, prompt_tokens, completion_tokens, cached_tokens, latency_ms, cost_usd, class, fallback, finish_reason, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, turn, job, c.Role, callCount, c.Model, c.Provider, c.PromptTokens, c.CompletionTokens, c.CachedTokens,
		c.Latency.Milliseconds(), c.CostUSD, string(c.Class), fb, c.FinishReason, now)
}

// An index job's running aggregate goes out as a row of its own every memoryLedgerCheckpointCalls
// calls or memoryLedgerCheckpointAge, so a crash or a power cut loses at most that much of a long
// index (the job's calls are already in memory_spend; only the ledger rows would be missing).
const (
	memoryLedgerCheckpointCalls = 1000
	memoryLedgerCheckpointAge   = time.Minute
)

// noteMemoryLLMCall keeps one row per turn call, but combines a job's many index requests by role. A
// full index can make tens of thousands of embed and summary requests; without this boundary the bounded
// ledger queue can drop rows and each row competes with other SQLite writers on low-spec devices.
// A call with neither a turn nor a job (console memory.search, evaluation, timeline) is not recorded:
// nothing reads such a row (the console shows ledgers per turn), and its cost is in memory_spend.
func (s *Service) noteMemoryLLMCall(ctx context.Context, c llmCall) {
	if s.Store == nil || s.practice {
		return
	}
	job := observability.JobID(ctx)
	if job == "" {
		if observability.TurnID(ctx) == "" {
			return
		}
		c.CallCount = max(1, c.CallCount)
		s.noteLLMCall(ctx, c)
		return
	}
	count := max(1, c.CallCount)
	now := s.Clock.Now()
	var checkpoint *llmCall
	s.memoryLedgerMu.Lock()
	if s.memoryLedger == nil {
		s.memoryLedger = map[string]map[string]llmCall{}
	}
	byRole := s.memoryLedger[job]
	if byRole == nil {
		byRole = map[string]llmCall{}
		s.memoryLedger[job] = byRole
	}
	if current, ok := byRole[c.Role]; ok {
		current.CallCount += count
		current.PromptTokens += c.PromptTokens
		current.CompletionTokens += c.CompletionTokens
		current.CachedTokens += c.CachedTokens
		current.CostUSD += c.CostUSD
		current.Latency += c.Latency
		if current.Class == "" {
			current.Class = c.Class
		} else if c.Class != "" && current.Class != c.Class {
			current.Class = errclass.Unknown
		}
		current.FinishReason = ""
		if current.CallCount >= memoryLedgerCheckpointCalls || now.Sub(current.since) >= memoryLedgerCheckpointAge {
			checkpoint = &current
			delete(byRole, c.Role)
		} else {
			byRole[c.Role] = current
		}
	} else {
		c.CallCount = count
		c.since = now
		byRole[c.Role] = c
	}
	s.memoryLedgerMu.Unlock()
	if checkpoint != nil {
		s.noteLLMCall(ctx, *checkpoint)
	}
}

// flushAllMemoryLLMCalls writes every job aggregate still held (Close, after all work has stopped).
func (s *Service) flushAllMemoryLLMCalls() {
	s.memoryLedgerMu.Lock()
	jobs := make([]string, 0, len(s.memoryLedger))
	for job := range s.memoryLedger {
		jobs = append(jobs, job)
	}
	s.memoryLedgerMu.Unlock()
	sort.Strings(jobs)
	for _, job := range jobs {
		s.flushMemoryLLMCalls(observability.WithJob(context.Background(), job))
	}
}

// flushMemoryLLMCalls closes one indexing work unit and hands its role summaries to the bounded writer.
func (s *Service) flushMemoryLLMCalls(ctx context.Context) {
	job := observability.JobID(ctx)
	if job == "" {
		return
	}
	s.memoryLedgerMu.Lock()
	byRole := s.memoryLedger[job]
	delete(s.memoryLedger, job)
	s.memoryLedgerMu.Unlock()
	roles := make([]string, 0, len(byRole))
	for role := range byRole {
		roles = append(roles, role)
	}
	sort.Strings(roles)
	for _, role := range roles {
		s.noteLLMCall(ctx, byRole[role])
	}
}

func memoryJobContext(ctx context.Context, prefix string) context.Context {
	if observability.JobID(ctx) != "" {
		return ctx
	}
	return observability.WithJob(ctx, newID(prefix))
}

// toolCall is one tool execution of a turn. Params are only counted by name.
type toolCall struct {
	Tool, Guard, Status string
	ParamKeys           []string
	// ParamSources is reserved: it is filled (parameter name → "message"|"conversation"|"default"|"setting"|"model")
	// once tools report where each value came from. No tool does yet, so rows hold {}.
	ParamSources map[string]string
	Latency      time.Duration
	CostUSD      float64
	ResultChars  int
	Class        errclass.Class
}

// noteToolCall records one tool execution (or one the turn's Guard held back) of the turn ctx belongs to.
func (s *Service) noteToolCall(ctx context.Context, c toolCall) {
	if s.Store == nil || s.practice {
		return
	}
	keys := append([]string{}, c.ParamKeys...)
	sort.Strings(keys)
	kj, _ := json.Marshal(keys)
	src := c.ParamSources
	if src == nil {
		src = map[string]string{}
	}
	sj, _ := json.Marshal(src)
	guard := c.Guard
	if guard == "" {
		guard = "allowed"
	}
	s.ledger.add(s, `INSERT INTO tool_calls(turn_id, tool, param_keys, param_sources, guard, status, latency_ms, cost_usd, result_chars, class, created_at)
		VALUES(?,?,?,?,?,?,?,?,?,?,?)`, observability.TurnID(ctx), c.Tool, string(kj), string(sj), guard, c.Status, c.Latency.Milliseconds(),
		c.CostUSD, c.ResultChars, string(c.Class), s.Clock.Now().UTC().Format(time.RFC3339))
}

// ledgerQueue is how many rows may wait for the writer. A turn makes a handful of rows, so this only
// fills when the database stalls; rows that do not fit are dropped (and counted), never waited for.
const (
	ledgerQueue = 1024
	ledgerBatch = 128
	ledgerFlush = 100 * time.Millisecond // how long the writer gathers rows into one transaction
)

type ledgerRow struct {
	query string
	args  []any
}

// ledgerWriter is the one goroutine that writes the call ledgers: rows come in on a bounded channel and
// go out in one transaction per flush. It starts with the first row, stops with the service (Close
// cancels the work context) and flushes what it holds before it returns.
type ledgerWriter struct {
	once     sync.Once
	rows     chan ledgerRow
	dropped  atomic.Int64
	warnedAt atomic.Int64 // unix seconds of the last "dropping rows" warning
}

func (w *ledgerWriter) add(s *Service, query string, args ...any) {
	w.once.Do(func() {
		w.rows = make(chan ledgerRow, ledgerQueue)
		s.ownedGo("call ledger", func(ctx context.Context) { w.run(ctx, s) })
	})
	select {
	case w.rows <- ledgerRow{query, args}:
	default:
		n := w.dropped.Add(1)
		now := time.Now().Unix()
		if last := w.warnedAt.Load(); now-last >= 60 && w.warnedAt.CompareAndSwap(last, now) {
			s.Log.Warn("call ledger is full, rows are dropped", "dropped_total", n)
		}
	}
}

// drain writes what is still queued, on the caller's goroutine. Close calls it after all work (the writer
// goroutine included) has stopped: a job cancelled by Close hands its last aggregate over while the
// writer may already be gone, and those rows would otherwise stay in the channel.
func (w *ledgerWriter) drain(s *Service) {
	w.once.Do(func() { w.rows = make(chan ledgerRow, ledgerQueue) }) // visibility of rows; no writer starts here
	var batch []ledgerRow
	for {
		select {
		case r := <-w.rows:
			batch = append(batch, r)
		default:
			w.flush(s, batch)
			return
		}
	}
}

func (w *ledgerWriter) run(ctx context.Context, s *Service) {
	var batch []ledgerRow
	for {
		select {
		case r := <-w.rows:
			batch = append(batch[:0], r)
			// gather what arrives in the next moment into the same transaction
			timer := time.NewTimer(ledgerFlush)
		gather:
			for len(batch) < ledgerBatch {
				select {
				case r := <-w.rows:
					batch = append(batch, r)
				case <-timer.C:
					break gather
				case <-ctx.Done():
					break gather
				}
			}
			timer.Stop()
			w.flush(s, batch)
		case <-ctx.Done():
			batch = batch[:0]
			for {
				select {
				case r := <-w.rows:
					batch = append(batch, r)
					continue
				default:
				}
				break
			}
			w.flush(s, batch)
			return
		}
	}
}

// flush writes rows in one transaction. A row the database refuses is skipped, not retried.
func (w *ledgerWriter) flush(s *Service, rows []ledgerRow) {
	if len(rows) == 0 {
		return
	}
	db, err := s.adminDB()
	if err != nil {
		return
	}
	err = db.WithWriter(func(tx *sql.Tx) error {
		for _, r := range rows {
			if _, err := tx.Exec(r.query, r.args...); err != nil {
				s.Log.Debug("call ledger row failed", "err", err.Error())
			}
		}
		return nil
	})
	if err != nil {
		s.Log.Debug("call ledger write failed", "err", err.Error())
	}
}

// LedgerDropped is how many call-ledger rows were dropped because the writer's queue was full.
func (s *Service) LedgerDropped() int64 { return s.ledger.dropped.Load() }

// ledgerRoleKey carries a more specific name for the model call made under ctx.
type ledgerRoleKey struct{}

// withLedgerRole names what a planner-route call is for ("reference", "name_call", "follow",
// "thread_summary"), so the ledger can tell those small classifier calls from the turn's own planner.
// The route that serves the call is not changed.
func withLedgerRole(ctx context.Context, label string) context.Context {
	return context.WithValue(ctx, ledgerRoleKey{}, label)
}

func ledgerRole(ctx context.Context, role string) string {
	if l, _ := ctx.Value(ledgerRoleKey{}).(string); l != "" {
		return l
	}
	return role
}

// declaredParamKeys keeps the parameter names the tool declares (its Inputs) and folds anything else the
// planner invented into "_other", so a model cannot write free text into the ledger through a key name.
// The list is capped.
func declaredParamKeys(declared []tools.Input, params map[string]any) []string {
	known := make(map[string]bool, len(declared))
	for _, in := range declared {
		known[in.Name] = true
	}
	var keys []string
	other := false
	for k := range params {
		if known[k] {
			keys = append(keys, k)
		} else {
			other = true
		}
	}
	sort.Strings(keys)
	if len(keys) > 16 {
		keys = keys[:16]
	}
	if other {
		keys = append(keys, "_other")
	}
	return keys
}

// toolErrClass maps a failed tool result's error code to the shared failure vocabulary.
func toolErrClass(code string) errclass.Class {
	switch code {
	case "":
		return errclass.None
	case errcode.ProviderUnconfigured:
		return errclass.Auth
	case errcode.ProviderUnavailable:
		return errclass.Overloaded
	case errcode.ProviderRateLimited:
		return errclass.RateLimited
	case errcode.ProviderRejected:
		return errclass.BadRequest
	case errcode.ProviderFormat:
		return errclass.Format
	case errcode.ToolLimited, errcode.BudgetExceeded:
		return errclass.Quota
	case errcode.InvalidInput:
		return errclass.BadRequest
	case errcode.PermissionDenied:
		return errclass.Permission
	}
	return errclass.Unknown
}

// turnLedgers is the body-free call records of one turn, for the console's turn detail.
func (s *Service) turnLedgers(turnID string) map[string]any {
	out := map[string]any{"llm_calls": []map[string]any{}, "tool_calls": []map[string]any{}, "delivery": []map[string]any{}}
	db, err := s.adminDB()
	if err != nil || turnID == "" {
		return out
	}
	read := func(key, query string, cols []string, conv func(raw []any) map[string]any) {
		rows, err := db.DB.Query(query, turnID)
		if err != nil {
			return
		}
		defer rows.Close()
		list := []map[string]any{}
		for rows.Next() {
			raw := make([]any, len(cols))
			ptr := make([]any, len(cols))
			for i := range raw {
				ptr[i] = &raw[i]
			}
			if rows.Scan(ptr...) != nil {
				return
			}
			list = append(list, conv(raw))
		}
		out[key] = list
	}
	rowMap := func(cols []string) func(raw []any) map[string]any {
		return func(raw []any) map[string]any {
			m := make(map[string]any, len(cols))
			for i, c := range cols {
				if b, ok := raw[i].([]byte); ok {
					raw[i] = string(b)
				}
				m[c] = raw[i]
			}
			return m
		}
	}
	llm := []string{"role", "call_count", "model", "provider", "prompt_tokens", "completion_tokens", "cached_tokens", "latency_ms", "cost_usd", "class", "fallback", "finish_reason", "created_at"}
	read("llm_calls", `SELECT `+strings.Join(llm, ", ")+` FROM llm_calls WHERE turn_id=? ORDER BY id`, llm, func(raw []any) map[string]any {
		m := rowMap(llm)(raw)
		m["fallback"] = asInt(m["fallback"]) == 1
		return m
	})
	tc := []string{"tool", "param_keys", "param_sources", "guard", "status", "latency_ms", "cost_usd", "result_chars", "class", "created_at"}
	read("tool_calls", `SELECT `+strings.Join(tc, ", ")+` FROM tool_calls WHERE turn_id=? ORDER BY id`, tc, func(raw []any) map[string]any {
		m := rowMap(tc)(raw)
		var keys []string
		_ = json.Unmarshal([]byte(asString(m["param_keys"])), &keys)
		src := map[string]string{}
		_ = json.Unmarshal([]byte(asString(m["param_sources"])), &src)
		m["param_keys"], m["param_sources"] = append([]string{}, keys...), src
		return m
	})
	dl := []string{"parts", "sent", "retries", "class", "reference_dropped", "created_at"}
	read("delivery", `SELECT `+strings.Join(dl, ", ")+` FROM delivery_ledger WHERE turn_id=? ORDER BY id`, dl, func(raw []any) map[string]any {
		m := rowMap(dl)(raw)
		m["reference_dropped"] = asInt(m["reference_dropped"]) == 1
		return m
	})
	return out
}

func asInt(v any) int64 {
	switch n := v.(type) {
	case int64:
		return n
	case int:
		return int64(n)
	}
	return 0
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// withLoggingLevel applies a channel's logging.level to a turn's console record (D9). ops keeps the
// turn whole; errors keeps the words only of turns that ended in an error; off of none. It applies to
// turns recorded from now on; turns already in the history keep what they were stored with. A turn without
// its words still keeps what the counters and ledgers need: ids, timing, tool names and outcomes, costs,
// the evidence's places and scores, and which lines went into the context.
func (s *Service) withLoggingLevel(level, outcome string, turnErr error, body map[string]any) map[string]any {
	if level == "" || level == "ops" || (level == "errors" && outcome == "error") {
		return body
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return body
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return body
	}
	for _, k := range []string{"message", "answer", "answer_preview", "speaker"} {
		delete(m, k)
	}
	each := func(key string, fn func(map[string]any)) {
		list, _ := m[key].([]any)
		for _, it := range list {
			if e, ok := it.(map[string]any); ok {
				fn(e)
			}
		}
	}
	each("hits", func(h map[string]any) {
		for _, k := range []string{"excerpt", "summary", "speakers"} {
			delete(h, k)
		}
	})
	// a tool's result and a plan's parameters can quote the question (a search query, a reminder text)
	each("tools", func(t map[string]any) {
		for _, k := range []string{"data", "error"} {
			delete(t, k)
		}
	})
	each("plan", func(p map[string]any) { delete(p, "parameters") })
	// provider and tool error texts can echo what the member wrote: keep the failure class instead
	if _, ok := m["error"]; ok {
		class := errclass.Of(turnErr)
		if class == errclass.Unknown || class == errclass.None {
			class = errclass.Class(outcome)
		}
		m["error"] = string(class)
	}
	m["body_free"] = level
	return m
}
