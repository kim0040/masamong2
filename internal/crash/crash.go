// Package crash keeps one failing message, request or background pass from taking the whole daemon
// down. Go does not recover a panic in one goroutine from another, so every goroutine the daemon starts
// for outside input (Discord events, jobs, background passes) defers Recover.
package crash

import (
	"fmt"
	"log/slog"
	"runtime/debug"
)

// Recover logs a panic with its stack instead of letting it crash the process. It must be deferred
// directly: defer crash.Recover(log, "discord message").
func Recover(log *slog.Logger, where string) {
	if r := recover(); r != nil {
		report(log, where, r)
	}
}

// Go runs fn in a new goroutine that recovers.
func Go(log *slog.Logger, where string, fn func()) {
	go func() {
		defer Recover(log, where)
		fn()
	}()
}

// Run calls fn and returns a panic as an error, for callers that record an outcome.
func Run(log *slog.Logger, where string, fn func()) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = Error(log, where, r)
		}
	}()
	fn()
	return nil
}

// Error logs a value recovered by the caller's own deferred recover() and returns it as an error.
// Called inside that deferred function, the logged stack still shows where the panic happened.
func Error(log *slog.Logger, where string, r any) error {
	report(log, where, r)
	return fmt.Errorf("internal error in %s: %v", where, r)
}

func report(log *slog.Logger, where string, r any) {
	if log == nil {
		log = slog.Default()
	}
	log.Error("panic recovered", "where", where, "panic", fmt.Sprint(r), "stack", string(debug.Stack()))
}
