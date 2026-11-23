package application

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"masamong/internal/features/reminders"
)

// A daily series due at 09:00 is sent on time, catches up with the newest occurrence still inside grace
// (never the older ones), and skips one that is past grace with nothing newer inside it.
func TestSeriesDue(t *testing.T) {
	t.Parallel()
	daily := reminders.Rule{Kind: reminders.RepeatDaily, Time: "09:00"}
	loc := time.UTC
	at := time.Date(2026, 11, 1, 9, 0, 0, 0, loc)
	grace := 30 * time.Minute
	for _, tc := range []struct {
		name string
		now  time.Time
		due  time.Time
		skip bool
	}{
		{"on time", at.Add(time.Minute), at, false},
		{"days later, inside today's grace", at.Add(48*time.Hour + 10*time.Minute), at.Add(48 * time.Hour), false},
		{"days later, past grace", at.Add(48*time.Hour + time.Hour), at, true},
		{"past grace the same day", at.Add(time.Hour), at, true},
	} {
		due, skip := seriesDue(daily, at, tc.now, grace, loc)
		if !due.Equal(tc.due) || skip != tc.skip {
			t.Errorf("%s: %v %v, want %v %v", tc.name, due, skip, tc.due, tc.skip)
		}
	}
}

func TestOneOffReminderOutcome(t *testing.T) {
	t.Parallel()
	refused := fmt.Errorf("x: %w", ErrNotDelivered)
	for _, tc := range []struct {
		err      error
		attempts int
		want     reminderOutcome
	}{
		{nil, 1, reminderOutcomeSent},
		{fmt.Errorf("x: %w", ErrDMClosed), 1, reminderOutcomeDMClosed},
		{refused, reminderMaxAttempts - 1, reminderOutcomeRetry},
		{refused, reminderMaxAttempts, reminderOutcomeFailed},
		{errors.New("connection reset"), 1, reminderOutcomeUnknown},
	} {
		if got := oneOffOutcome(tc.err, tc.attempts); got != tc.want {
			t.Errorf("%v after %d: %v, want %v", tc.err, tc.attempts, got, tc.want)
		}
	}
	if reminderRetryDelay(2) != 4*time.Minute {
		t.Fatal("retry delay")
	}
}
