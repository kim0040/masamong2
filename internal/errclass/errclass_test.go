package errclass

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

type classed struct{ c Class }

func (e classed) Error() string   { return string(e.c) }
func (e classed) ErrClass() Class { return e.c }

func TestOf(t *testing.T) {
	cases := []struct {
		err  error
		want Class
	}{
		{nil, None},
		{context.Canceled, Canceled},
		{fmt.Errorf("wrapped: %w", context.DeadlineExceeded), Timeout},
		{fmt.Errorf("x: %w", classed{Quota}), Quota},
		{errors.New("something"), Unknown},
	}
	for _, c := range cases {
		if got := Of(c.err); got != c.want {
			t.Errorf("Of(%v) = %q, want %q", c.err, got, c.want)
		}
	}
}

func TestFromStatusAndRetry(t *testing.T) {
	for status, want := range map[int]Class{200: None, 401: Auth, 402: Quota, 403: Permission, 404: NotFound, 429: RateLimited,
		400: BadRequest, 422: BadRequest, 500: Overloaded, 503: Overloaded, 504: Timeout, 413: ContextLength} {
		if got := FromStatus(status); got != want {
			t.Errorf("FromStatus(%d) = %q, want %q", status, got, want)
		}
	}
	if !RateLimited.Retryable() || !Overloaded.Retryable() || Auth.Retryable() || BadRequest.Retryable() || Permission.Retryable() {
		t.Fatal("retryable classes")
	}
}
