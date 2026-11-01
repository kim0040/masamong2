package application

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/clock"
)

func TestSchedulerHeartbeatIsThrottledWithinTenMinutes(t *testing.T) {
	start := time.Date(2026, 10, 12, 1, 0, 0, 0, time.UTC)
	svc, err := Open(t.TempDir(), clock.Fixed{T: start}, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	setup := svc.Execute(context.Background(), action.Request{Action: "setup.resume", Input: json.RawMessage(`{"instance":"gate"}`), Locale: "en"})
	if !setup.OK {
		t.Fatalf("setup: %+v", setup.Error)
	}
	doc := svc.Doc()
	doc.Backup.Schedule = "off"
	svc.SetDocForTest(doc)
	svc.noteUptime("start", "")

	read := func() string {
		t.Helper()
		var value string
		if err := svc.Store.DB.QueryRow(`SELECT v FROM admin_state WHERE k='uptime:heartbeat'`).Scan(&value); err != nil {
			t.Fatal(err)
		}
		return value
	}
	svc.schedulerTick()
	first := read()
	for _, minute := range []int{1, 5, 7} {
		svc.Clock = clock.Fixed{T: start.Add(time.Duration(minute) * time.Minute)}
		svc.schedulerTick()
		if got := read(); got != first {
			t.Fatalf("heartbeat changed after %d minutes: %s", minute, got)
		}
	}
	// a crash a few minutes in is dated by the saved heartbeat (the run's first minute)
	svc.noteUptime("start", "")
	var events []uptimeEvent
	if _, err := svc.kvGet("uptime:events", &events); err != nil {
		t.Fatal(err)
	}
	if len(events) < 2 || events[1].Kind != "crash" || events[1].At != start.Format(time.RFC3339) {
		t.Fatalf("unclean-shutdown time did not use the latest saved heartbeat: %#v", events)
	}

	// Over two hours of minute ticks (with jitter) the stored time is never more than ten minutes behind
	// the tick that follows, and it is written about every nine to ten minutes, not every minute.
	writes := 0
	last := first
	for sec := 8 * 60; sec <= 120*60; sec += 60 + (sec/60)%7 {
		now := start.Add(time.Duration(sec) * time.Second)
		svc.Clock = clock.Fixed{T: now}
		svc.schedulerTick()
		if got := read(); got != last {
			writes++
			last = got
		}
		stored, err := time.Parse(`"2006-01-02T15:04:05Z"`, last)
		if err != nil {
			t.Fatal(err)
		}
		if age := now.Sub(stored); age > uptimeHeartbeatEvery {
			t.Fatalf("at %s the stored heartbeat is %v old", now.Format(time.RFC3339), age)
		}
	}
	if writes < 10 || writes > 14 {
		t.Fatalf("%d heartbeat writes in two hours, want about one per nine to ten minutes", writes)
	}

	// a clock that went back must not leave a heartbeat in the future
	back := start.Add(30 * time.Minute)
	svc.Clock = clock.Fixed{T: back}
	svc.schedulerTick()
	if got := read(); got != `"`+back.Format(time.RFC3339)+`"` {
		t.Fatalf("heartbeat after the clock went backwards = %s", got)
	}

	// a clean stop records its own event and needs no heartbeat: the next start sees no crash
	svc.NoteStop(false)
	svc.noteUptime("start", "")
	events = nil
	if _, err := svc.kvGet("uptime:events", &events); err != nil {
		t.Fatal(err)
	}
	crashes := 0
	for _, e := range events {
		if e.Kind == "crash" {
			crashes++
		}
	}
	if crashes != 1 {
		t.Fatalf("a clean stop was read as a crash: %#v", events)
	}
}
