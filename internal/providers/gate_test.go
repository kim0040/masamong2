package providers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// A gated client never has more requests in flight than its gate, however many callers it has at once;
// the ungated client it was copied from is not bound by it. A caller that gives up while it waits for a
// slot is told it was cancelled.
func TestGateBoundsRequestsInFlight(t *testing.T) {
	var now, most atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := now.Add(1)
		for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
		}
		time.Sleep(40 * time.Millisecond)
		now.Add(-1)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	burst := func(c *Client) int32 {
		most.Store(0)
		var wg sync.WaitGroup
		for range 4 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if _, err := c.Get(context.Background(), srv.URL, nil); err != nil {
					t.Error(err)
				}
			}()
		}
		wg.Wait()
		return most.Load()
	}
	base := NewClient(5*time.Second, nil)
	gated := base.WithGate(2)
	if got := burst(gated); got != 2 {
		t.Fatalf("gate of 2: %d in flight", got)
	}
	if got := burst(base); got != 4 {
		t.Fatalf("no gate: %d in flight", got)
	}
	if base.WithGate(0) != base || base.Gate != nil {
		t.Fatal("no bound must leave the client as it is")
	}

	one := base.WithGate(1)
	one.Gate <- struct{}{} // another request holds the only slot
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	_, err := one.Get(ctx, srv.URL, nil)
	if e, ok := err.(*Error); !ok || e.Kind != "timeout" || !e.NotSent {
		t.Fatalf("waiting past the deadline for a slot is a timeout of a request never sent: %#v", err)
	}
}

type gateTransport func(*http.Request) (*http.Response, error)

func (f gateTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type panicBody struct {
	where  string
	closed int
}

func (b *panicBody) Read([]byte) (int, error) {
	if b.where == "read" {
		panic("test read panic")
	}
	return 0, io.EOF
}
func (b *panicBody) Close() error {
	b.closed++
	if b.where == "close" {
		panic("test close panic")
	}
	return nil
}

func TestGateReleasedAfterAttemptPanic(t *testing.T) {
	for _, where := range []string{"transport", "read", "close"} {
		t.Run(where, func(t *testing.T) {
			c := NewClient(time.Second, nil).WithGate(1)
			c.InProcess, c.MaxRetries = true, 0
			body := &panicBody{where: where}
			c.HTTP.Transport = gateTransport(func(*http.Request) (*http.Response, error) {
				if where == "transport" {
					panic("test transport panic")
				}
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: body}, nil
			})
			func() {
				defer func() {
					if recover() == nil {
						t.Error("expected panic")
					}
				}()
				_, _ = c.Get(context.Background(), "https://example.test", nil)
			}()
			if got := len(c.Gate); got != 0 {
				t.Errorf("leaked %d gate slots", got)
			}
			if where != "transport" && body.closed != 1 {
				t.Errorf("body closed %d times", body.closed)
			}
			c.HTTP.Transport = gateTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if _, err := c.Get(ctx, "https://example.test", nil); err != nil {
				t.Error("next request failed:", err)
			}
			if len(c.Gate) != 0 {
				t.Error("gate not empty after success")
			}
		})
	}
}

func TestGateReleasedDuringRetryBackoff(t *testing.T) {
	c := NewClient(time.Second, nil).WithGate(1)
	c.InProcess, c.MaxRetries = true, 1
	var attempts int
	c.HTTP.Transport = gateTransport(func(*http.Request) (*http.Response, error) {
		attempts++
		status := 200
		if attempts == 1 {
			status = 503
		}
		return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{}`))}, nil
	})
	c.Sleep = func(context.Context, time.Duration) error {
		if len(c.Gate) != 0 {
			t.Error("retry sleep held a slot")
		}
		return nil
	}
	if _, err := c.Get(context.Background(), "https://example.test", nil); err != nil {
		t.Fatal(err)
	}
}

func sharedTestGate(c *Client, n int) *Client {
	if g, ok := any(c).(interface {
		WithProviderGate(string, string, Credential, int) *Client
	}); ok {
		return g.WithProviderGate("capacity-test", "https://capacity.test", Credential{}, n)
	}
	return c.WithGate(n)
}

func TestSharedGateCapacityChangeKeepsOldRequests(t *testing.T) {
	old := sharedTestGate(NewClient(time.Second, nil), 2)
	if err := old.enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := old.enter(context.Background()); err != nil {
		t.Fatal(err)
	}
	defer old.leave()
	defer old.leave()
	current := sharedTestGate(NewClient(time.Second, nil), 1)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := current.enter(ctx); err == nil {
		current.leave()
		t.Error("resized governor forgot old requests")
	}
	// Increasing capacity wakes a waiter while old clients continue to use the same governor.
	current = sharedTestGate(NewClient(time.Second, nil), 3)
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	if err := current.enter(ctx2); err != nil {
		t.Fatal(err)
	}
	current.leave()
}
