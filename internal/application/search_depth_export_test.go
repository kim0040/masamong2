package application

import "time"

// SetFetchSoftForTest shortens the soft deadline of page reading (once two pages are in); it returns
// the restore.
func SetFetchSoftForTest(d time.Duration) func() {
	old := fetchSoftAfter
	fetchSoftAfter = d
	return func() { fetchSoftAfter = old }
}

// SearchDeadlineForTest is how long search.web waits for one service at a depth.
func SearchDeadlineForTest(depth string) time.Duration { return searchTimeout(depth) }

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
