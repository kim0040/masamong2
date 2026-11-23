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
	// imageConnectWait and imageConnectPoll shorten a picture job's wait for the gateway (imagequeue.go).
	imageConnectWait, imageConnectPoll time.Duration
	// chatDeadlines replace chatDeadline's limits for the roles they name (chat_input.go); a test may
	// change them while the service runs.
	chatDeadlines atomic.Pointer[map[string]time.Duration]
	// search replaces defaultSearchLimits (features.go); a test may change it while the service runs.
	search atomic.Pointer[searchLimits]
	// fetchBase points TinyFish Fetch at a test server (registry.go); read when the fetcher is made.
	fetchBase atomic.Pointer[string]
	// probeTimeout shortens a connection check (Registry.Probe), in nanoseconds; 0 = probeTimeout.
	probeTimeout atomic.Int64
	// news replaces the release notes' pace and sends per tick (updatenews_announce.go).
	news atomic.Pointer[newsLimits]
	// updateAPI points the release check at a test server (updates.go).
	updateAPI atomic.Pointer[string]
	// downloads points release downloads at a test server (selfupdate.go).
	downloads atomic.Pointer[string]
	// redirectHost widens where a release download may be redirected (selfupdate.go).
	redirectHost atomic.Pointer[func(host string) bool]
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
