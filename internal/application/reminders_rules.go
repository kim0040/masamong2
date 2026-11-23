package application

import (
	"errors"
	"time"

	"masamong/internal/features/reminders"
)

// The reminder rules that need no I/O: which candidate times are still ahead, whether a reminder can
// still be cancelled, and how a due reminder is served and how its send attempt ends.

func futureOnly(ts []time.Time, now time.Time) []time.Time {
	var out []time.Time
	for _, t := range ts {
		if t.After(now) {
			out = append(out, t)
		}
	}
	return out
}

// cancellable is the Go form of the reminderLive SQL condition: a reminder that can still be
// cancelled is live — before it is sent, or a series while one occurrence is being sent.
func cancellable(r reminder) bool {
	switch r.Status {
	case reminderProposed, reminderScheduled, reminderPaused:
		return true
	case reminderSending:
		return r.Repeat != nil
	}
	return false
}

// seriesDue is the occurrence of a series due at at to send now, or skip when none should be sent. A
// series that was not served (the daemon was off, or grace is longer than its interval) sends the newest
// occurrence still inside grace and skips the older ones, never sending them in a burst; with none inside
// grace and at itself past grace, the occurrence is skipped.
func seriesDue(rule reminders.Rule, at, now time.Time, grace time.Duration, loc *time.Location) (due time.Time, skip bool) {
	if next, ok := rule.Next(at, loc); now.Sub(at) > grace || (ok && !next.After(now)) {
		if occ, ok := rule.Latest(now, grace, loc); ok {
			return occ, false
		} else if now.Sub(at) > grace {
			return at, true
		}
	}
	return at, false
}

// reminderOutcome is how a one-off reminder's send attempt ends.
type reminderOutcome int

const (
	reminderOutcomeSent     reminderOutcome = iota
	reminderOutcomeDMClosed                 // the owner's DMs are closed for good: failed
	reminderOutcomeRetry                    // Discord refused it, nothing was posted: try again later
	reminderOutcomeFailed                   // refused reminderMaxAttempts times
	reminderOutcomeUnknown                  // the connection broke: it may have been posted, so it is not sent again
)

// oneOffOutcome classifies a one-off reminder's send error after its attempts'th attempt.
func oneOffOutcome(err error, attempts int) reminderOutcome {
	switch {
	case err == nil:
		return reminderOutcomeSent
	case errors.Is(err, ErrDMClosed):
		return reminderOutcomeDMClosed
	case errors.Is(err, ErrNotDelivered) && attempts < reminderMaxAttempts:
		return reminderOutcomeRetry
	case errors.Is(err, ErrNotDelivered):
		return reminderOutcomeFailed
	}
	return reminderOutcomeUnknown
}

// reminderRetryDelay is the wait before the next try after attempts refused sends.
func reminderRetryDelay(attempts int) time.Duration {
	return time.Duration(attempts*attempts) * time.Minute
}
