package application

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/hostprofile"
	"masamong/internal/memory/engine"
)

// countHook replaces the count with one that waits for release and returns its value (or panics on a
// negative one), and counts the calls.
func countHook(t *testing.T) (release chan int, calls *atomic.Int32) {
	t.Helper()
	release, calls = make(chan int), new(atomic.Int32)
	old := pendingCountHook
	pendingCountHook = func(ctx context.Context, _ *engine.Engine, _ string) (int, error) {
		calls.Add(1)
		select {
		case n := <-release:
			if n < 0 {
				panic("count failed hard")
			}
			return n, nil
		case <-ctx.Done():
			return 0, ctx.Err()
		}
	}
	t.Cleanup(func() { pendingCountHook = old })
	return release, calls
}

// A zero gap field follows the host profile: 30s is past a large host's 20s gap and inside
// medium's one minute, which is what c.gap == 0 has always meant.
func TestPendingRecountGapUsesTheHostProfile(t *testing.T) {
	s, _ := deliveryService(t)
	prev := hostprofile.Current()
	t.Cleanup(func() { hostprofile.Apply(prev) })
	c := &s.pendingMsgs
	arm := func(ended time.Time) (started bool) {
		t.Helper()
		c.mu.Lock()
		c.known, c.ident, c.key = true, "A", "A|old"
		c.counted = time.Now().Add(-time.Hour)
		c.ended = ended
		c.run = nil
		if c.waiting != nil {
			c.waiting.Stop()
			c.waiting = nil
		}
		c.mu.Unlock()
		_, started = s.admit("A", "A|new", false)
		return started
	}
	large := prev
	large.PendingRecountGap = 20 * time.Second
	hostprofile.Apply(large)
	if !arm(time.Now().Add(-30 * time.Second)) {
		t.Fatal("30s after a count, a 20s gap did not recount")
	}
	medium := prev
	medium.PendingRecountGap = time.Minute
	hostprofile.Apply(medium)
	if arm(time.Now()) {
		t.Fatal("inside the one-minute gap a count started")
	}
	c.mu.Lock()
	if c.waiting != nil {
		c.waiting.Stop()
		c.waiting = nil
	}
	c.run = nil
	c.mu.Unlock()
}

// The console and the doctor share one admission and one background count: requests wait with their own
// context, the gap follows every count (also a failed or panicked one, also for the doctor), a changed
// space is counted at once, and a waiter gets the result of the count it waited for.
func TestPendingCountAdmission(t *testing.T) {
	s, _ := deliveryService(t)
	c := &s.pendingMsgs
	c.gap = time.Hour // the reminder never fires during the test
	release, calls := countHook(t)
	bg := context.Background()

	run, started := s.admit("A", "k", false)
	if run == nil || !started {
		t.Fatal("the first request did not start a count")
	}
	s.startPendingCount(nil, "A", "k", run)
	for _, doctor := range []bool{false, true} {
		if r, st := s.admit("A", "k", doctor); r != run || st {
			t.Fatalf("while the first count runs (doctor=%v): a second count, or not waiting for the first", doctor)
		}
	}
	// a request that gives up waiting does not cancel the count
	gone, cancel := context.WithCancel(bg)
	cancel()
	if _, err := s.pendingFor(gone, "A", run); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled request kept waiting: %v", err)
	}
	release <- 5
	if n, err := s.pendingFor(bg, "A", run); n != 5 || err != nil {
		t.Fatalf("waiter got %d, %v", n, err)
	}

	// within the gap a change keeps the last count for both, and arms one reminder
	for _, doctor := range []bool{false, true} {
		if r, st := s.admit("A", "k2", doctor); r != nil || st {
			t.Fatalf("stale within the gap (doctor=%v): counted", doctor)
		}
	}
	if c.waiting == nil {
		t.Fatal("no reminder armed for the stale count")
	}
	if n, err := s.pendingFor(bg, "A", nil); n != 5 || err != nil {
		t.Fatalf("stored count %d, %v", n, err)
	}

	// another space (a config change) is counted without waiting for the gap; the old space's console
	// keeps its number while it runs, the new one waits
	runB, started := s.admit("B", "kb", true)
	if runB == nil || !started {
		t.Fatal("a changed space waited for the gap")
	}
	s.startPendingCount(nil, "B", "kb", runB)
	if r, _ := s.admit("A", "k2", false); r != nil {
		t.Fatal("the old space's console waited for the new space's count")
	}
	if r, _ := s.admit("B", "kb", false); r != runB {
		t.Fatal("the new space's console did not wait for its first count")
	}
	release <- 7
	if n, err := s.pendingFor(bg, "B", runB); n != 7 || err != nil {
		t.Fatalf("new space: %d, %v", n, err)
	}
	if _, err := s.pendingFor(bg, "A", runB); !errors.Is(err, errPendingUncounted) {
		t.Fatalf("a request for the old space took the new space's number: %v", err)
	}

	// a panicking count ends its run (waiters get an error, the slot is free) and starts the gap
	c.mu.Lock()
	c.ended, c.counted = time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour)
	c.mu.Unlock()
	runP, started := s.admit("B", "kb", true)
	if !started {
		t.Fatal("a stale count after the gap did not start")
	}
	s.startPendingCount(nil, "B", "kb", runP)
	release <- -1
	if _, err := s.pendingFor(bg, "B", runP); err == nil {
		t.Fatal("a panicked count reported success")
	}
	if r, st := s.admit("B", "kb2", true); r != nil || st {
		t.Fatal("no gap after a panicked count, or its slot is still taken")
	}
	if n, err := s.pendingFor(bg, "B", nil); n != 7 || err != nil {
		t.Fatalf("after a failed recount the last good count is gone: %d, %v", n, err)
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("%d counts, want 3", got)
	}

	// after Close nothing is admitted and the reminder is stopped
	c.close()
	if c.waiting != nil {
		t.Fatal("the reminder outlived close")
	}
	c.mu.Lock()
	c.ended = time.Now().Add(-2 * time.Hour)
	c.mu.Unlock()
	if r, st := s.admit("B", "kb3", true); r != nil || st {
		t.Fatal("a count was admitted after close")
	}
}
