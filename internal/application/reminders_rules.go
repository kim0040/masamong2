package application

import "time"

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
