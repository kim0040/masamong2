package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/errclass"
	"masamong/internal/providers"
)

// host is a fake chat host: status (0 = 200 with text), what model it was asked for, how often.
type host struct {
	status int
	text   string
	delay  time.Duration
	cost   float64
	calls  atomic.Int32
	model  atomic.Value
}

func (h *host) adapter(t *testing.T) *Adapter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.calls.Add(1)
		var body struct {
			Model string `json:"model"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.model.Store(body.Model)
		if h.delay > 0 {
			select {
			case <-time.After(h.delay):
			case <-r.Context().Done():
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		if h.status != 0 {
			w.WriteHeader(h.status)
			_, _ = w.Write([]byte(`{"error":{"message":"nope"}}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": h.text}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "cost": h.cost},
		})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FAILOVER_KEY", "k")
	cred, _ := providers.Resolve("env:FAILOVER_KEY")
	c := providers.NewClient(5*time.Second, nil)
	c.MaxRetries = 0
	return New(c, srv.URL, cred)
}

func chain(t *testing.T, b *Breaker, primary *host, fallbacks ...*host) *Adapter {
	a := primary.adapter(t)
	a.Failover = &Failover{Provider: "p0", Model: "m0", Timeout: time.Second, Breaker: b}
	for i, h := range fallbacks {
		a.Failover.Fallbacks = append(a.Failover.Fallbacks, Fallback{A: h.adapter(t), Provider: "p" + string(rune('1'+i)), Model: "m" + string(rune('1'+i))})
	}
	return a
}

var hi = ChatRequest{Model: "m0", Messages: []Message{{Role: "user", Content: "hi"}}}

func TestFailoverMovesOnAndNamesWhoAnswered(t *testing.T) {
	for _, status := range []int{429, 500, 503, 401, 402, 404} {
		p, f := &host{status: status, cost: 0.001}, &host{text: "from fallback", cost: 0.002}
		out, err := chain(t, nil, p, f).Chat(context.Background(), hi)
		if err != nil || out.Content != "from fallback" || out.Provider != "p1" || !out.Fallback {
			t.Fatalf("%d: %+v %v", status, out, err)
		}
		if f.model.Load() != "m1" || len(out.Attempts) != 2 || out.Attempts[0].Class == "" {
			t.Fatalf("%d: the fallback is asked for its own model: %v %+v", status, f.model.Load(), out.Attempts)
		}
	}
}

func TestFailoverKeepsRequestErrors(t *testing.T) {
	p, f := &host{status: 400}, &host{text: "x"}
	_, err := chain(t, nil, p, f).Chat(context.Background(), hi)
	if err == nil || f.calls.Load() != 0 {
		t.Fatalf("a malformed request is not sent elsewhere: %v, fallback calls %d", err, f.calls.Load())
	}
}

func TestFailoverOnEmptyAnswerAndTimeout(t *testing.T) {
	p, f := &host{text: "  "}, &host{text: "ok"}
	if out, err := chain(t, nil, p, f).Chat(context.Background(), hi); err != nil || out.Content != "ok" || out.Attempts[0].Class != errclass.Empty {
		t.Fatalf("empty: %+v %v", out, err)
	}
	slow, f2 := &host{delay: 3 * time.Second, text: "late"}, &host{text: "quick"}
	began := time.Now()
	out, err := chain(t, nil, slow, f2).Chat(context.Background(), hi)
	if err != nil || out.Content != "quick" || out.Attempts[0].Class != errclass.Timeout || time.Since(began) > 2500*time.Millisecond {
		t.Fatalf("timeout: %+v %v after %s", out, err, time.Since(began))
	}
}

// A tool round the model ends on purpose with nothing to add (no text, no calls, finish_reason stop)
// is its answer: asking the fallback would only cost a call and seconds.
func TestFailoverTakesADeliberateEmptyToolRound(t *testing.T) {
	p, f := &host{text: ""}, &host{text: "fallback"}
	req := hi
	req.EmptyOK = true
	out, err := chain(t, nil, p, f).Chat(context.Background(), req)
	if err != nil || out.Content != "" || out.Fallback || f.calls.Load() != 0 {
		t.Fatalf("empty tool round: %+v %v, fallback calls %d", out, err, f.calls.Load())
	}
}

func TestFailoverAddsUpCostAndReportsLastError(t *testing.T) {
	p, f := &host{text: "", cost: 0.001}, &host{status: 503}
	out, err := chain(t, nil, p, f).Chat(context.Background(), hi)
	if err == nil || out.Provider != "p1" || len(out.Attempts) != 2 {
		t.Fatalf("both failed: %+v %v", out, err)
	}
	if out.CostUSD == nil || *out.CostUSD != 0.001 {
		t.Fatalf("the empty answer's cost is kept: %v", out.CostUSD)
	}
	if errclass.Of(err) != errclass.Overloaded {
		t.Fatalf("the last error's class: %v", errclass.Of(err))
	}
}

func TestFailoverOnlyForTheConfiguredModel(t *testing.T) {
	p, f := &host{status: 500}, &host{text: "x"}
	req := hi
	req.Model = "another"
	if _, err := chain(t, nil, p, f).Chat(context.Background(), req); err == nil || f.calls.Load() != 0 {
		t.Fatal("a probe of another model does not fail over")
	}
}

func TestBreakerRestsAFailingModel(t *testing.T) {
	now := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	b := NewBreaker(func() time.Time { return now })
	p, f := &host{status: 402}, &host{text: "ok"}
	a := chain(t, b, p, f)
	_, _ = a.Chat(context.Background(), hi)
	_, _ = a.Chat(context.Background(), hi)
	if p.calls.Load() != 1 || f.calls.Load() != 2 {
		t.Fatalf("no credit rests the primary: primary %d fallback %d", p.calls.Load(), f.calls.Load())
	}
	if s := b.Snapshot(); len(s) != 1 || s[0].Key != "p0/m0" || s[0].Class != errclass.Quota {
		t.Fatalf("snapshot %+v", s)
	}
	now = now.Add(authRest + time.Second)
	p.status = 0
	p.text = "back"
	if out, _ := a.Chat(context.Background(), hi); out.Content != "back" || len(b.Snapshot()) != 0 {
		t.Fatalf("after the rest the primary is asked again: %+v", out)
	}

	// ordinary failures rest a model only after failRun in a row
	p2, f2 := &host{status: 500}, &host{text: "ok"}
	a2 := chain(t, b, p2, f2)
	a2.Failover.Model = "m0"
	a2.Failover.Provider = "q0"
	for range failRun + 2 {
		_, _ = a2.Chat(context.Background(), hi)
	}
	if p2.calls.Load() != failRun {
		t.Fatalf("rested after %d failures: %d calls", failRun, p2.calls.Load())
	}
}

func TestBreakerNeverSkipsTheLast(t *testing.T) {
	b := NewBreaker(nil)
	p, f := &host{status: 401}, &host{status: 401}
	a := chain(t, b, p, f)
	for range 3 {
		_, _ = a.Chat(context.Background(), hi)
	}
	if f.calls.Load() != 3 {
		t.Fatalf("the last model is always asked: %d", f.calls.Load())
	}
}

func TestFailoverStopsOnCancel(t *testing.T) {
	p, f := &host{delay: 2 * time.Second}, &host{text: "x"}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	_, err := chain(t, nil, p, f).Chat(ctx, hi)
	if err == nil || f.calls.Load() != 0 || !errors.Is(err, context.DeadlineExceeded) && errclass.Of(err) != errclass.Timeout {
		t.Fatalf("the caller's deadline ends the chain: %v, fallback %d", err, f.calls.Load())
	}
}
