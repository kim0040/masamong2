package application

import "sync"

// logOnce remembers the log lines a service has written once (Service.loggedOnce): a bad setting read
// on every call is logged at its first read, not on each one, and the practice instance logs its own.
type logOnce struct{ seen sync.Map }

// first reports whether key is new, and remembers it.
func (l *logOnce) first(key string) bool {
	_, seen := l.seen.LoadOrStore(key, true)
	return !seen
}
