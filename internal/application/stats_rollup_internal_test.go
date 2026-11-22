package application

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"slices"
	"strings"
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

// A finished day is final: after its source rows are pruned it keeps its totals, a forced recompute
// outside the sources' retention window is refused (H1), and the days a long downtime left unfinished
// are finished before the prune that would take their rows (H3).
func TestStatsFinishedDayOutlivesItsPrunedSource(t *testing.T) {
	s, clk := statsRollupService(t, "Asia/Seoul")
	ctx := context.Background()
	local := func(day, hm string) time.Time {
		tm, err := time.ParseInLocation("2006-01-02 15:04", day+" "+hm, statsKST)
		if err != nil {
			t.Fatal(err)
		}
		return tm
	}
	llm := func(day string, n int) {
		for i := 0; i < n; i++ {
			statsLLM(t, s, local(day, "12:00").Add(time.Duration(i)*time.Minute), "chat", "gpt-x", 1, 10, 1, 500, 0.001, "", "stop", "")
		}
	}
	llm("2026-10-08", 1)
	llm("2026-10-09", 2)
	llm("2026-10-10", 1)
	// the scheduled pass: from now on the service knows pruning has run
	if st := s.runScheduledRetention(ctx); !st.OK {
		t.Fatal(st.Error)
	}
	if days := statsDaysOf(t, s, statsGroupLLM); !slices.Equal(days, []string{"2026-10-08", "2026-10-09", "2026-10-10"}) {
		t.Fatalf("finished %v", days)
	}
	// written before the service went down for 120 days, never rolled up
	llm("2026-10-11", 3)
	llm("2026-10-12", 1)
	clk.t = clk.t.AddDate(0, 0, 120)
	if st := s.runScheduledRetention(ctx); !st.OK {
		t.Fatal(st.Error)
	}
	if n := statsCount(t, s, `SELECT COUNT(*) FROM llm_calls`); n != 0 {
		t.Fatalf("ledger rows older than ledger_days stay: %d", n)
	}
	for day, want := range map[string]int64{"2026-10-08": 1, "2026-10-09": 2, "2026-10-10": 1, "2026-10-11": 3, "2026-10-12": 1} {
		if got := statsN(t, s, day, "llm.calls", "role=chat|model=gpt-x|provider=none"); got != want {
			t.Errorf("%s: %d calls after the prune, want %d", day, got, want)
		}
	}
	last := statsLastFinishable(clk.t, statsKST)
	if days := statsDaysOf(t, s, statsGroupLLM); days[len(days)-1] != last || len(days) != 3+120 {
		t.Fatalf("days after the downtime: %d through %s, want through %s", len(days), days[len(days)-1], last)
	}
	// forcing a day whose source was pruned is refused; the totals stay
	out := statsRollupNow(t, s, statsRollupInput{Group: statsGroupLLM, Since: "2026-10-09", Force: true})
	if out.Skipped[statsGroupLLM] != "outside_retention" {
		t.Fatalf("forced outside the window: %+v", out)
	}
	if statsN(t, s, "2026-10-09", "llm.calls", "role=chat|model=gpt-x|provider=none") != 2 {
		t.Fatal("a refused force changed a finished day")
	}
	// without force a finished day is never rewritten, with force one inside the window is
	inside := statsDayAdd(last, -2)
	llm(inside, 2)
	if out := statsRollupNow(t, s, statsRollupInput{Group: statsGroupLLM, Since: inside}); out.Finished[statsGroupLLM] != 0 {
		t.Fatalf("finished days rewritten without force: %+v", out)
	}
	if n := statsCount(t, s, `SELECT COUNT(*) FROM stats_daily WHERE day=?`, inside); n != 0 {
		t.Fatalf("an unforced run changed a finished day: %d rows", n)
	}
	if out := statsRollupNow(t, s, statsRollupInput{Group: statsGroupLLM, Since: inside, Force: true}); out.Finished[statsGroupLLM] != 3 || out.Skipped[statsGroupLLM] != "" {
		t.Fatalf("forced inside the window: %+v", out)
	}
	if statsN(t, s, inside, "llm.calls", "role=chat|model=gpt-x|provider=none") != 2 {
		t.Fatal("a forced day was not recomputed")
	}
}

// Prune waits for the rollup (H3): a source table of a group never rolled up is not pruned at all, one
// whose group finished through some day is pruned only up to the end of that day, and the schedule state
// says why. With statistics off the gate is off.
func TestStatsPruneWaitsForUnrolledDays(t *testing.T) {
	s, clk := statsRollupService(t, "Asia/Seoul")
	ctx := context.Background()
	old := clk.t.AddDate(0, 0, -100)
	statsLLM(t, s, old, "chat", "gpt-x", 1, 10, 1, 500, 0.001, "", "stop", "")
	statsLLM(t, s, old.AddDate(0, 0, 5), "chat", "gpt-x", 1, 10, 1, 500, 0.001, "", "stop", "")
	oldDay := clk.t.AddDate(0, 0, -410).In(statsKST).Format("2006-01-02")
	statsMust(t, s, `INSERT INTO activity_counts(day, guild_id, channel_id, user_id, name, n, last_at) VALUES(?, 'g', 'c', 'u1', '', 4, ?)`, oldDay, stamp(old))
	// tools: finished through one old day; a row of that day and one of the next, both past ledger_days
	toolDay := old.In(statsKST).Format("2006-01-02")
	if err := s.statsReplaceDay(ctx, statsDayWrite{Group: statsGroupTools, Day: toolDay, Zone: "Asia/Seoul"}); err != nil {
		t.Fatal(err)
	}
	tool := func(at time.Time) {
		statsMust(t, s, `INSERT INTO tool_calls(turn_id, tool, guard, status, latency_ms, cost_usd, result_chars, class, provider, created_at)
			VALUES('', 'search.web', 'allowed', 'ok', 100, 0, 1, '', '', ?)`, stamp(at))
	}
	finishedAt := statsDayStart(toolDay, statsKST).Add(12 * time.Hour)
	tool(finishedAt)
	tool(finishedAt.AddDate(0, 0, 1))

	// a rollup that is already running (or failed) leaves the days unfinished
	statsRollups.Store(s, struct{}{})
	res, err := s.retentionPass(ctx)
	statsRollups.Delete(s)
	if err != nil {
		t.Fatal(err)
	}
	if res.statsErr == "" || res.stats != nil {
		t.Fatalf("the busy rollup is not reported: %+v", res)
	}
	if n := statsCount(t, s, `SELECT COUNT(*) FROM llm_calls`); n != 2 || res.removed["llm_calls"] != 0 || !strings.Contains(res.held["llm_calls"], "first llm statistics rollup") {
		t.Fatalf("llm rows not rolled up were pruned (or not said why): %d left, held %q", n, res.held["llm_calls"])
	}
	if statsCount(t, s, `SELECT COUNT(*) FROM activity_counts`) != 1 || res.held["activity"] == "" {
		t.Fatalf("activity of a day not rolled up was pruned: %+v", res.held)
	}
	if n := statsCount(t, s, `SELECT COUNT(*) FROM tool_calls`); n != 1 || res.removed["tool_calls"] != 1 || !strings.Contains(res.held["tool_calls"], "finished through "+toolDay) {
		t.Fatalf("tool rows: %d left (want the unfinished day's), removed %d, held %q", n, res.removed["tool_calls"], res.held["tool_calls"])
	}
	if statsCount(t, s, `SELECT COUNT(*) FROM tool_calls WHERE created_at=?`, stamp(finishedAt.AddDate(0, 0, 1))) != 1 {
		t.Fatal("the row of the unfinished day went")
	}

	// the scheduled pass rolls them up first, then prunes; its state shows the rollup
	st := s.runScheduledRetention(ctx)
	if !st.OK || st.Detail["stats"] == nil {
		t.Fatalf("scheduled pass: %+v", st)
	}
	if n := statsCount(t, s, `SELECT COUNT(*) FROM llm_calls`) + statsCount(t, s, `SELECT COUNT(*) FROM tool_calls`) + statsCount(t, s, `SELECT COUNT(*) FROM activity_counts`); n != 0 {
		t.Fatalf("%d source rows left after the rollup finished their days", n)
	}
	for _, c := range []struct {
		day, metric, dims string
		want              int64
	}{
		{old.In(statsKST).Format("2006-01-02"), "llm.calls", "role=chat|model=gpt-x|provider=none", 1},
		{old.AddDate(0, 0, 5).In(statsKST).Format("2006-01-02"), "llm.calls", "role=chat|model=gpt-x|provider=none", 1},
		{statsDayAdd(toolDay, 1), "tools.calls", "tool=search.web|status=ok|class=none|provider=none", 1},
		{oldDay, "activity.messages", "", 4},
	} {
		if got := statsN(t, s, c.day, c.metric, c.dims); got != c.want {
			t.Errorf("%s %s: %d, want %d", c.day, c.metric, got, c.want)
		}
	}
	if statsCount(t, s, `SELECT COUNT(*) FROM stats_daily WHERE day=? AND metric='tools.calls'`, toolDay) != 0 {
		t.Fatal("a finished day was rewritten")
	}
	var rec map[string]string
	if ok, _ := s.kvGet(statsPrunedKey, &rec); !ok || rec["llm_calls"] == "" || rec["activity_counts"] == "" {
		t.Fatalf("prune cuts not recorded: %v", rec)
	}

	// statistics off: the gate is off, and the cut is still recorded
	off := false
	d := s.Doc()
	d.Stats.Enabled = &off
	s.setDoc(d, s.Revision())
	statsLLM(t, s, clk.t.AddDate(0, 0, -200), "chat", "gpt-x", 1, 1, 1, 1, 0, "", "", "")
	before := rec["llm_calls"]
	clk.t = clk.t.Add(48 * time.Hour)
	res, err = s.retentionPass(ctx)
	if err != nil || res.stats != nil || len(res.held) != 0 {
		t.Fatalf("statistics off: %+v %v", res, err)
	}
	if statsCount(t, s, `SELECT COUNT(*) FROM llm_calls`) != 0 {
		t.Fatal("with statistics off old rows are pruned as before")
	}
	_, _ = s.kvGet(statsPrunedKey, &rec)
	if rec["llm_calls"] <= before {
		t.Fatalf("cut not moved: %q → %q", before, rec["llm_calls"])
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

// Close stops a backfill between days (M5): the days finished so far are whole, nothing is pruned, the
// schedule does not record a run, and the next start continues after the last finished day.
func TestStatsRollupStopsBetweenDaysOnClose(t *testing.T) {
	s := privacyService(t)
	clk := &movableClock{t: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)}
	s.Clock = clk
	statsSetZone(s, "Asia/Seoul")
	first := "2026-09-21"
	for i := 0; i < 20; i++ {
		statsTurn(t, s, statsDayStart(statsDayAdd(first, i), statsKST).Add(12*time.Hour), "answered", 0, 100, 0, 0)
	}
	reached := make(chan struct{})
	hook := func(svc *Service, ctx context.Context, group, day string) {
		if svc == s && group == statsGroupTurns && day == "2026-09-23" {
			close(reached)
			<-ctx.Done() // stays inside the rollup until Close cancels it
		}
	}
	statsDayHook.Store(&hook)
	t.Cleanup(func() { statsDayHook.Store(nil) })
	if !s.startRetentionPass(context.Background()) {
		t.Fatal("pass not started")
	}
	if s.startRetentionPass(context.Background()) {
		t.Fatal("a second pass started beside the first")
	}
	select {
	case <-reached:
	case <-time.After(20 * time.Second):
		t.Fatal("the rollup never reached the third day")
	}
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Close waited for the whole backfill")
	}

	s2, err := Open(s.DataRoot, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	s2.Clock = clk
	statsSetZone(s2, "Asia/Seoul")
	if days := statsDaysOf(t, s2, statsGroupTurns); !slices.Equal(days, []string{"2026-09-21", "2026-09-22", "2026-09-23"}) {
		t.Fatalf("finished before Close: %v", days)
	}
	if n := statsCount(t, s2, `SELECT COUNT(*) FROM stats_daily WHERE metric='turns.count'`); n != 3 {
		t.Fatalf("%d day rows for 3 finished days", n)
	}
	if n := statsCount(t, s2, `SELECT COUNT(*) FROM turn_log`); n != 20 {
		t.Fatalf("source rows pruned during a stopped pass: %d", n)
	}
	var gc scheduleState
	if ok, _ := s2.kvGet("schedule:usage_gc", &gc); ok {
		t.Fatalf("a pass stopped by shutdown was recorded as run: %+v", gc)
	}
	if _, err := s2.runRetention(context.Background()); err != nil {
		t.Fatal(err)
	}
	days := statsDaysOf(t, s2, statsGroupTurns)
	if len(days) != 20 || days[0] != first || days[19] != "2026-10-10" {
		t.Fatalf("after restart: %v", days)
	}
	for _, d := range days {
		if n := statsN(t, s2, d, "turns.count", "outcome=answered"); n != 1 {
			t.Fatalf("%s: %d turns", d, n)
		}
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
