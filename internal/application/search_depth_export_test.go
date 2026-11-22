package application

import "time"

// SetFetchSoftForTest shortens s's soft deadline of page reading (once two pages are in); it returns
// the restore.
func SetFetchSoftForTest(s *Service, d time.Duration) func() {
	return s.changeSearchLimits(func(l *searchLimits) { l.fetchSoft = d })
}

// SearchDeadlineForTest is how long search.web waits for one service of a kind at a depth.
func SearchDeadlineForTest(kind, depth string) time.Duration {
	return defaultSearchLimits.timeout(kind, depth)
}

// SearchTotalForTest is how long a whole search at a depth may take across the services it tries.
func SearchTotalForTest(depth string) time.Duration { return defaultSearchLimits.totalTimeout(depth) }

// SetSearchLimitsForTest sets one service's limit and the whole search's limit at a depth in s, and the
// time under which the next service is not asked; it returns the restore.
func SetSearchLimitsForTest(s *Service, depth string, own, total, minLeft time.Duration) func() {
	return s.changeSearchLimits(func(l *searchLimits) {
		l.deadline[depth], l.total[depth], l.minLeft = own, total, minLeft
		l.deadlineKind = map[string]time.Duration{}
	})
}

// SetFetchDeadlineForTest sets s's hard limit of page reading at a depth; it returns the restore.
func SetFetchDeadlineForTest(s *Service, depth string, d time.Duration) func() {
	return s.changeSearchLimits(func(l *searchLimits) { l.fetchHard[depth] = d })
}

// SetFetchBaseForTest points TinyFish Fetch at a test server; it returns the restore.
func SetFetchBaseForTest(url string) func() {
	old := tinyfishFetchBase
	tinyfishFetchBase = url
	return func() { tinyfishFetchBase = old }
}

// SetProbeTimeoutForTest shortens the limit of a connection check; it returns the restore.
func SetProbeTimeoutForTest(d time.Duration) func() {
	old := probeTimeout
	probeTimeout = d
	return func() { probeTimeout = old }
}
