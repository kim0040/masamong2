package application

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/storage"
)

// Statistics rollup (v1): the derived groups of the registry (stats_registry.go) are computed from the
// rows the install already keeps, one finished local day at a time, and written with statsReplaceDay.
//
// The rules (review of the statistics design, 2026-11-22):
//   - A finished day is final (H1). It is never recomputed unless the operator forces it, and a forced
//     day must lie inside every source's retention window, so pruned sources never replace good totals
//     with nothing.
//   - Pruning waits for the rollup (H3). The daily usage_gc pass finishes days first; then every prune of
//     a source table stops at the end of the last finished day of its group (statsPruneGate). A group
//     that has never been rolled up holds its tables entirely. With stats.enabled false the gate is off.
//   - The cut of every prune of a source table is recorded (statsPrunedKey) before rows are deleted, so a
//     later backfill, a forced day or a day after a time with statistics off can tell a complete day from
//     one that lost rows.
//   - A day is the local day in the zone at rollup time and is finished statsRollupGrace after its
//     midnight (M6). Each finished day records its zone; a day keeps it when forced, and the first day
//     after a zone change starts where the previous one ended, so no row is counted twice or lost.
//   - Reads go through Store.Reader(); each (group, day) is one short write transaction (M4). The rollup
//     runs one at a time per service, off the scheduler tick, and stops between days when the service
//     closes (M5).
//   - llm_calls rows that aggregate a job's calls count call_count calls, add no latency to the
//     histogram and count once as an error (M3). Lab turns are left out (L3). Activity comes from
//     activity_counts, never memory_observations (M9). memory_spend is not copied (M6).
//
// Only body-free columns are read: numbers, closed values and names from code or config.

const (
	// statsRollupGrace lets the ledger queue and the counter flush land before a day is finished.
	statsRollupGrace = 10 * time.Minute
	// statsRollupMaxPause caps the pause between two days of a rollup (it pauses as long as the day
	// took, so a slow disk gets proportionally more room for the writes of replies).
	statsRollupMaxPause = 50 * time.Millisecond
	// statsPrunedKey (admin_state) holds, per source table, the latest cut a prune applied: rows before
	// it may be gone. RFC3339 UTC for timestamp columns, YYYY-MM-DD for activity_counts.
	statsPrunedKey = "stats:pruned"
	statsDayLayout = "2006-01-02"
)

// statsSource is a table a derived group is computed from and the column its day is cut on.
type statsSource struct {
	table, column string
	dayOnly       bool // the column holds the local day (activity_counts.day), not a timestamp
}

// statsSources are the tables each derived group reads; every table feeds one group. The prune rules
// on these tables are gated by that group's last finished day.
var statsSources = map[string][]statsSource{
	statsGroupTurns:         {{table: "turn_log", column: "at"}},
	statsGroupLLM:           {{table: "llm_calls", column: "created_at"}},
	statsGroupTools:         {{table: "tool_calls", column: "created_at"}},
	statsGroupDeliveries:    {{table: "delivery_ledger", column: "created_at"}},
	statsGroupFeedback:      {{table: "turn_feedback", column: "at"}},
	statsGroupParticipation: {{table: "participation_log", column: "created_at"}, {table: "conversation_log", column: "created_at"}},
	statsGroupReminders:     {{table: "reminders", column: "updated_at"}},
	statsGroupJobs:          {{table: "jobs", column: "updated_at"}},
	statsGroupActivity:      {{table: "activity_counts", column: "day", dayOnly: true}},
}

// statsSourceGroup is the group a source table feeds.
func statsSourceGroup(table string) (string, bool) {
	for g, srcs := range statsSources {
		for _, src := range srcs {
			if src.table == table {
				return g, true
			}
		}
	}
	return "", false
}

// statsClaim takes the service's rollup slot. With wait it waits for a running rollup (until ctx ends).
func (s *Service) statsClaim(ctx context.Context, wait bool) (func(), bool) {
	for {
		if s.runs.statsRollup.CompareAndSwap(false, true) {
			return func() { s.runs.statsRollup.Store(false) }, true
		}
		if !wait {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// statsDayHook runs after each finished day (tests stop a backfill midway). Never set in production.
var statsDayHook atomic.Pointer[func(s *Service, ctx context.Context, group, day string)]

// ---------------------------------------------------------------------------
// days

func statsDayStart(day string, loc *time.Location) time.Time {
	t, _ := time.ParseInLocation(statsDayLayout, day, loc)
	return t
}

func statsDayAdd(day string, n int) string {
	t, err := time.Parse(statsDayLayout, day)
	if err != nil {
		return day
	}
	return t.AddDate(0, 0, n).Format(statsDayLayout)
}

// statsFirstDayFrom is the first local day that starts at or after t.
func statsFirstDayFrom(t time.Time, loc *time.Location) string {
	d := t.In(loc).Format(statsDayLayout)
	if statsDayStart(d, loc).Before(t) {
		d = statsDayAdd(d, 1)
	}
	return d
}

// statsLastFinishable is the latest local day the rollup may finish at now.
func statsLastFinishable(now time.Time, loc *time.Location) string {
	return statsDayAdd(now.Add(-statsRollupGrace).In(loc).Format(statsDayLayout), -1)
}

func statsZone(name string, fallback *time.Location) *time.Location {
	if name == "" {
		return fallback
	}
	if loc, err := time.LoadLocation(name); err == nil {
		return loc
	}
	return fallback
}

func statsLaterDay(a, b string) string {
	if b > a {
		return b
	}
	return a
}

// ---------------------------------------------------------------------------
// the rollup

// errStatsStopped is a rollup cut short because its context ended (service shutdown).
var errStatsStopped = errors.New("statistics rollup stopped")

// statsProgress is what the rollup knows about pruning, read once per run.
type statsProgress struct {
	now        time.Time
	loc        *time.Location
	keep       map[string]int    // retention days per key (config.Retention.Days)
	pruned     map[string]string // statsPrunedKey
	everPruned bool              // a prune ran before cuts were recorded (an install upgraded to statistics)
}

// statsKeep is how long rows of a source table stay (-1 = forever): the shortest of the prune rules
// on it.
func statsKeep(days map[string]int, table string) int {
	if table == "jobs" {
		return jobRetentionDays
	}
	keep := -1
	for _, r := range pruneRules {
		if r.table != table {
			continue
		}
		if d := days[r.days]; d >= 0 && (keep < 0 || d < keep) {
			keep = d
		}
	}
	return keep
}

// recordedFrom is the first day of src no recorded prune can have cut ("" = no bound).
func (p statsProgress) recordedFrom(src statsSource) string {
	cut := p.pruned[src.table]
	if cut == "" {
		return ""
	}
	if src.dayOnly {
		return cut
	}
	t, err := time.Parse(time.RFC3339, cut)
	if err != nil {
		return ""
	}
	return statsFirstDayFrom(t, p.loc)
}

// windowFrom is the first day of src inside its retention window ("" = kept forever).
func (p statsProgress) windowFrom(src statsSource) string {
	keep := statsKeep(p.keep, src.table)
	if keep < 0 {
		return ""
	}
	cut := p.now.AddDate(0, 0, -keep)
	if src.dayOnly {
		return cut.In(p.loc).Format(statsDayLayout)
	}
	return statsFirstDayFrom(cut, p.loc)
}

func (s *Service) statsReadProgress(now time.Time, loc *time.Location) statsProgress {
	p := statsProgress{now: now, loc: loc, keep: s.Doc().Retention.Days(), pruned: map[string]string{}}
	_, _ = s.kvGet(statsPrunedKey, &p.pruned)
	if p.pruned == nil {
		p.pruned = map[string]string{}
	}
	var gc scheduleState
	if ok, _ := s.kvGet("schedule:usage_gc", &gc); ok && gc.LastRun != "" {
		p.everPruned = true
	}
	return p
}

// statsRollup finishes the past days of every derived group (or in.Group). Without Since it continues
// each group after its last finished day, or backfills a group never rolled up from the oldest complete
// day of its sources. With Since it goes over every day from Since: unfinished days inside the sources'
// retention are finished, finished ones are rewritten only with Force. The caller holds statsClaim.
func (s *Service) statsRollup(ctx context.Context, in statsRollupInput) (statsRollupOutput, error) {
	loc := s.instanceZone()
	out := statsRollupOutput{Zone: loc.String(), Finished: map[string]int{}, Through: map[string]string{}, Skipped: map[string]string{}}
	if !s.statsOn() {
		return out, errStatsOff
	}
	db, err := s.adminDB()
	if err != nil {
		return out, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	groups := statsDerivedGroups()
	if in.Group != "" {
		if !slices.Contains(groups, in.Group) {
			return out, fmt.Errorf("%w: group %q", errStatsUndeclared, in.Group)
		}
		groups = []string{in.Group}
	}
	doc := s.Doc()
	p := s.statsReadProgress(s.Clock.Now(), loc)
	last := statsLastFinishable(p.now, loc)
	var firstErr error
	for _, g := range groups {
		n, through, why, err := s.statsRollupGroup(ctx, db, doc, g, in, p, last)
		out.Finished[g] = n
		if through != "" {
			out.Through[g] = through
		}
		if why != "" {
			out.Skipped[g] = why
		}
		if err != nil {
			if errors.Is(err, errStatsStopped) {
				return out, err
			}
			s.Log.Warn("statistics rollup failed", "group", g, "err", err)
			if firstErr == nil {
				firstErr = fmt.Errorf("%s: %w", g, err)
			}
		}
	}
	return out, firstErr
}

// statsDayZones lists a group's finished days with the zone each was cut in.
func statsDayZones(ctx context.Context, db *storage.Store, group string) (map[string]string, string, error) {
	rows, err := db.Reader().QueryContext(ctx, `SELECT day, zone FROM stats_days WHERE grp=?`, group)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := map[string]string{}
	through := ""
	for rows.Next() {
		var d, z string
		if err := rows.Scan(&d, &z); err != nil {
			return nil, "", err
		}
		out[d] = z
		through = statsLaterDay(through, d)
	}
	return out, through, rows.Err()
}

// statsOldestDay is the local day of a source table's oldest row ("" when it is empty).
func statsOldestDay(ctx context.Context, db *storage.Store, src statsSource, loc *time.Location) (string, error) {
	var v sql.NullString
	if err := db.Reader().QueryRowContext(ctx, `SELECT MIN(`+src.column+`) FROM `+src.table).Scan(&v); err != nil {
		return "", err
	}
	if !v.Valid || v.String == "" {
		return "", nil
	}
	if src.dayOnly {
		return v.String, nil
	}
	t, err := time.Parse(time.RFC3339, v.String)
	if err != nil {
		return "", nil // an unreadable stamp does not move the start
	}
	return t.In(loc).Format(statsDayLayout), nil
}

// statsRollupGroup finishes one group's days. It returns how many days it finished, the group's last
// finished day, a reason code when days were left out, and the error that stopped it.
func (s *Service) statsRollupGroup(ctx context.Context, db *storage.Store, doc config.Document, g string, in statsRollupInput,
	p statsProgress, last string) (int, string, string, error) {
	zones, through, err := statsDayZones(ctx, db, g)
	if err != nil {
		return 0, "", "failed", err
	}
	// recFrom: no recorded prune cut these days. winFrom: inside every source's retention window.
	recFrom, winFrom, oldest, skipOldest := "", "", "", ""
	for _, src := range statsSources[g] {
		recFrom = statsLaterDay(recFrom, p.recordedFrom(src))
		winFrom = statsLaterDay(winFrom, p.windowFrom(src))
		if in.Since == "" && through == "" {
			o, err := statsOldestDay(ctx, db, src, p.loc)
			if err != nil {
				return 0, through, "failed", err
			}
			if o != "" {
				if oldest == "" || o < oldest {
					oldest = o
				}
				skipOldest = statsLaterDay(skipOldest, statsDayAdd(o, 1))
			}
		}
	}
	var start string
	switch {
	case in.Since != "":
		start = in.Since
	case through != "":
		// after the last finished day the gate kept every row, unless a prune ran ungated since
		// (statistics were off for a while): the record says which days lost rows
		start = statsLaterDay(statsDayAdd(through, 1), recFrom)
	default:
		// first rollup of this group: from the oldest complete day of its sources. A source pruned
		// before cuts were recorded may have lost part of its oldest day and everything outside its
		// window (H1); one that was never pruned is complete.
		start = last
		if oldest != "" {
			start = statsLaterDay(oldest, recFrom)
			if p.everPruned {
				start = statsLaterDay(start, statsLaterDay(winFrom, skipOldest))
			}
		}
	}
	// complete reports whether day d of this group can be computed from what its sources hold now.
	complete := func(d string, finished bool) bool {
		if d < recFrom {
			return false
		}
		if through != "" && d > through {
			return true // the gate kept these rows
		}
		if finished || p.everPruned {
			return d >= winFrom // a forced day, or one before the last finished day: inside every window (H1)
		}
		return true
	}
	n, why := 0, ""
	for d := start; d <= last; d = statsDayAdd(d, 1) {
		if ctx.Err() != nil {
			return n, through, "stopped", errStatsStopped
		}
		zone, finished := zones[d]
		if finished && !in.Force {
			continue
		}
		if !complete(d, finished) {
			why = "outside_retention"
			continue
		}
		began := time.Now()
		dayLoc, zoneName := p.loc, p.loc.String()
		if finished {
			dayLoc, zoneName = statsZone(zone, p.loc), zone // a finished day keeps its zone (M6)
		}
		lo, hi := statsDayStart(d, dayLoc), statsDayStart(statsDayAdd(d, 1), dayLoc)
		if pz, ok := zones[statsDayAdd(d, -1)]; ok {
			// start where the previous finished day ended, also when the zone changed in between
			lo = statsDayStart(d, statsZone(pz, p.loc))
		}
		if hi.Before(lo) {
			hi = lo
		}
		rows, err := s.statsCollect(ctx, db, doc, g, statsSpan{day: d, lo: lo, hi: hi, loc: dayLoc})
		if err != nil {
			if ctx.Err() != nil {
				return n, through, "stopped", errStatsStopped
			}
			return n, through, "failed", fmt.Errorf("%s: %w", d, err)
		}
		err = s.statsReplaceDay(ctx, statsDayWrite{Group: g, Day: d, Zone: zoneName, Force: finished, Rows: rows})
		switch {
		case errors.Is(err, errStatsFinalized):
			// another process (a CLI run on the same database) finished it first
		case err != nil:
			if ctx.Err() != nil {
				return n, through, "stopped", errStatsStopped
			}
			return n, through, "failed", fmt.Errorf("%s: %w", d, err)
		default:
			n++
		}
		zones[d] = zoneName
		through = statsLaterDay(through, d)
		if h := statsDayHook.Load(); h != nil {
			(*h)(s, ctx, g, d)
		}
		select {
		case <-ctx.Done():
			return n, through, "stopped", errStatsStopped
		case <-time.After(min(time.Since(began), statsRollupMaxPause)):
		}
	}
	return n, through, why, nil
}

// statsSpan is the stretch of time one finished day covers.
type statsSpan struct {
	day    string
	lo, hi time.Time
	loc    *time.Location
}

func (sp statsSpan) bounds() (string, string) {
	return sp.lo.UTC().Format(time.RFC3339), sp.hi.UTC().Format(time.RFC3339)
}

// statsScan runs a query on the read pool and hands each row to fn.
func statsScan(ctx context.Context, db *storage.Store, q string, args []any, fn func(*sql.Rows) error) error {
	rows, err := db.Reader().QueryContext(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		if err := fn(rows); err != nil {
			return err
		}
	}
	return rows.Err()
}

func statsB(v bool) float64 {
	if v {
		return 1
	}
	return 0
}

// statsCollect computes one group's rows of one day. One row is made per source row; statsReplaceDay
// adds rows with the same dimensions together.
func (s *Service) statsCollect(ctx context.Context, db *storage.Store, doc config.Document, g string, sp statsSpan) ([]statsRow, error) {
	lo, hi := sp.bounds()
	span := []any{lo, hi}
	var out []statsRow
	add := func(metric string, dims map[string]string, n int64, vals map[string]float64, hist []int64) {
		out = append(out, statsRow{Day: sp.day, Metric: metric, Dims: dims, N: n, Vals: vals, Hist: hist})
	}
	var err error
	switch g {
	case statsGroupTurns:
		err = statsScan(ctx, db, `SELECT at, outcome, hits, cost_usd, latency_ms FROM turn_log WHERE at>=? AND at<? AND lab=0`, span,
			func(r *sql.Rows) error {
				var at, outcome string
				var hits, latency int64
				var cost float64
				if err := r.Scan(&at, &outcome, &hits, &cost, &latency); err != nil {
					return err
				}
				vals := map[string]float64{"cost_usd": cost, "hits": float64(hits)}
				var hist []int64
				if outcome == "answered" {
					vals["latency_ms"] = float64(latency)
					hist = statsHistAdd(nil, latency)
				}
				add("turns.count", map[string]string{"outcome": outcome}, 1, vals, hist)
				hour := ""
				if t, err := time.Parse(time.RFC3339, at); err == nil {
					hour = strconv.Itoa(t.In(sp.loc).Hour())
				}
				add("turns.hour", map[string]string{"hour": hour}, 1, nil, nil)
				return nil
			})
	case statsGroupLLM:
		err = statsScan(ctx, db, `SELECT role, model, provider, call_count, prompt_tokens, completion_tokens, cached_tokens, latency_ms,
			cost_usd, class, fallback, finish_reason, job_id FROM llm_calls WHERE created_at>=? AND created_at<?`, span,
			func(r *sql.Rows) error {
				var role, model, provider, class, finish, job string
				var calls, prompt, completion, cached, latency, fallback int64
				var cost float64
				if err := r.Scan(&role, &model, &provider, &calls, &prompt, &completion, &cached, &latency, &cost, &class, &fallback, &finish, &job); err != nil {
					return err
				}
				calls = max(calls, 1)
				failed := class != "" && class != "none"
				vals := map[string]float64{"prompt_tokens": float64(prompt), "completion_tokens": float64(completion),
					"cached_tokens": float64(cached), "cost_usd": cost, "errors": statsB(failed), "fallback": statsB(fallback != 0),
					"job_rows": statsB(job != "")}
				if prompt+completion > 0 && cost == 0 {
					vals["zero_cost"] = float64(calls)
				}
				var hist []int64
				if calls == 1 { // a job aggregate's latency is the sum of its calls (M3)
					hist = statsHistAdd(nil, latency)
				}
				add("llm.calls", map[string]string{"role": role, "model": model, "provider": statsProviderFamily(doc, provider)}, calls, vals, hist)
				if failed { // a job aggregate counts once: at least one of its calls failed
					add("llm.errors", map[string]string{"role": role, "class": class}, 1, nil, nil)
				}
				add("llm.finish", map[string]string{"role": role, "finish": finish}, calls, nil, nil)
				return nil
			})
	case statsGroupTools:
		err = statsScan(ctx, db, `SELECT tool, guard, status, latency_ms, cost_usd, result_chars, class, provider FROM tool_calls
			WHERE created_at>=? AND created_at<?`, span,
			func(r *sql.Rows) error {
				var tool, guard, status, class, provider string
				var latency, chars int64
				var cost float64
				if err := r.Scan(&tool, &guard, &status, &latency, &cost, &chars, &class, &provider); err != nil {
					return err
				}
				add("tools.guard", map[string]string{"tool": tool, "guard": guard}, 1, nil, nil)
				if guard == "skipped" { // held back by the turn's guard: not an execution
					return nil
				}
				add("tools.calls", map[string]string{"tool": tool, "status": status, "class": class, "provider": statsProviderFamily(doc, provider)}, 1,
					map[string]float64{"cost_usd": cost, "result_chars": float64(chars), "latency_ms": float64(latency)}, statsHistAdd(nil, latency))
				return nil
			})
	case statsGroupDeliveries:
		err = statsScan(ctx, db, `SELECT parts, sent, retries, class, reference_dropped FROM delivery_ledger WHERE created_at>=? AND created_at<?`, span,
			func(r *sql.Rows) error {
				var parts, sent, retries, dropped int64
				var class string
				if err := r.Scan(&parts, &sent, &retries, &class, &dropped); err != nil {
					return err
				}
				add("deliveries.count", map[string]string{"class": class}, 1, map[string]float64{"parts": float64(parts), "sent": float64(sent),
					"retries": float64(retries), "reference_dropped": statsB(dropped != 0), "incomplete": statsB(sent < parts)}, nil)
				return nil
			})
	case statsGroupFeedback:
		err = statsScan(ctx, db, `SELECT source, rating, reason FROM turn_feedback WHERE at>=? AND at<?`, span,
			func(r *sql.Rows) error {
				var source, reason string
				var rating int64
				if err := r.Scan(&source, &rating, &reason); err != nil {
					return err
				}
				if kind, ok := strings.CutPrefix(source, "implicit:"); ok {
					add("feedback.implicit", map[string]string{"kind": kind}, 1, nil, nil)
					return nil
				}
				add("feedback.ratings", map[string]string{"source": source, "rating": strconv.FormatInt(rating, 10), "reason": reason}, 1, nil, nil)
				return nil
			})
	case statsGroupParticipation:
		err = statsScan(ctx, db, `SELECT mode, kind, state, cost_usd FROM participation_log WHERE created_at>=? AND created_at<?`, span,
			func(r *sql.Rows) error {
				var mode, kind, state string
				var cost float64
				if err := r.Scan(&mode, &kind, &state, &cost); err != nil {
					return err
				}
				add("participation.decisions", map[string]string{"mode": mode, "kind": kind, "state": state}, 1, map[string]float64{"cost_usd": cost}, nil)
				return nil
			})
		if err == nil {
			err = statsScan(ctx, db, `SELECT kind, decision, outcome, cost_usd FROM conversation_log WHERE created_at>=? AND created_at<?`, span,
				func(r *sql.Rows) error {
					var kind, decision, outcome string
					var cost float64
					if err := r.Scan(&kind, &decision, &outcome, &cost); err != nil {
						return err
					}
					add("participation.conversation", map[string]string{"kind": kind, "decision": decision, "outcome": outcome}, 1,
						map[string]float64{"cost_usd": cost}, nil)
					return nil
				})
		}
	case statsGroupReminders:
		// a reminder counts on the day of its last change, with the status it has then
		err = statsScan(ctx, db, `SELECT kind, status, due_at, attempts, updated_at FROM reminders WHERE updated_at>=? AND updated_at<?`, span,
			func(r *sql.Rows) error {
				var kind, status, due, updated string
				var attempts int64
				if err := r.Scan(&kind, &status, &due, &attempts, &updated); err != nil {
					return err
				}
				late := "none"
				if status == "sent" && due != "" {
					dt, err1 := time.Parse(time.RFC3339, due)
					ut, err2 := time.Parse(time.RFC3339, updated)
					if err1 == nil && err2 == nil {
						late = statsLatenessOf(ut.Sub(dt))
					}
				}
				add("reminders.status", map[string]string{"kind": kind, "status": status, "lateness": late}, 1,
					map[string]float64{"attempts": float64(attempts)}, nil)
				return nil
			})
	case statsGroupJobs:
		// jobs that ended that day (a job still running has no outcome yet)
		err = statsScan(ctx, db, `SELECT type, status, COALESCE(error_code,'') FROM jobs WHERE updated_at>=? AND updated_at<? AND status IN (`+finishedJobStates+`)`, span,
			func(r *sql.Rows) error {
				var typ, status, code string
				if err := r.Scan(&typ, &status, &code); err != nil {
					return err
				}
				if code == "" && status == "succeeded" {
					code = "ok"
				}
				add("jobs.runs", map[string]string{"type": typ, "status": status, "code": code}, 1, nil, nil)
				return nil
			})
	case statsGroupActivity:
		// activity_counts keeps the local day it was counted in (M9: never memory_observations)
		var messages int64
		rooms, people := map[string]bool{}, map[string]bool{}
		err = statsScan(ctx, db, `SELECT guild_id, channel_id, user_id, n FROM activity_counts WHERE day=?`, []any{sp.day},
			func(r *sql.Rows) error {
				var guild, channel, user string
				var n int64
				if err := r.Scan(&guild, &channel, &user, &n); err != nil {
					return err
				}
				messages += n
				rooms[guild+"\x00"+channel], people[user] = true, true
				return nil
			})
		if err == nil && messages > 0 {
			add("activity.messages", nil, messages, nil, nil)
			add("activity.rooms", nil, int64(len(rooms)), nil, nil)
			add("activity.people", nil, int64(len(people)), nil, nil)
		}
	default:
		return nil, fmt.Errorf("%w: group %q has no rollup", errStatsUndeclared, g)
	}
	return out, err
}

// ---------------------------------------------------------------------------
// the prune gate (review H3)

// statsPruneGate says how far each source table may be pruned: up to the end of its group's last
// finished day, nothing while the group was never rolled up. Off (no limits) with statistics off.
type statsPruneGate struct {
	on   bool
	ts   map[string]string // table → RFC3339 UTC: rows before it belong to finished days
	day  map[string]string // day-column table → its first unfinished day
	hold map[string]string // table → why nothing may be pruned
	why  map[string]string // table → why a cut was lowered
}

func (s *Service) statsGate(ctx context.Context, rolled *statsRollupOutput, rollErr string) statsPruneGate {
	g := statsPruneGate{ts: map[string]string{}, day: map[string]string{}, hold: map[string]string{}, why: map[string]string{}}
	if !s.statsOn() {
		return g
	}
	g.on = true
	db, err := s.adminDB()
	loc := s.instanceZone()
	for group, srcs := range statsSources {
		var zones map[string]string
		through := ""
		if err == nil {
			zones, through, err = statsDayZones(ctx, db, group)
		}
		// why the rollup did not get further this pass, when it did not
		problem := ""
		if rolled != nil {
			if code := rolled.Skipped[group]; code == "failed" || code == "stopped" {
				problem = " (rollup " + code
				if rollErr != "" {
					problem += ": " + rollErr
				}
				problem += ")"
			}
		} else if rollErr != "" {
			problem = " (rollup did not run: " + rollErr + ")"
		}
		for _, src := range srcs {
			switch {
			case err != nil:
				g.hold[src.table] = "statistics state unreadable: " + err.Error()
			case through == "":
				g.hold[src.table] = "waiting for the first " + group + " statistics rollup" + problem
			case src.dayOnly:
				g.day[src.table] = statsDayAdd(through, 1)
				g.why[src.table] = "the " + group + " statistics are finished through " + through + problem
			default:
				g.ts[src.table] = statsDayStart(statsDayAdd(through, 1), statsZone(zones[through], loc)).UTC().Format(time.RFC3339)
				g.why[src.table] = "the " + group + " statistics are finished through " + through + problem
			}
		}
	}
	return g
}

// bound lowers a prune cutoff of table to what the gate allows. "" means the table is not pruned now;
// the second result says why the cutoff was lowered or held.
func (g statsPruneGate) bound(table string, dayOnly bool, cutoff string) (string, string) {
	if !g.on {
		return cutoff, ""
	}
	if _, src := statsSourceGroup(table); !src {
		return cutoff, ""
	}
	if why, held := g.hold[table]; held {
		return "", why
	}
	limit := g.ts[table]
	if dayOnly {
		limit = g.day[table]
	}
	if limit != "" && limit < cutoff {
		return limit, g.why[table] + "; kept from " + limit
	}
	return cutoff, ""
}

// statsNotePruned records prune cuts of source tables before the rows go (each keeps its latest cut).
func (s *Service) statsNotePruned(cuts map[string]string) error {
	if len(cuts) == 0 {
		return nil
	}
	rec := map[string]string{}
	if _, err := s.kvGet(statsPrunedKey, &rec); err != nil {
		return err
	}
	if rec == nil {
		rec = map[string]string{}
	}
	changed := false
	for table, cut := range cuts {
		if cut > rec[table] {
			rec[table], changed = cut, true
		}
	}
	if !changed {
		return nil
	}
	return s.kvSet(statsPrunedKey, rec)
}

// ---------------------------------------------------------------------------
// stats.rollup (operator only: CLI or console)

func (s *Service) statsRollupAction(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	input := statsRollupInput{Group: strVal(in, "group", ""), Since: strVal(in, "since", ""), Force: boolVal(in, "force")}
	if input.Group != "" && !slices.Contains(statsDerivedGroups(), input.Group) {
		return action.Fail(req, errcode.InvalidInput, "generic.error", fmt.Sprintf("group must be one of %v", statsDerivedGroups()), nil)
	}
	if input.Since != "" {
		if _, err := time.Parse(statsDayLayout, input.Since); err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "since must be a day (YYYY-MM-DD)", nil)
		}
	}
	if input.Force && input.Since == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "force rewrites finished days from since: give since", nil)
	}
	if !s.statsOn() {
		return action.Fail(req, errcode.FeatureDisabled, "generic.error", errStatsOff.Error(), nil)
	}
	wctx, done, ok := s.detachedWork(ctx)
	if !ok {
		return action.Fail(req, errcode.IO, "generic.error", "service is shutting down", nil)
	}
	defer done()
	release, ok := s.statsClaim(wctx, true)
	if !ok {
		return action.Fail(req, errcode.IO, "generic.error", "service is shutting down", nil)
	}
	out, err := s.statsRollup(wctx, input)
	release()
	res := map[string]any{"zone": out.Zone, "finished": out.Finished, "through": out.Through, "skipped": out.Skipped}
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), res)
	}
	return action.OK(req, res)
}
