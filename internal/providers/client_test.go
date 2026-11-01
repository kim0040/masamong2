package providers

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/errclass"
)

func noSleep(context.Context, time.Duration) error { return nil }

func TestRetriesTransientThenSucceeds(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&n, 1) < 3 {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(503)
			return
		}
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	c := NewClient(time.Second, nil)
	c.Sleep = noSleep
	resp, err := c.Get(context.Background(), srv.URL+"/x?key=abc", nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Attempts != 3 || resp.Status != 200 {
		t.Fatalf("attempts %d status %d", resp.Attempts, resp.Status)
	}
	if strings.Contains(resp.URL, "abc") {
		t.Fatalf("url not redacted: %s", resp.URL)
	}
}

func TestGivesUpAfterMaxRetries(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(429) }))
	defer srv.Close()
	c := NewClient(time.Second, nil)
	c.Sleep = noSleep
	resp, err := c.Get(context.Background(), srv.URL, nil)
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != 429 || !pe.Retryable() || pe.Attempts != 3 {
		t.Fatalf("err %v", err)
	}
	if resp == nil || resp.Status != 429 {
		t.Fatal("response body must still be returned for diagnostics")
	}
}

func TestNonRetryable4xx(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&n, 1)
		w.WriteHeader(401)
	}))
	defer srv.Close()
	c := NewClient(time.Second, nil)
	c.Sleep = noSleep
	_, err := c.Get(context.Background(), srv.URL, nil)
	var pe *Error
	if !errors.As(err, &pe) || pe.Status != 401 || pe.Retryable() || n != 1 {
		t.Fatalf("401 must not retry: %v n=%d", err, n)
	}
}

func TestTimeoutClassified(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(200 * time.Millisecond) }))
	defer srv.Close()
	c := NewClient(30*time.Millisecond, nil)
	c.Sleep = noSleep
	c.MaxRetries = 0
	_, err := c.Get(context.Background(), srv.URL, nil)
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != "timeout" {
		t.Fatalf("timeout kind: %v", err)
	}
}

func TestBodyCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(make([]byte, 100)) }))
	defer srv.Close()
	c := NewClient(time.Second, nil)
	c.MaxBody = 50
	_, err := c.Get(context.Background(), srv.URL, nil)
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != "too_large" {
		t.Fatalf("cap: %v", err)
	}
}

func TestCrossHostRedirectRefused(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte("leak")) }))
	defer other.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, other.URL, http.StatusFound)
	}))
	defer srv.Close()
	c := NewClient(time.Second, nil)
	c.MaxRetries = 0
	if _, err := c.Get(context.Background(), srv.URL, map[string]string{"Authorization": "Bearer x"}); err == nil {
		t.Fatal("cross-host redirect must be refused")
	}
}

func TestResolveCredential(t *testing.T) {
	t.Setenv("X_KEY", " abc ")
	c, err := Resolve("env:X_KEY")
	if err != nil || !c.Present() || c.Value() != "abc" {
		t.Fatalf("env: %v %+v", err, c)
	}
	t.Setenv("X_PLACEHOLDER", "your_api_key_here")
	c, _ = Resolve("env:X_PLACEHOLDER")
	if c.Present() {
		t.Fatal("placeholder must count as absent")
	}
	c, err = Resolve("")
	if err != nil || c.Present() {
		t.Fatal("empty ref is absent, not an error")
	}
	if _, err := Resolve("vault:foo"); err == nil {
		t.Fatal("unknown scheme must error")
	}
	if _, err := Resolve("file:/nonexistent/path/key"); err != nil {
		t.Fatalf("missing file is absent: %v", err)
	}
	if c.String() != "<none>" {
		t.Fatalf("string %q", c.String())
	}
}

func TestQuota(t *testing.T) {
	q := NewQuota(map[string]int{"kma": 2})
	for i := 0; i < 2; i++ {
		if _, _, ok := q.Reserve("kma"); !ok {
			t.Fatal("under limit")
		}
	}
	if _, _, ok := q.Reserve("kma"); ok {
		t.Fatal("over limit")
	}
	if _, _, ok := q.Reserve("other"); !ok {
		t.Fatal("unlimited provider")
	}
}

func TestCrossHostRedirectIsNotRetried(t *testing.T) {
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	defer other.Close()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		http.Redirect(w, r, strings.Replace(other.URL, "127.0.0.1", "localhost", 1)+"/x", http.StatusMovedPermanently)
	}))
	defer srv.Close()
	c := NewClient(2*time.Second, nil)
	c.RetryBase = time.Millisecond
	_, err := c.Get(context.Background(), srv.URL, nil)
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != "redirect" || pe.Retryable() || hits != 1 || !strings.Contains(err.Error(), "moved to localhost") {
		t.Fatalf("redirect: hits=%d err=%v", hits, err)
	}
}

// Retry-After is read as delay seconds or as an HTTP date; a wait that is not above zero or is longer
// than the cap is not used.
func TestParseRetryAfter(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	date := func(d time.Duration) string { return now.Add(d).Format(http.TimeFormat) }
	for in, want := range map[string]time.Duration{
		"1": time.Second, " 30 ": 30 * time.Second, date(5 * time.Second): 5 * time.Second, date(30 * time.Second): 30 * time.Second,
		now.Add(10 * time.Second).Format(time.RFC850): 10 * time.Second, now.Add(2 * time.Second).Format(time.ANSIC): 2 * time.Second,
	} {
		if got, ok := parseRetryAfter(in, now); !ok || got != want {
			t.Errorf("%q → %v %v, want %v", in, got, ok, want)
		}
	}
	for _, in := range []string{"", "0", "-3", "31", "3600", "1.5", "soon", date(0), date(-time.Minute), date(31 * time.Second)} {
		if got, ok := parseRetryAfter(in, now); ok {
			t.Errorf("%q must not be used, got %v", in, got)
		}
	}
}

// The backoff doubles per attempt, half of it jittered, never above the old linear waits for the default
// two retries and never above the cap.
func TestBackoffIsExponentialWithBoundedJitter(t *testing.T) {
	c := NewClient(time.Second, nil)
	c.RetryBase = 500 * time.Millisecond
	for _, j := range []float64{0, 0.5, 0.999999, 1, -1, 2} {
		c.Jitter = func() float64 { return j }
		total := time.Duration(0)
		for attempt := 1; attempt <= 2; attempt++ {
			d := c.backoff(attempt)
			lo, hi := c.RetryBase<<(attempt-1)/2, c.RetryBase<<(attempt-1)
			if d < lo || d > hi {
				t.Fatalf("jitter %v attempt %d: %v outside [%v, %v]", j, attempt, d, lo, hi)
			}
			total += d
		}
		if total > 1500*time.Millisecond { // what the linear backoff waited at most
			t.Fatalf("jitter %v: total %v", j, total)
		}
	}
	c.Jitter = func() float64 { return 0 }
	if a, b := c.backoff(1), c.backoff(2); a != 250*time.Millisecond || b != 500*time.Millisecond {
		t.Fatalf("lower bounds %v %v", a, b)
	}
	c.Jitter = func() float64 { return 1 }
	if d := c.backoff(40); d != maxRetryWait {
		t.Fatalf("cap: %v", d)
	}
	c.Jitter = nil // the real source stays in range
	for range 100 {
		if d := c.backoff(1); d < 250*time.Millisecond || d > 500*time.Millisecond {
			t.Fatalf("random backoff %v", d)
		}
	}
}

// A 503 with a Retry-After date waits until that date; without one the jittered backoff is used.
func TestRetryWaitsUseRetryAfterDateOrBackoff(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&n, 1) {
		case 1:
			w.Header().Set("Retry-After", now.Add(7*time.Second).Format(http.TimeFormat))
			w.WriteHeader(503)
		case 2:
			w.WriteHeader(429)
		default:
			_, _ = w.Write([]byte(`{}`))
		}
	}))
	defer srv.Close()
	var waits []time.Duration
	c := NewClient(time.Second, nil)
	c.Sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	c.Now = func() time.Time { return now }
	c.Jitter = func() float64 { return 0.5 }
	if _, err := c.Get(context.Background(), srv.URL, nil); err != nil {
		t.Fatal(err)
	}
	if len(waits) != 2 || waits[0] != 7*time.Second || waits[1] != 750*time.Millisecond {
		t.Fatalf("waits %v", waits)
	}
}

// A caller's deadline that passes while the client waits to retry is still a timeout (the class the
// provider-outage handling reads), not a cancel; a cancelled context stays a cancel.
func TestDeadlineDuringRetryWaitIsATimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { time.Sleep(100 * time.Millisecond) }))
	defer srv.Close()
	c := NewClient(time.Minute, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	_, err := c.Get(ctx, srv.URL, nil)
	var pe *Error
	if !errors.As(err, &pe) || pe.Kind != "timeout" || errclass.Of(err) != errclass.Timeout {
		t.Fatalf("deadline kind: %v", err)
	}
	ctx2, cancel2 := context.WithCancel(context.Background())
	time.AfterFunc(30*time.Millisecond, cancel2)
	_, err = c.Get(ctx2, srv.URL, nil)
	if !errors.As(err, &pe) || pe.Kind != "cancelled" {
		t.Fatalf("cancel kind: %v", err)
	}
}
