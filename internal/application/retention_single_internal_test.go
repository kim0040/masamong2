package application

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"masamong/internal/action"
)

// The console's "run now" for usage_gc and the scheduler's daily pass share one mark: while either
// runs, the other does not start beside it (both prune the same tables and record the same schedule).
func TestUsageGCRunsOneAtATime(t *testing.T) {
	t.Parallel()
	s := privacyService(t)
	clk := &movableClock{t: time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC)}
	s.Clock = clk
	statsSetZone(s, "Asia/Seoul")
	for i := 0; i < 3; i++ {
		statsTurn(t, s, statsDayStart(statsDayAdd("2026-10-05", i), statsKST).Add(12*time.Hour), "answered", 0, 100, 0, 0)
	}
	reached, release := make(chan struct{}), make(chan struct{})
	hook := func(svc *Service, ctx context.Context, group, day string) {
		if svc == s && group == statsGroupTurns && day == "2026-10-05" {
			close(reached)
			select { // the scheduled pass stays inside its rollup until the test lets it go
			case <-release:
			case <-ctx.Done():
			}
		}
	}
	s.hooks.statsDay.Store(&hook)
	runNow := func() action.Result {
		raw, _ := json.Marshal(map[string]any{"id": "usage_gc"})
		return s.Execute(context.Background(), action.Request{Action: "schedules.run", Input: raw, Locale: "en"})
	}

	if !s.startRetentionPass(context.Background()) {
		t.Fatal("scheduled pass not started")
	}
	select {
	case <-reached:
	case <-time.After(20 * time.Second):
		t.Fatal("the scheduled pass never reached its rollup")
	}
	if r := runNow(); r.OK {
		t.Fatalf("the console ran usage_gc beside the scheduled pass: %+v", r.Output)
	}
	close(release)
	deadline := time.Now().Add(20 * time.Second)
	for {
		if !s.runs.retention.Load() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the scheduled pass did not finish")
		}
		time.Sleep(10 * time.Millisecond)
	}
	s.hooks.statsDay.Store(nil)
	if r := runNow(); !r.OK {
		t.Fatalf("run now after the scheduled pass: %+v", r.Error)
	}
	if s.runs.retention.Load() {
		t.Fatal("the console run left its mark behind")
	}

	// and the other way: while the console's run holds the mark, the scheduler does not start one
	s.runs.retention.Store(true)
	if s.startRetentionPass(context.Background()) {
		t.Fatal("the scheduler started a pass beside a running one")
	}
	s.runs.retention.Store(false)
}
