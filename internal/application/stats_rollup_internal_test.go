package application

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/features/participation"
)

var statsKST = func() *time.Location {
	loc, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		panic(err)
	}
	return loc
}()

// statsRollupService is a service with a movable clock (2026-10-11 09:00 UTC, 18:00 in Seoul) and the
// instance zone set to zone.
func statsRollupService(t *testing.T, zone string) (*Service, *movableClock) {
	t.Helper()
	s, clk := deliveryService(t)
	statsSetZone(s, zone)
	return s, clk
}

func statsSetZone(s *Service, zone string) {
	d := s.Doc()
	d.I18n.Timezone = zone
	s.setDoc(d, s.Revision())
}

func statsMust(t *testing.T, s *Service, q string, args ...any) {
	t.Helper()
	if _, err := s.Store.DB.Exec(q, args...); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
}

func statsCount(t *testing.T, s *Service, q string, args ...any) int {
	t.Helper()
	var n int
	if err := s.Store.DB.QueryRow(q, args...).Scan(&n); err != nil {
		t.Fatalf("%s: %v", q, err)
	}
	return n
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

var statsTurnSeq int

func statsTurn(t *testing.T, s *Service, at time.Time, outcome string, lab int, latency, hits int, cost float64) {
	t.Helper()
	statsTurnSeq++
	statsMust(t, s, `INSERT INTO turn_log(id, at, day, hour, guild_id, channel_id, outcome, tools, hits, cost_usd, latency_ms, lab, body)
		VALUES(?,?,?,?, 'g', 'c', ?, '', ?, ?, ?, ?, '{}')`, fmt.Sprintf("st-%d", statsTurnSeq), stamp(at), at.Format("2006-01-02"),
		at.Hour(), outcome, hits, cost, latency, lab)
}

func statsLLM(t *testing.T, s *Service, at time.Time, role, model string, calls, prompt, completion, latency int, cost float64, class, finish, job string) {
	t.Helper()
	statsMust(t, s, `INSERT INTO llm_calls(turn_id, job_id, role, call_count, model, provider, prompt_tokens, completion_tokens, cached_tokens,
		latency_ms, cost_usd, class, fallback, finish_reason, created_at) VALUES('', ?, ?, ?, ?, '', ?, ?, 0, ?, ?, ?, 0, ?, ?)`,
		job, role, calls, model, prompt, completion, latency, cost, class, finish, stamp(at))
}

func statsRollupNow(t *testing.T, s *Service, in statsRollupInput) statsRollupOutput {
	t.Helper()
	release, ok := s.statsClaim(context.Background(), false)
	if !ok {
		t.Fatal("a rollup is already running")
	}
	defer release()
	out, err := s.statsRollup(context.Background(), in)
	if err != nil {
		t.Fatalf("rollup: %v (%+v)", err, out)
	}
	return out
}

func statsDaysOf(t *testing.T, s *Service, group string) []string {
	t.Helper()
	days, err := s.statsFinishedDays(context.Background(), group, "", "")
	if err != nil {
		t.Fatal(err)
	}
	return days
}

func statsN(t *testing.T, s *Service, day, metric, dims string) int64 {
	t.Helper()
	n, _, _ := statsRead(t, s, day, metric, dims)
	return n
}

func histTotal(h []int64) int64 {
	var n int64
	for _, c := range h {
		n += c
	}
	return n
}

// Every derived group is computed from the rows that exist, one finished local day at a time: lab
// turns are left out (L3), an llm_calls row that aggregates a job's calls counts its calls, adds no
// latency to the histogram and counts once as an error (M3), and a day without rows still finishes.
func TestStatsRollupComputesEachGroupFromItsSources(t *testing.T) {
	s, _ := statsRollupService(t, "Asia/Seoul")
	at := time.Date(2026, 10, 9, 1, 30, 0, 0, statsKST) // 2026-10-08 16:30 UTC: the local day decides
	statsTurn(t, s, at, "answered", 0, 900, 2, 0.1)
	statsTurn(t, s, at.Add(time.Minute), "answered", 0, 3000, 3, 0.2)
	statsTurn(t, s, at.Add(2*time.Minute), "error", 0, 50, 0, 0)
	statsTurn(t, s, at.Add(3*time.Minute), "answered", 1, 100, 9, 9) // a lab turn
	statsLLM(t, s, at, "chat", "gpt-x", 1, 100, 20, 800, 0.01, "", "stop", "")
	statsLLM(t, s, at, "memory_summary", "gpt-x", 50, 5000, 0, 40000, 0, "timeout", "", "job-1") // a job aggregate
	statsLLM(t, s, at, "chat", "gpt-x", 1, 0, 0, 200, 0, "rate_limited", "", "")
	statsMust(t, s, `INSERT INTO tool_calls(turn_id, tool, guard, status, latency_ms, cost_usd, result_chars, class, provider, created_at)
		VALUES('', 'search.web', 'allowed', 'ok', 1200, 0.002, 500, '', '', ?), ('', 'search.web', 'skipped', 'skipped', 0, 0, 0, '', '', ?)`, stamp(at), stamp(at))
	statsMust(t, s, `INSERT INTO delivery_ledger(turn_id, guild_id, channel_id, parts, sent, retries, class, reference_dropped, created_at)
		VALUES('t1', 'g', 'c', 3, 2, 1, 'overloaded', 0, ?), ('t2', 'g', 'c', 1, 1, 0, '', 1, ?)`, stamp(at), stamp(at))
	statsMust(t, s, `INSERT INTO turn_feedback(turn_id, user_hash, source, rating, reason, at) VALUES('t1', 'h1', 'menu', -1, 'wrong', ?),
		('t1', 'h2', 'implicit:thanks', 1, '', ?)`, stamp(at), stamp(at))
	statsMust(t, s, `INSERT INTO participation_log(id, guild_id, channel_id, created_at, mode, kind, state, cost_usd) VALUES('p1', 'g', 'c', ?, 'shadow', ?, ?, 0.003)`,
		stamp(at), participation.Question, partShadow)
	statsMust(t, s, `INSERT INTO conversation_log(id, guild_id, channel_id, created_at, kind, decision, outcome, cost_usd) VALUES('cv1', 'g', 'c', ?, ?, 'skip_cap', '', 0)`,
		at.UTC().Format(time.RFC3339Nano), convFollow)
	statsMust(t, s, `INSERT INTO reminders(id, owner_id, content, timezone, status, due_at, attempts, created_at, updated_at)
		VALUES('r1', 'u1', 'x', 'Asia/Seoul', 'sent', ?, 1, ?, ?)`, stamp(at.Add(-3*time.Minute)), stamp(at.Add(-time.Hour)), stamp(at))
	statsMust(t, s, `INSERT INTO jobs(id, type, status, error_code, created_at, updated_at) VALUES('j1', ?, 'succeeded', '', ?, ?),
		('j2', ?, 'failed', ?, ?, ?), ('j3', ?, 'running', '', ?, ?)`, jobPrepare, stamp(at), stamp(at),
		jobPrepare, errcode.IO, stamp(at), stamp(at), jobPrepare, stamp(at), stamp(at))
	statsMust(t, s, `INSERT INTO activity_counts(day, guild_id, channel_id, user_id, name, n, last_at) VALUES('2026-10-09', 'g', 'c1', 'u1', '', 3, ?),
		('2026-10-09', 'g', 'c1', 'u2', '', 2, ?), ('2026-10-09', 'g', 'c2', 'u1', '', 1, ?)`, stamp(at), stamp(at), stamp(at))

	out := statsRollupNow(t, s, statsRollupInput{})
	for _, g := range statsDerivedGroups() {
		if out.Finished[g] != 2 || out.Through[g] != "2026-10-10" {
			t.Errorf("%s: finished %d through %q, want 2 days through 2026-10-10 (%+v)", g, out.Finished[g], out.Through[g], out)
		}
		if days := statsDaysOf(t, s, g); !slices.Equal(days, []string{"2026-10-09", "2026-10-10"}) {
			t.Errorf("%s: finished days %v", g, days)
		}
	}
	if out.Zone != "Asia/Seoul" {
		t.Fatalf("zone %q", out.Zone)
	}
	day := "2026-10-09"
	n, vals, hist := statsRead(t, s, day, "turns.count", "outcome=answered")
	if n != 2 || math.Abs(vals["cost_usd"]-0.3) > 1e-9 || vals["hits"] != 5 || vals["latency_ms"] != 3900 || histTotal(hist) != 2 {
		t.Fatalf("answered turns (the lab turn is left out): n=%d vals=%v hist=%v", n, vals, hist)
	}
	if n, vals, hist := statsRead(t, s, day, "turns.count", "outcome=error"); n != 1 || vals["latency_ms"] != 0 || hist != nil {
		t.Fatalf("error turns carry no latency: n=%d vals=%v hist=%v", n, vals, hist)
	}
	if n := statsN(t, s, day, "turns.hour", "hour=01"); n != 3 {
		t.Fatalf("turns by local hour: %d", n)
	}
	n, vals, hist = statsRead(t, s, day, "llm.calls", "role=chat|model=gpt-x|provider=none")
	if n != 2 || vals["errors"] != 1 || vals["prompt_tokens"] != 100 || vals["zero_cost"] != 0 || histTotal(hist) != 2 {
		t.Fatalf("single calls: n=%d vals=%v hist=%v", n, vals, hist)
	}
	n, vals, hist = statsRead(t, s, day, "llm.calls", "role=memory_summary|model=gpt-x|provider=none")
	if n != 50 || vals["job_rows"] != 1 || vals["zero_cost"] != 50 || vals["errors"] != 1 || hist != nil {
		t.Fatalf("a job aggregate is 50 calls, one error, no latency in the histogram (M3): n=%d vals=%v hist=%v", n, vals, hist)
	}
	for dims, want := range map[string]int64{"role=memory_summary|class=timeout": 1, "role=chat|class=rate_limited": 1} {
		if got := statsN(t, s, day, "llm.errors", dims); got != want {
			t.Errorf("llm.errors %s = %d", dims, got)
		}
	}
	if statsN(t, s, day, "llm.finish", "role=memory_summary|finish=none") != 50 || statsN(t, s, day, "llm.finish", "role=chat|finish=stop") != 1 {
		t.Fatal("llm.finish counts calls by finish bucket")
	}
	if n, vals, _ := statsRead(t, s, day, "tools.calls", "tool=search.web|status=ok|class=none|provider=none"); n != 1 || vals["result_chars"] != 500 {
		t.Fatalf("tool executions: n=%d vals=%v", n, vals)
	}
	if statsN(t, s, day, "tools.guard", "tool=search.web|guard=allowed") != 1 || statsN(t, s, day, "tools.guard", "tool=search.web|guard=skipped") != 1 {
		t.Fatal("tool steps by guard")
	}
	if statsCount(t, s, `SELECT COUNT(*) FROM stats_daily WHERE metric='tools.calls'`) != 1 {
		t.Fatal("a step the guard held back is not an execution")
	}
	if _, vals, _ := statsRead(t, s, day, "deliveries.count", "class=overloaded"); vals["incomplete"] != 1 || vals["parts"] != 3 || vals["sent"] != 2 {
		t.Fatalf("deliveries: %v", vals)
	}
	if _, vals, _ := statsRead(t, s, day, "deliveries.count", "class=none"); vals["reference_dropped"] != 1 || vals["incomplete"] != 0 {
		t.Fatalf("deliveries: %v", vals)
	}
	for _, c := range []struct{ metric, dims string }{
		{"feedback.ratings", "source=menu|rating=bad|reason=wrong"},
		{"feedback.implicit", "kind=thanks"},
		{"participation.decisions", "mode=shadow|kind=" + participation.Question + "|state=" + partShadow},
		{"participation.conversation", "kind=" + convFollow + "|decision=skip_cap|outcome=none"}, // open: the export folds it to skip
		{"reminders.status", "kind=reminder|status=sent|lateness=late_5m"},
		{"jobs.runs", "type=" + jobPrepare + "|status=succeeded|code=ok"},
		{"jobs.runs", "type=" + jobPrepare + "|status=failed|code=" + errcode.IO},
	} {
		if got := statsN(t, s, day, c.metric, c.dims); got != 1 {
			t.Errorf("%s %s = %d, want 1", c.metric, c.dims, got)
		}
	}
	if statsCount(t, s, `SELECT COUNT(*) FROM stats_daily WHERE metric='jobs.runs'`) != 2 {
		t.Fatal("a job still running has no outcome yet")
	}
	for metric, want := range map[string]int64{"activity.messages": 6, "activity.rooms": 2, "activity.people": 2} {
		if got := statsN(t, s, day, metric, ""); got != want {
			t.Errorf("%s = %d, want %d", metric, got, want)
		}
	}
	// an empty day is finished with no rows
	if statsCount(t, s, `SELECT COUNT(*) FROM stats_daily WHERE day='2026-10-10'`) != 0 {
		t.Fatal("an empty day holds rows")
	}
	// a second run has nothing to do and changes nothing
	rows := statsCount(t, s, `SELECT COUNT(*) FROM stats_daily`)
	again := statsRollupNow(t, s, statsRollupInput{})
	for g, n := range again.Finished {
		if n != 0 {
			t.Errorf("%s finished %d days again", g, n)
		}
	}
	if statsCount(t, s, `SELECT COUNT(*) FROM stats_daily`) != rows {
		t.Fatal("a second run changed the totals")
	}
}

// An install upgraded to statistics has pruned before cuts were recorded: its first backfill starts
// inside every source's retention window and skips each source's oldest surviving day, which a prune
// may have cut in half (H1). An install that never pruned starts at its oldest row.
func TestStatsBackfillAfterUpgradeSkipsPartialDays(t *testing.T) {
	for _, c := range []struct {
		name     string
		upgraded bool
		back     []int // days back of the rows (ledger_days is 90)
		from     func(clk time.Time, day func(int) string) string
	}{
		{"never_pruned", false, []int{95, 60, 59}, func(_ time.Time, day func(int) string) string { return day(95) }},
		// rows outside the window may be partly pruned: start at the window's first whole day
		{"pruned_outside_window", true, []int{95, 60, 59}, func(clk time.Time, _ func(int) string) string {
			return statsFirstDayFrom(clk.AddDate(0, 0, -90), statsKST)
		}},
		// the oldest surviving day may have been cut in half by the last prune: skip it
		{"pruned_oldest_day", true, []int{60, 59}, func(_ time.Time, day func(int) string) string { return day(59) }},
	} {
		t.Run(c.name, func(t *testing.T) {
			s, clk := statsRollupService(t, "Asia/Seoul")
			if c.upgraded {
				if err := s.kvSet("schedule:usage_gc", scheduleState{LastRun: stamp(clk.t.Add(-time.Hour)), OK: true}); err != nil {
					t.Fatal(err)
				}
			}
			day := func(back int) string { return clk.t.AddDate(0, 0, -back).In(statsKST).Format("2006-01-02") }
			for _, back := range c.back {
				statsLLM(t, s, statsDayStart(day(back), statsKST).Add(12*time.Hour), "chat", "gpt-x", 1, 1, 1, 1, 0, "", "", "")
			}
			statsRollupNow(t, s, statsRollupInput{Group: statsGroupLLM})
			days := statsDaysOf(t, s, statsGroupLLM)
			want := c.from(clk.t, day)
			if len(days) == 0 || days[0] != want || days[len(days)-1] != statsLastFinishable(clk.t, statsKST) {
				t.Fatalf("backfill %v .. %v, want from %s", days[0], days[len(days)-1], want)
			}
		})
	}
}

// The day is the local day in the zone at rollup time; a finished day keeps its zone, and the first day
// after a zone change starts where the last finished one ended, so every row counts exactly once (M6).
func TestStatsRollupZoneChangeCountsEveryRowOnce(t *testing.T) {
	s, clk := statsRollupService(t, "Asia/Seoul")
	ctx := context.Background()
	turn := func(at time.Time) { statsTurn(t, s, at, "answered", 0, 100, 0, 0) }
	utc := func(mo time.Month, d, h int) time.Time { return time.Date(2026, mo, d, h, 0, 0, 0, time.UTC) }
	turn(utc(10, 10, 1)) // 2026-10-10 10:00 in Seoul
	statsRollupNow(t, s, statsRollupInput{Group: statsGroupTurns})
	if n := statsN(t, s, "2026-10-10", "turns.count", "outcome=answered"); n != 1 {
		t.Fatalf("Seoul day: %d", n)
	}
	// Seoul (+9) → UTC: Seoul's 2026-10-10 ended at 15:00 UTC; UTC's 2026-10-11 starts there, not at midnight
	statsSetZone(s, "UTC")
	turn(utc(10, 10, 16))
	turn(utc(10, 11, 12))
	clk.t = utc(10, 12, 9)
	statsRollupNow(t, s, statsRollupInput{Group: statsGroupTurns})
	if n := statsN(t, s, "2026-10-11", "turns.count", "outcome=answered"); n != 2 {
		t.Fatalf("first UTC day after the change: %d turns, want 2 (none lost in the gap)", n)
	}
	// UTC → Seoul: UTC's 2026-10-11 ended at midnight UTC, after Seoul's 2026-10-12 began
	statsSetZone(s, "Asia/Seoul")
	turn(utc(10, 11, 20)) // inside the finished UTC day: must not be counted again on the next day
	turn(utc(10, 12, 5))
	clk.t = utc(10, 13, 9)
	statsRollupNow(t, s, statsRollupInput{Group: statsGroupTurns})
	if n := statsN(t, s, "2026-10-12", "turns.count", "outcome=answered"); n != 1 {
		t.Fatalf("first Seoul day after the change: %d turns, want 1 (no overlap)", n)
	}
	zones := map[string]string{}
	rows, err := s.Store.DB.Query(`SELECT day, zone FROM stats_days WHERE grp='turns'`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var d, z string
		_ = rows.Scan(&d, &z)
		zones[d] = z
	}
	rows.Close()
	if zones["2026-10-10"] != "Asia/Seoul" || zones["2026-10-11"] != "UTC" || zones["2026-10-12"] != "Asia/Seoul" {
		t.Fatalf("zones %v", zones)
	}
	// the prune gate ends where the last finished day ended, in that day's zone
	if g := s.statsGate(ctx, nil, ""); g.ts["turn_log"] != "2026-10-12T15:00:00Z" {
		t.Fatalf("gate %q", g.ts["turn_log"])
	}
	// a forced day keeps the zone it was finished in: UTC's 2026-10-11 now also holds the late row
	statsRollupNow(t, s, statsRollupInput{Group: statsGroupTurns, Since: "2026-10-11", Force: true})
	if n := statsN(t, s, "2026-10-11", "turns.count", "outcome=answered"); n != 3 {
		t.Fatalf("forced UTC day: %d", n)
	}
	var total int64
	_ = s.Store.DB.QueryRow(`SELECT SUM(n) FROM stats_daily WHERE metric='turns.count'`).Scan(&total)
	if total != 5 {
		t.Fatalf("%d turns counted, 5 written", total)
	}
	var zone string
	_ = s.Store.DB.QueryRow(`SELECT zone FROM stats_days WHERE grp='turns' AND day='2026-10-11'`).Scan(&zone)
	if zone != "UTC" {
		t.Fatalf("a forced day moved to %q", zone)
	}
}

func TestStatsRollupAction(t *testing.T) {
	s, _ := statsRollupService(t, "Asia/Seoul")
	run := func(in map[string]any) action.Result {
		raw, _ := json.Marshal(in)
		return s.Execute(context.Background(), action.Request{Action: "stats.rollup", Input: raw, Locale: "en"})
	}
	at := time.Date(2026, 10, 9, 12, 0, 0, 0, statsKST)
	statsTurn(t, s, at, "answered", 0, 100, 0, 0)
	r := run(nil)
	if !r.OK || r.Output["through"].(map[string]string)[statsGroupTurns] != "2026-10-10" || r.Output["zone"] != "Asia/Seoul" {
		t.Fatalf("rollup: %+v %+v", r.Output, r.Error)
	}
	for _, in := range []map[string]any{{"group": "actions"}, {"group": "nope"}, {"force": true}, {"since": "10/09/2026"}} {
		if r := run(in); r.OK || r.Error.Code != errcode.InvalidInput {
			t.Errorf("%v accepted: %+v", in, r.Error)
		}
	}
	statsTurn(t, s, at.Add(time.Hour), "answered", 0, 100, 0, 0)
	if r := run(map[string]any{"group": "turns", "since": "2026-10-09"}); !r.OK || r.Output["finished"].(map[string]int)[statsGroupTurns] != 0 {
		t.Fatalf("finished day rewritten without force: %+v", r)
	}
	if r := run(map[string]any{"group": "turns", "since": "2026-10-09", "force": true}); !r.OK || r.Output["finished"].(map[string]int)[statsGroupTurns] != 2 {
		t.Fatalf("forced: %+v %+v", r.Output, r.Error)
	}
	if n := statsN(t, s, "2026-10-09", "turns.count", "outcome=answered"); n != 2 {
		t.Fatalf("forced day: %d", n)
	}
	// a day outside a source's retention window is refused even before any prune ran (H1): its rows
	// may go at the next pass
	old := time.Date(2026, 7, 1, 12, 0, 0, 0, statsKST) // past ledger_days (90)
	statsLLM(t, s, old, "chat", "gpt-x", 1, 1, 1, 1, 0, "", "", "")
	if r := run(map[string]any{"group": "llm", "since": "2026-07-01"}); !r.OK || r.Output["finished"].(map[string]int)[statsGroupLLM] == 0 {
		t.Fatalf("unfinished days of a source never pruned: %+v %+v", r.Output, r.Error)
	}
	r = run(map[string]any{"group": "llm", "since": "2026-07-01", "force": true})
	if !r.OK || r.Output["skipped"].(map[string]string)[statsGroupLLM] != "outside_retention" {
		t.Fatalf("forced outside the window: %+v %+v", r.Output, r.Error)
	}
	off := false
	d := s.Doc()
	d.Stats.Enabled = &off
	s.setDoc(d, s.Revision())
	if r := run(nil); r.OK || r.Error.Code != errcode.FeatureDisabled {
		t.Fatalf("statistics off: %+v", r.Error)
	}
}
