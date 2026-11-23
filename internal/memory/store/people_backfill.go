package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
)

// The paid people backfill (person memory phase 4, contract person-memory-v170 §5) keeps its runs and
// windows in two tables of its own, never in memory_batch_plans: a room held by a batch plan is left
// alone by the background index pass, and a backfill must never hold a room's indexing.
//
// people_backfill_run is one operator-started backfill of one server: the plan it was confirmed for
// (token and parameters), the chosen model and provider, the cap and what was spent against it, the
// pilot's counts-only report, and the provider batches in flight (their references, so a restart polls
// them instead of submitting again). people_backfill_window is one window of the run: the observations
// it covers by id (never their text), its state and what applying it saved. A pilot window also keeps
// the two models' validated answers (result) until the run applies or supersedes it; those are the only
// sentences these tables hold, and nothing reads them without checking their evidence again.
//
// SQLite only, like the rest of room memory: the application registers the DDL with its feature tables.

// Run states.
const (
	BackfillPiloting   = "piloting"   // the pilot's standard calls are running
	BackfillPiloted    = "piloted"    // the pilot finished: its report and review set are ready
	BackfillSubmitted  = "submitted"  // the operator started the run: batches are submitted and polled
	BackfillStopped    = "stopped"    // halted (stopped_by says why); an operator may continue it
	BackfillDone       = "done"       // no window is queued or in flight
	BackfillSuperseded = "superseded" // a newer plan of the server replaced it; its pilot answers are gone
)

// Window states. An expired batch item goes back to queued.
const (
	WindowQueued    = "queued"
	WindowSubmitted = "submitted"
	WindowApplied   = "applied"
	WindowFailed    = "failed"
	WindowSkipped   = "skipped"
	WindowRefused   = "refused"
)

// SpendPeopleBackfill is the memory_spend kind of the backfill, separate from the derived budget's kinds.
const SpendPeopleBackfill = "people_backfill"

// PeopleBackfillDDL creates the backfill tables (run with the application's feature tables).
var PeopleBackfillDDL = []string{
	`CREATE TABLE IF NOT EXISTS people_backfill_run (
		id TEXT PRIMARY KEY,
		guild_id TEXT NOT NULL,
		state TEXT NOT NULL,
		token TEXT NOT NULL,
		params TEXT NOT NULL,
		model TEXT NOT NULL DEFAULT '',
		provider TEXT NOT NULL DEFAULT '',
		budget_usd REAL NOT NULL DEFAULT 0,
		spent_usd REAL NOT NULL DEFAULT 0,
		stopped_by TEXT NOT NULL DEFAULT '',
		pilot_done INTEGER NOT NULL DEFAULT 0,
		pilot TEXT NOT NULL DEFAULT '',
		batches TEXT NOT NULL DEFAULT '',
		windows INTEGER NOT NULL DEFAULT 0,
		created_at TEXT NOT NULL,
		updated_at TEXT NOT NULL
	)`,
	`CREATE INDEX IF NOT EXISTS idx_people_backfill_run_guild ON people_backfill_run(guild_id, state)`,
	`CREATE TABLE IF NOT EXISTS people_backfill_window (
		run_id TEXT NOT NULL,
		id TEXT NOT NULL,
		room TEXT NOT NULL,
		seq INTEGER NOT NULL,
		lines INTEGER NOT NULL DEFAULT 0,
		obs TEXT NOT NULL,
		first_at TEXT NOT NULL DEFAULT '',
		last_at TEXT NOT NULL DEFAULT '',
		state TEXT NOT NULL,
		pilot INTEGER NOT NULL DEFAULT 0,
		batch TEXT NOT NULL DEFAULT '',
		prompt_hash TEXT NOT NULL DEFAULT '',
		read_at TEXT NOT NULL DEFAULT '',
		attempts INTEGER NOT NULL DEFAULT 0,
		claims INTEGER NOT NULL DEFAULT 0,
		rejected INTEGER NOT NULL DEFAULT 0,
		cost_usd REAL NOT NULL DEFAULT 0,
		result TEXT NOT NULL DEFAULT '',
		updated_at TEXT NOT NULL,
		PRIMARY KEY(run_id, id)
	) WITHOUT ROWID`,
	`CREATE INDEX IF NOT EXISTS idx_people_backfill_window_state ON people_backfill_window(run_id, state, seq)`,
}

// BackfillRun is one people_backfill_run row.
type BackfillRun struct {
	ID        string
	GuildID   string
	State     string
	Token     string // the plan token the operator confirmed
	Params    []byte // the plan's parameters (JSON: rooms, since, until, budget)
	Model     string // the run's model; "" before the run
	Provider  string // config provider id
	BudgetUSD float64
	SpentUSD  float64 // settled from real usage: pilot and run together
	StoppedBy string
	PilotDone bool
	Pilot     []byte // the pilot's report (JSON, counts only)
	Batches   []byte // the batches of the run (JSON, references only)
	Windows   int
	CreatedAt string
	UpdatedAt string
}

// BackfillWindow is one people_backfill_window row.
type BackfillWindow struct {
	RunID      string
	ID         string // the window id, sent as the batch custom_id
	Room       string
	Seq        int // the plan's order
	Lines      int // merged lines (the estimate's unit)
	Obs        []string
	FirstAt    string
	LastAt     string
	State      string
	Pilot      bool
	Batch      string // the key of the batch it is in, while submitted
	PromptHash string // what the model was sent: the answer applies only to the same prompt
	ReadAt     string // when the lines were read for that prompt (RFC 3339)
	Attempts   int
	Claims     int
	Rejected   int
	CostUSD    float64
	Result     []byte // a pilot window's two answers (JSON)
	UpdatedAt  string
}

// backfillQuerier is what both *sql.DB and *sql.Tx offer.
type backfillQuerier interface {
	Exec(query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
}

// ErrBackfillRunState is returned when a run is not in the state a write expected (another writer moved it).
var ErrBackfillRunState = errors.New("people backfill: the run is in another state")

const backfillRunCols = `id,guild_id,state,token,params,model,provider,budget_usd,spent_usd,stopped_by,pilot_done,pilot,batches,windows,created_at,updated_at`

func scanBackfillRun(row interface{ Scan(...any) error }) (BackfillRun, error) {
	var r BackfillRun
	var params, pilot, batches string
	var done int
	err := row.Scan(&r.ID, &r.GuildID, &r.State, &r.Token, &params, &r.Model, &r.Provider, &r.BudgetUSD, &r.SpentUSD, &r.StoppedBy,
		&done, &pilot, &batches, &r.Windows, &r.CreatedAt, &r.UpdatedAt)
	r.Params, r.Pilot, r.Batches, r.PilotDone = []byte(params), []byte(pilot), []byte(batches), done != 0
	return r, err
}

// InsertBackfillRun stores a new run.
func InsertBackfillRun(q backfillQuerier, r BackfillRun) error {
	_, err := q.Exec(`INSERT INTO people_backfill_run(`+backfillRunCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.GuildID, r.State, r.Token, string(r.Params), r.Model, r.Provider, r.BudgetUSD, r.SpentUSD, r.StoppedBy,
		boolInt(r.PilotDone), string(r.Pilot), string(r.Batches), r.Windows, r.CreatedAt, r.UpdatedAt)
	return err
}

// SaveBackfillRun replaces a run's row, but not its spent total (AddBackfillRunSpend only, with the
// windows it settles), when its stored state is one of from (any state when from is empty);
// ErrBackfillRunState otherwise, and sql.ErrNoRows when the run is gone.
func SaveBackfillRun(q backfillQuerier, r BackfillRun, from ...string) error {
	query := `UPDATE people_backfill_run SET state=?,token=?,params=?,model=?,provider=?,budget_usd=?,stopped_by=?,pilot_done=?,pilot=?,batches=?,windows=?,updated_at=? WHERE id=?`
	args := []any{r.State, r.Token, string(r.Params), r.Model, r.Provider, r.BudgetUSD, r.StoppedBy, boolInt(r.PilotDone),
		string(r.Pilot), string(r.Batches), r.Windows, r.UpdatedAt, r.ID}
	if len(from) > 0 {
		query += ` AND state IN (` + placeholders(len(from)) + `)`
		args = append(args, toAny(from)...)
	}
	res, err := q.Exec(query, args...)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		var exists bool
		if err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM people_backfill_run WHERE id=?)`, r.ID).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return sql.ErrNoRows
		}
		return ErrBackfillRunState
	}
	return nil
}

// AddBackfillRunSpend adds usd to a run's spent total.
func AddBackfillRunSpend(q backfillQuerier, id string, usd float64, at string) error {
	_, err := q.Exec(`UPDATE people_backfill_run SET spent_usd=spent_usd+?, updated_at=? WHERE id=?`, usd, at, id)
	return err
}

// LoadBackfillRun reads one run; ok is false when there is none.
func LoadBackfillRun(q backfillQuerier, id string) (BackfillRun, bool, error) {
	r, err := scanBackfillRun(q.QueryRow(`SELECT `+backfillRunCols+` FROM people_backfill_run WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return BackfillRun{}, false, nil
	}
	return r, err == nil, err
}

// BackfillRuns lists runs, newest first: of one server ("" = every server) in one of states (any when
// empty).
func BackfillRuns(q backfillQuerier, guild string, states ...string) ([]BackfillRun, error) {
	query := `SELECT ` + backfillRunCols + ` FROM people_backfill_run WHERE 1=1`
	var args []any
	if guild != "" {
		query += ` AND guild_id=?`
		args = append(args, guild)
	}
	if len(states) > 0 {
		query += ` AND state IN (` + placeholders(len(states)) + `)`
		args = append(args, toAny(states)...)
	}
	rows, err := q.Query(query+` ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BackfillRun
	for rows.Next() {
		r, err := scanBackfillRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

const backfillWindowCols = `run_id,id,room,seq,lines,obs,first_at,last_at,state,pilot,batch,prompt_hash,read_at,attempts,claims,rejected,cost_usd,result,updated_at`

func scanBackfillWindow(row interface{ Scan(...any) error }) (BackfillWindow, error) {
	var w BackfillWindow
	var obs, result string
	var pilot int
	err := row.Scan(&w.RunID, &w.ID, &w.Room, &w.Seq, &w.Lines, &obs, &w.FirstAt, &w.LastAt, &w.State, &pilot, &w.Batch, &w.PromptHash,
		&w.ReadAt, &w.Attempts, &w.Claims, &w.Rejected, &w.CostUSD, &result, &w.UpdatedAt)
	if err != nil {
		return w, err
	}
	w.Pilot, w.Result = pilot != 0, []byte(result)
	if obs != "" {
		if err := json.Unmarshal([]byte(obs), &w.Obs); err != nil {
			return w, err
		}
	}
	return w, nil
}

// InsertBackfillWindows stores a run's windows.
func InsertBackfillWindows(q backfillQuerier, ws []BackfillWindow) error {
	for _, w := range ws {
		obs, err := json.Marshal(w.Obs)
		if err != nil {
			return err
		}
		if _, err := q.Exec(`INSERT INTO people_backfill_window(`+backfillWindowCols+`) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			w.RunID, w.ID, w.Room, w.Seq, w.Lines, string(obs), w.FirstAt, w.LastAt, w.State, boolInt(w.Pilot), w.Batch, w.PromptHash,
			w.ReadAt, w.Attempts, w.Claims, w.Rejected, w.CostUSD, string(w.Result), w.UpdatedAt); err != nil {
			return err
		}
	}
	return nil
}

// SaveBackfillWindow replaces a window's row.
func SaveBackfillWindow(q backfillQuerier, w BackfillWindow) error {
	obs, err := json.Marshal(w.Obs)
	if err != nil {
		return err
	}
	_, err = q.Exec(`UPDATE people_backfill_window SET room=?,seq=?,lines=?,obs=?,first_at=?,last_at=?,state=?,pilot=?,batch=?,prompt_hash=?,read_at=?,
		attempts=?,claims=?,rejected=?,cost_usd=?,result=?,updated_at=? WHERE run_id=? AND id=?`,
		w.Room, w.Seq, w.Lines, string(obs), w.FirstAt, w.LastAt, w.State, boolInt(w.Pilot), w.Batch, w.PromptHash, w.ReadAt,
		w.Attempts, w.Claims, w.Rejected, w.CostUSD, string(w.Result), w.UpdatedAt, w.RunID, w.ID)
	return err
}

// LoadBackfillWindow reads one window of a run; ok is false when there is none.
func LoadBackfillWindow(q backfillQuerier, runID, id string) (BackfillWindow, bool, error) {
	w, err := scanBackfillWindow(q.QueryRow(`SELECT `+backfillWindowCols+` FROM people_backfill_window WHERE run_id=? AND id=?`, runID, id))
	if errors.Is(err, sql.ErrNoRows) {
		return BackfillWindow{}, false, nil
	}
	return w, err == nil, err
}

// BackfillWindows lists a run's windows in plan order: those in one of states (all when empty), the
// pilot's only when pilotOnly is set.
func BackfillWindows(q backfillQuerier, runID string, pilotOnly bool, states ...string) ([]BackfillWindow, error) {
	query := `SELECT ` + backfillWindowCols + ` FROM people_backfill_window WHERE run_id=?`
	args := []any{runID}
	if pilotOnly {
		query += ` AND pilot=1`
	}
	if len(states) > 0 {
		query += ` AND state IN (` + placeholders(len(states)) + `)`
		args = append(args, toAny(states)...)
	}
	rows, err := q.Query(query+` ORDER BY seq`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BackfillWindow
	for rows.Next() {
		w, err := scanBackfillWindow(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// BackfillCounts are a run's window counts by state and what applying them saved.
type BackfillCounts struct {
	States   map[string]int
	Claims   int
	Rejected int
}

// CountBackfillWindows counts a run's windows by state, with the claims saved and the entries dropped.
func CountBackfillWindows(q backfillQuerier, runID string) (BackfillCounts, error) {
	c := BackfillCounts{States: map[string]int{}}
	rows, err := q.Query(`SELECT state, COUNT(*), COALESCE(SUM(claims),0), COALESCE(SUM(rejected),0) FROM people_backfill_window WHERE run_id=? GROUP BY state`, runID)
	if err != nil {
		return c, err
	}
	defer rows.Close()
	for rows.Next() {
		var state string
		var n, claims, rejected int
		if err := rows.Scan(&state, &n, &claims, &rejected); err != nil {
			return c, err
		}
		c.States[state] = n
		c.Claims += claims
		c.Rejected += rejected
	}
	return c, rows.Err()
}

// ClearBackfillResults deletes the pilot answers of a run (the only sentences these tables hold).
func ClearBackfillResults(q backfillQuerier, runID, at string) error {
	_, err := q.Exec(`UPDATE people_backfill_window SET result='', updated_at=? WHERE run_id=? AND result<>''`, at, runID)
	return err
}

// AddBackfillSpend records backfill spend for day (YYYY-MM-DD, UTC) in memory_spend, in the caller's
// transaction (with the window it settles).
func AddBackfillSpend(q backfillQuerier, day string, usd float64, calls int) error {
	if usd == 0 && calls == 0 {
		return nil
	}
	_, err := q.Exec(`INSERT INTO memory_spend(day, kind, usd, calls) VALUES(?,?,?,?) ON CONFLICT(day, kind) DO UPDATE SET usd=memory_spend.usd+excluded.usd, calls=memory_spend.calls+excluded.calls`,
		day, SpendPeopleBackfill, usd, calls)
	return err
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

func placeholders(n int) string {
	return strings.TrimSuffix(strings.Repeat("?,", n), ",")
}
