package application

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"masamong/internal/chat"
)

// The turn log's outcome for every way a turn ends; the first match wins, in this order.
func TestTurnOutcome(t *testing.T) {
	answered := chat.Result{Answer: "응"}
	for _, c := range []struct {
		err  error
		res  chat.Result
		want string
	}{
		{nil, answered, "answered"},
		{nil, chat.Result{Answer: " \n"}, "silent"},
		{ErrChatPaused, answered, "paused"},
		{ErrChatDisabled, answered, "disabled"},
		{fmt.Errorf("%w: user", ErrChatLimited), answered, "limited"},
		{ErrChatBusy, answered, "limited"},
		{DMLimited{Scope: "user"}, answered, "limited"},
		{ErrChatBudget, answered, "budget"},
		{notAddressedErr{decision: "about_bot"}, answered, "not_addressed"},
		{ErrNoVision, answered, "no_vision"},
		{ErrImageUnreadable, answered, "no_vision"},
		{ReactOnly{Emoji: "👀"}, answered, "reacted"},
		{context.Canceled, answered, "canceled"},
		{context.DeadlineExceeded, answered, "error"},
		{errors.New("provider down"), chat.Result{}, "error"},
		{errors.Join(ErrChatPaused, context.Canceled), answered, "paused"},
	} {
		if got := turnOutcome(c.res, c.err); got != c.want {
			t.Errorf("turnOutcome(%q, %v) = %q, want %q", c.res.Answer, c.err, got, c.want)
		}
	}
}
