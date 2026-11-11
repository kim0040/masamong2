package application

import "time"

// SetFetchSoftForTest shortens the soft deadline of page reading (once two pages are in); it returns
// the restore.
func SetFetchSoftForTest(d time.Duration) func() {
	old := fetchSoftAfter
	fetchSoftAfter = d
	return func() { fetchSoftAfter = old }
}

// SearchDeadlineForTest is how long search.web waits for one service of a kind at a depth.
func SearchDeadlineForTest(kind, depth string) time.Duration { return searchTimeout(kind, depth) }

// SearchTotalForTest is how long a whole search at a depth may take across the services it tries.
func SearchTotalForTest(depth string) time.Duration { return searchTotalTimeout(depth) }

// SetSearchLimitsForTest sets one service's limit and the whole search's limit at a depth, and the
// time under which the next service is not asked; it returns the restore.
func SetSearchLimitsForTest(depth string, own, total, minLeft time.Duration) func() {
	oldOwn, oldTotal, oldMin := searchDeadline[depth], searchTotal[depth], searchMinLeft
	oldKind := searchDeadlineKind
	searchDeadline[depth], searchTotal[depth], searchMinLeft = own, total, minLeft
	searchDeadlineKind = map[string]time.Duration{}
	return func() {
		searchDeadline[depth], searchTotal[depth], searchMinLeft = oldOwn, oldTotal, oldMin
		searchDeadlineKind = oldKind
	}
}

// SetFetchDeadlineForTest sets the hard limit of page reading at a depth; it returns the restore.
func SetFetchDeadlineForTest(depth string, d time.Duration) func() {
	old := fetchHardDeadline[depth]
	fetchHardDeadline[depth] = d
	return func() { fetchHardDeadline[depth] = old }
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
