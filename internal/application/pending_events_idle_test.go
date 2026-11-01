package application

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestPendingEventPollBacksOffAndResetsWhenWorkExists(t *testing.T) {
	delay := pendingEventPollMin
	for _, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 10 * time.Second, 10 * time.Second} {
		if delay != want {
			t.Fatalf("idle poll delay = %s, want %s", delay, want)
		}
		delay = nextPendingEventPoll(delay, false)
	}
	if got := nextPendingEventPoll(delay, true); got != time.Second {
		t.Fatalf("poll delay with pending work = %s, want 1s", got)
	}
}

// The real worker loop: it backs off while nothing is pending, an enqueue wakes it at once instead of
// waiting out the long interval, and shutdown ends it without waiting either. The intervals are shrunk
// and the passes are counted through the step hook.
func TestPendingEventWorkerBacksOffWakesOnEnqueueAndStopsOnShutdown(t *testing.T) {
	oldMin, oldMax, oldHook := pendingEventPollMin, pendingEventPollMax, pendingEventStepHook
	pendingEventPollMin, pendingEventPollMax = 40*time.Millisecond, 10*time.Second
	var mu sync.Mutex
	var steps []time.Time
	pendingEventStepHook = func() {
		mu.Lock()
		steps = append(steps, time.Now())
		mu.Unlock()
	}
	defer func() { pendingEventPollMin, pendingEventPollMax, pendingEventStepHook = oldMin, oldMax, oldHook }()
	s, _ := deliveryService(t) // setting the service up starts the worker, with the shrunk intervals
	count := func() int { mu.Lock(); defer mu.Unlock(); return len(steps) }
	waitFor := func(what string, limit time.Duration, ok func() bool) {
		t.Helper()
		deadline := time.Now().Add(limit)
		for !ok() {
			if time.Now().After(deadline) {
				t.Fatalf("timed out waiting for %s (%d passes)", what, count())
			}
			time.Sleep(5 * time.Millisecond)
		}
	}

	// idle: waits of 40, 80, 160, 320, 640 ms ...; a fixed 40 ms poll would make ~25 passes in the first second
	waitFor("the idle passes", 10*time.Second, func() bool { return count() >= 7 })
	mu.Lock()
	gaps := make([]time.Duration, 0, len(steps)-1)
	for i := 1; i < len(steps); i++ {
		gaps = append(gaps, steps[i].Sub(steps[i-1]))
	}
	mu.Unlock()
	if gaps[len(gaps)-1] < 3*gaps[0] {
		t.Fatalf("idle polling did not back off: gaps %v", gaps)
	}

	// the next wait is now over a second; an enqueue must not sit through it
	n := count()
	time.Sleep(60 * time.Millisecond)
	if count() != n {
		t.Fatalf("a pass ran inside the backed-off wait")
	}
	started := time.Now()
	if _, err := s.Store.EnqueueEvent("source-change", "not a replayable job"); err != nil {
		t.Fatal(err)
	}
	waitFor("the pass the enqueue wakes", 3*time.Second, func() bool { return count() > n })
	if woke := time.Since(started); woke > 800*time.Millisecond {
		t.Fatalf("the enqueue woke the worker after %v, it waited out the backoff", woke)
	}
	// the event cannot be replayed: it stays pending, so the worker polls at the short interval
	n = count()
	waitFor("short polls while an event is pending", 5*time.Second, func() bool { return count() >= n+3 })
	dir := filepath.Join(filepath.Dir(s.Store.Path()), "pending-events", "source-change")
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		_ = os.Remove(filepath.Join(dir, e.Name()))
	}

	// shutdown while it waits: it ends promptly
	waitFor("the backoff to grow again", 10*time.Second, func() bool {
		mu.Lock()
		defer mu.Unlock()
		k := len(steps)
		return k >= 3 && steps[k-1].Sub(steps[k-2]) >= 160*time.Millisecond
	})
	started = time.Now()
	closed := make(chan error, 1)
	go func() { closed <- s.Close() }()
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("shutdown took %v with the worker mid-wait", took)
	}
	if s.eventWorker.Load() {
		t.Fatal("the event worker is still running after Close")
	}
}
