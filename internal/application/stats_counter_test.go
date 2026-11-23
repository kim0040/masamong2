package application

import (
	"context"
	"maps"
	"sync"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/tools"
)

func statsDayOf(s *Service) string {
	return s.Clock.Now().In(s.instanceZone()).Format("2006-01-02")
}

func statsCountN(t *testing.T, s *Service, day, dims string) int64 {
	t.Helper()
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	var n int64
	if err := db.DB.QueryRow(`SELECT COALESCE(SUM(n),0) FROM stats_daily WHERE day=? AND metric='actions.calls' AND dims=?`, day, dims).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// TestStatsActionCounterCountsDims: every Execute lands in the counter under (action, via, code) —
// catalog id, folded action id, the shared via derivation and the result code — and a flush writes
// them to stats_daily.
func TestStatsActionCounterCountsDims(t *testing.T) {
	t.Parallel()
	s, _ := deliveryService(t)
	s.statsFlush(context.Background()) // clear the setup calls
	day := statsDayOf(s)
	exec := func(act, rid string) action.Result {
		return s.Execute(context.Background(), action.Request{Action: act, RequestID: rid})
	}
	exec("help.version", "cli")
	exec("help.version", "cli")
	exec("no.such.action", "web")      // an id nothing declares
	exec("runtime.status", "gui_poll") // a polled console load
	exec("help.version", "dc:i1")      // the Discord admin path
	exec("runtime.doctor", "tui")

	// an operator's HTTP tool folds to custom_http, an unknown id to unknown (review M7/H4)
	d := s.Doc()
	d.Tools = maps.Clone(d.Tools)
	d.Tools["acme_lookup"] = tools.Settings{HTTP: &tools.HTTP{URL: "https://example.com/lookup"}}
	s.setDoc(d, s.Revision())
	s.statsCount(action.Request{Action: "acme_lookup", RequestID: "web"}, action.Result{OK: true})
	// a model tool call counts under via=chat (never exported; that is Part C's job)
	s.statsCount(action.Request{Action: "help.version", RequestID: "chat",
		Invoker: &action.Invoker{Via: viaChat}}, action.Result{OK: true})
	s.statsCount(action.Request{Action: "help.version", RequestID: "chat",
		Invoker: &action.Invoker{Via: viaTask}}, action.Result{OK: true})

	if got := len(s.actionStats.counts); got != 7 {
		t.Fatalf("unflushed keys = %d, want 7", got)
	}
	s.statsFlush(context.Background())
	if got := len(s.actionStats.counts); got != 0 {
		t.Fatalf("flush left %d keys", got)
	}
	for dims, want := range map[string]int64{
		"action=help.version|via=cli|code=ok":                  2,
		"action=unknown|via=gui|code=" + errcode.ActionUnknown: 1,
		"action=runtime.status|via=gui_poll|code=ok":           1,
		"action=help.version|via=discord|code=ok":              1,
		"action=runtime.doctor|via=tui|code=ok":                1,
		"action=custom_http|via=gui|code=ok":                   1,
		"action=help.version|via=chat|code=ok":                 2,
	} {
		if n := statsCountN(t, s, day, dims); n != want {
			t.Errorf("%s: n = %d, want %d", dims, n, want)
		}
	}
}

// TestStatsActionVia: the shared derivation covers every source the counter may see.
func TestStatsActionVia(t *testing.T) {
	t.Parallel()
	for name, tc := range map[string]struct {
		req  action.Request
		want string
	}{
		"gui":        {action.Request{RequestID: "web"}, "gui"},
		"gui prefix": {action.Request{RequestID: "web:tab"}, "gui"},
		"poll":       {action.Request{RequestID: "gui_poll"}, "gui_poll"},
		"cli":        {action.Request{RequestID: "cli"}, "cli"},
		"tray":       {action.Request{RequestID: "tray"}, "tray"},
		"tui":        {action.Request{RequestID: "tui"}, "tui"},
		"sim":        {action.Request{RequestID: "sim"}, "sim"},
		"practice":   {action.Request{RequestID: "practice"}, "practice"},
		"system":     {action.Request{RequestID: "system:seed"}, "system"},
		"discord":    {action.Request{RequestID: "dc:123"}, "discord"},
		"api":        {action.Request{RequestID: "something-else"}, "api"},
		"empty":      {action.Request{}, "api"},
		"command":    {action.Request{Invoker: &action.Invoker{Via: "command"}}, "discord"},
		"reply":      {action.Request{Invoker: &action.Invoker{Via: "reply"}}, "discord"},
		"chat":       {action.Request{RequestID: "chat", Invoker: &action.Invoker{Via: "chat"}}, "chat"},
		"task":       {action.Request{RequestID: "chat", Invoker: &action.Invoker{Via: "task"}}, "chat"},
	} {
		if got := actionVia(tc.req); got != tc.want {
			t.Errorf("%s: via = %q, want %q", name, got, tc.want)
		}
	}
}

// TestStatsActionFlushMergesBack: a failed write keeps the deltas for the next flush.
func TestStatsActionFlushMergesBack(t *testing.T) {
	t.Parallel()
	s, _ := deliveryService(t)
	s.statsFlush(context.Background())
	day := statsDayOf(s)
	s.statsCount(action.Request{Action: "help.version", RequestID: "cli"}, action.Result{OK: true})
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`DROP TABLE stats_daily`); err != nil {
		t.Fatal(err)
	}
	s.statsFlush(context.Background())
	if got := len(s.actionStats.counts); got != 1 {
		t.Fatalf("failed flush lost the counts: %d keys", got)
	}
	if _, err := db.DB.Exec(`CREATE TABLE IF NOT EXISTS stats_daily (
		day TEXT NOT NULL, metric TEXT NOT NULL, dims TEXT NOT NULL DEFAULT '',
		n INTEGER NOT NULL DEFAULT 0, vals TEXT NOT NULL DEFAULT '{}',
		PRIMARY KEY(day, metric, dims)) WITHOUT ROWID`); err != nil {
		t.Fatal(err)
	}
	s.statsFlush(context.Background())
	if n := statsCountN(t, s, day, "action=help.version|via=cli|code=ok"); n != 1 {
		t.Fatalf("merged-back counts were not written: %d", n)
	}
}

// TestStatsActionOffDrops: errStatsOff drops the deltas instead of merging them back.
func TestStatsActionOffDrops(t *testing.T) {
	t.Parallel()
	s, _ := deliveryService(t)
	s.statsFlush(context.Background())
	s.statsCount(action.Request{Action: "help.version", RequestID: "cli"}, action.Result{OK: true})
	off := false
	d := s.Doc()
	d.Stats.Enabled = &off
	s.setDoc(d, s.Revision())
	s.statsFlush(context.Background())
	if got := len(s.actionStats.counts); got != 0 {
		t.Fatalf("stats off kept %d keys", got)
	}
	// and nothing new is counted while collection is off
	s.statsCount(action.Request{Action: "help.version", RequestID: "cli"}, action.Result{OK: true})
	if got := len(s.actionStats.counts); got != 0 {
		t.Fatalf("counted while off: %d", got)
	}
	if n := statsCountN(t, s, statsDayOf(s), "action=help.version|via=cli|code=ok"); n != 0 {
		t.Fatalf("dropped counts were written: %d", n)
	}
}

// TestStatsActionCloseFlushes: Close writes what is still counted before the store closes.
func TestStatsActionCloseFlushes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	s, err := Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	if r := s.Execute(context.Background(), action.Request{Action: "setup.resume", Input: []byte(`{"instance":"stats-counter"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	if r := s.Execute(context.Background(), action.Request{Action: "help.version", RequestID: "web"}); !r.OK {
		t.Fatal(r.Error)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s2.Close() })
	if n := statsCountN(t, s2, statsDayOf(s), "action=help.version|via=gui|code=ok"); n != 1 {
		t.Fatalf("Close did not flush the counter: %d", n)
	}
}

// TestStatsActionFlushTickThrottles: the scheduler flushes at most every statsFlushEvery and only
// when the counter holds something; a failed flush is throttled too.
func TestStatsActionFlushTickThrottles(t *testing.T) {
	t.Parallel()
	s, clk := deliveryService(t)
	ctx := context.Background()
	s.statsFlush(ctx)     // clear the setup calls; flushed is still zero
	s.statsFlushTick(ctx) // nothing to write: no flush, flushed stays zero
	s.statsCount(action.Request{Action: "help.version", RequestID: "cli"}, action.Result{OK: true})
	s.statsFlushTick(ctx) // first dirty tick flushes (flushed is still zero)
	if got := len(s.actionStats.counts); got != 0 {
		t.Fatalf("first dirty tick did not flush: %d", got)
	}
	s.statsCount(action.Request{Action: "help.version", RequestID: "cli"}, action.Result{OK: true})
	s.statsFlushTick(ctx) // inside the interval: kept
	if got := len(s.actionStats.counts); got != 1 {
		t.Fatalf("unthrottled tick: %d", got)
	}
	clk.t = clk.t.Add(statsFlushEvery + time.Minute)
	s.statsFlushTick(ctx)
	if got := len(s.actionStats.counts); got != 0 {
		t.Fatalf("due tick did not flush: %d", got)
	}
	// a failed flush merges the deltas back and the retry waits for the next interval
	s.statsCount(action.Request{Action: "help.version", RequestID: "cli"}, action.Result{OK: true})
	db, _ := s.adminDB()
	if _, err := db.DB.Exec(`DROP TABLE stats_daily`); err != nil {
		t.Fatal(err)
	}
	clk.t = clk.t.Add(statsFlushEvery + time.Minute)
	s.statsFlushTick(ctx)
	if got := len(s.actionStats.counts); got != 1 {
		t.Fatalf("failed tick lost counts: %d", got)
	}
	s.statsFlushTick(ctx) // same minute: no new attempt
	if got := len(s.actionStats.counts); got != 1 {
		t.Fatalf("unthrottled retry: %d", got)
	}
}

// TestStatsActionCounterRace: increments while a flush swaps and writes must neither race nor lose
// counts (the store write happens outside the counter lock). Run under -race.
func TestStatsActionCounterRace(t *testing.T) {
	t.Parallel()
	s, _ := deliveryService(t)
	ctx := context.Background()
	const goroutines, each = 8, 200
	var wg sync.WaitGroup
	stop := make(chan struct{})
	go func() {
		for {
			select {
			case <-stop:
				return
			default:
				s.statsFlush(ctx)
			}
		}
	}()
	for g := 0; g < goroutines; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < each; i++ {
				s.statsCount(action.Request{Action: "help.version", RequestID: "cli"}, action.Result{OK: true})
			}
		}()
	}
	wg.Wait()
	close(stop)
	s.statsFlush(ctx)
	if n := statsCountN(t, s, statsDayOf(s), "action=help.version|via=cli|code=ok"); n != goroutines*each {
		t.Fatalf("counted %d, want %d", n, goroutines*each)
	}
}

// TestStatsActionCounterSkips: no store and the practice instance are not counted.
func TestStatsActionCounterSkips(t *testing.T) {
	t.Parallel()
	s, err := Open(t.TempDir(), nil, false) // no database yet
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if s.Store != nil {
		t.Fatal("expected no store")
	}
	s.statsCount(action.Request{Action: "help.version", RequestID: "cli"}, action.Result{OK: true})
	if got := len(s.actionStats.counts); got != 0 {
		t.Fatalf("counted without a store: %d", got)
	}
	p, err := OpenPractice(context.Background(), s.DataRoot, "en", s.Log)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	p.statsCount(action.Request{Action: "help.version", RequestID: "cli"}, action.Result{OK: true})
	if got := len(p.actionStats.counts); got != 0 {
		t.Fatalf("practice counted: %d", got)
	}
}
