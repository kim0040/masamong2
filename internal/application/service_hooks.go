package application

import (
	"context"
	"time"

	"masamong/internal/memory/engine"
)

// serviceHooks are what a test injects into one service: shorter intervals, a function run at a point
// worth watching. Production leaves every field zero. They are given to the constructor (openHooked) or
// set before the work that reads them starts, and never changed while it runs, so a test that shrinks
// an interval in one service leaves every other service in the process as it is.
type serviceHooks struct {
	// pendingPollMin and pendingPollMax bound the pending-event worker's idle poll (pending_events.go);
	// zero = pendingEventPollMin and pendingEventPollMax.
	pendingPollMin, pendingPollMax time.Duration
	// pendingStep runs at the start of every pending-event worker pass.
	pendingStep func()
	// pendingCount replaces the console's count of messages waiting for a vector (pending_count.go).
	pendingCount func(ctx context.Context, eng *engine.Engine, space string) (int, error)
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
