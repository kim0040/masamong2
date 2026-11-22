package application

import (
	"context"
	"sync/atomic"
	"time"

	"masamong/internal/memory/engine"
)

// serviceHooks are what a test injects into one service: shorter intervals, a function run at a point
// worth watching. Production leaves every field zero. Plain fields are set through the constructor
// (openHooked) or before the work that reads them starts, and not changed while it runs; atomic fields
// may change at any time. A test that shrinks an interval in one service leaves every other service in
// the process as it is.
type serviceHooks struct {
	// pendingPollMin and pendingPollMax bound the pending-event worker's idle poll (pending_events.go);
	// zero = pendingEventPollMin and pendingEventPollMax.
	pendingPollMin, pendingPollMax time.Duration
	// pendingStep runs at the start of every pending-event worker pass.
	pendingStep func()
	// pendingCount replaces the console's count of messages waiting for a vector (pending_count.go).
	pendingCount func(ctx context.Context, eng *engine.Engine, space string) (int, error)
	// statsDay runs after each finished day of the statistics rollup (a test stops a backfill midway);
	// a test may set and clear it while the service runs.
	statsDay atomic.Pointer[func(s *Service, ctx context.Context, group, day string)]
	// threads run inside the thread bookkeeping (threads.go).
	threads threadHooks
	// zodiac stop a zodiac request where another can slip in (fortune.go).
	zodiac zodiacHooks
	// oldEditsAtOnce settles old edits at the first idle pass instead of oldEditsDelay after the start.
	oldEditsAtOnce bool
}

// durationOr is d, or def when d is zero (an interval a test did not shrink).
func durationOr(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}
