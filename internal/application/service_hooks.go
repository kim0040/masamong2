package application

import "time"

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
}

// durationOr is d, or def when d is zero (an interval a test did not shrink).
func durationOr(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}
