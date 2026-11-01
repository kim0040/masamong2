package openai

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"masamong/internal/errclass"
)

// Failover: a role's model may name fallbacks (config models.<role>.fallbacks), each another key, host
// or model. Chat asks them in order when the model before failed in a way another host or model can
// help with, so callers keep one Adapter and one model name and need no failover code of their own.
//
// What moves on to the next one: no answer in time, a network error, 429, 5xx, a refused key, no
// credit left, an unknown model, an empty answer, an answer that did not parse. What does not: a
// request the host calls malformed (it is ours to fix, and every host would refuse it), a prompt too
// long, a feature the model lacks (the callers have their own paths for pictures and tools), and our
// own cancel. A model that keeps failing is skipped for a while (Breaker), so turns do not each wait
// out its time limit; the last one in the list is always asked.

// Fallback is one stand-in model.
type Fallback struct {
	A        *Adapter
	Provider string // the config provider id, for logs and the call ledger
	Model    string
	Timeout  time.Duration // 0 = the primary's
}

// Failover is the primary model's side of the chain, set on the adapter the role's model uses.
type Failover struct {
	Provider  string // the primary's provider id
	Model     string // the primary model: only requests for it fail over (a console probe of another model does not)
	Timeout   time.Duration
	Fallbacks []Fallback
	Breaker   *Breaker
	// OnFail hears each attempt that failed and was followed by another (provider health).
	OnFail func(provider, model string, err error)
}

// Attempt is one model asked during a Chat that failed over.
type Attempt struct {
	Provider, Model string
	Class           errclass.Class // "" when it answered
	Skipped         bool           // the breaker had it resting; not called
	Elapsed         time.Duration
}

// failoverWorthy reports whether another host or model may succeed where this call failed.
func failoverWorthy(err error) bool {
	if errors.Is(err, ErrVisionUnsupported) || errors.Is(err, ErrToolsUnsupported) {
		return false
	}
	switch errclass.Of(err) {
	case errclass.BadRequest, errclass.ContextLength, errclass.Unsupported, errclass.Canceled:
		return false
	}
	return true
}

// errEmpty is an answer with no text and no tool calls.
var errEmpty = classedError{"the model returned no text", errclass.Empty}

func emptyAnswer(out ChatResult) bool {
	return strings.TrimSpace(out.Content) == "" && len(out.ToolCalls) == 0
}

// chatFailover runs req through the chain. ctx bounds the whole chain; each attempt also has its own
// time limit, so a model that hangs leaves time for the next.
func (a *Adapter) chatFailover(ctx context.Context, req ChatRequest) (ChatResult, error) {
	f := a.Failover
	chain := make([]Fallback, 0, 1+len(f.Fallbacks))
	chain = append(chain, Fallback{A: a, Provider: f.Provider, Model: req.Model, Timeout: f.Timeout})
	for _, fb := range f.Fallbacks {
		if fb.A != nil && fb.Model != "" {
			if fb.Timeout == 0 {
				fb.Timeout = f.Timeout
			}
			chain = append(chain, fb)
		}
	}
	var (
		attempts []Attempt
		spent    float64
		billed   bool
		firstErr error
		lastOut  ChatResult
		lastErr  error
	)
	for i, link := range chain {
		key := link.Provider + "/" + link.Model
		last := i == len(chain)-1
		if !last && f.Breaker.Resting(key) {
			attempts = append(attempts, Attempt{Provider: link.Provider, Model: link.Model, Skipped: true})
			continue
		}
		r := req
		r.Model = link.Model
		actx, cancel := ctx, context.CancelFunc(func() {})
		if link.Timeout > 0 {
			actx, cancel = context.WithTimeout(ctx, link.Timeout)
		}
		began := time.Now()
		out, err := link.A.chat(actx, r)
		cancel()
		if err == nil && emptyAnswer(out) && out.FinishReason != "tool_calls" && !(req.EmptyOK && (out.FinishReason == "stop" || out.FinishReason == "")) {
			err = fmt.Errorf("%w (finish_reason=%s)", errEmpty, out.FinishReason)
		}
		if err != nil && ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			err = classedError{fmt.Sprintf("no answer within %s", link.Timeout), errclass.Timeout}
		}
		at := Attempt{Provider: link.Provider, Model: link.Model, Class: errclass.Of(err), Elapsed: time.Since(began)}
		attempts = append(attempts, at)
		if out.CostUSD != nil {
			spent += *out.CostUSD
			billed = true
		}
		if err == nil {
			f.Breaker.Succeeded(key)
			out.Provider, out.Fallback, out.Attempts = link.Provider, i > 0, attempts
			out.CostUSD = nil
			if billed {
				out.CostUSD = &spent
			}
			return out, nil
		}
		f.Breaker.Failed(key, at.Class)
		if firstErr == nil {
			firstErr = err
		}
		lastOut, lastErr = out, err
		if ctx.Err() != nil || !failoverWorthy(err) {
			break
		}
		if !last && f.OnFail != nil {
			f.OnFail(link.Provider, link.Model, err)
		}
	}
	lastOut.Attempts = attempts
	for i := len(attempts) - 1; i >= 0; i-- {
		if !attempts[i].Skipped {
			lastOut.Provider = attempts[i].Provider
			break
		}
	}
	lastOut.CostUSD = nil
	if billed {
		lastOut.CostUSD = &spent
	}
	if lastErr == nil { // every model was resting but the last, which is always asked: not reached
		lastErr = firstErr
	}
	if firstErr != nil && firstErr != lastErr {
		return lastOut, fmt.Errorf("%w (first: %v)", lastErr, firstErr)
	}
	return lastOut, lastErr
}

// Breaker remembers models that failed recently, shared by every adapter of a service (adapters are
// built per call). A refused key or no credit rests the model for authRest; being rate limited for
// rateRest; other failures rest it for failRest once it failed failRun times in a row. After the rest
// the next call tries it again, and one success clears it.
type Breaker struct {
	mu    sync.Mutex
	now   func() time.Time
	state map[string]*breakerState
}

type breakerState struct {
	run   int
	until time.Time
	class errclass.Class
}

const (
	authRest = 10 * time.Minute
	rateRest = time.Minute
	failRest = time.Minute
	failRun  = 3
)

// NewBreaker makes a breaker on the given clock (nil = time.Now).
func NewBreaker(now func() time.Time) *Breaker {
	if now == nil {
		now = time.Now
	}
	return &Breaker{now: now, state: map[string]*breakerState{}}
}

// Resting reports whether key is being skipped.
func (b *Breaker) Resting(key string) bool {
	if b == nil {
		return false
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.state[key]
	return st != nil && b.now().Before(st.until)
}

func (b *Breaker) Succeeded(key string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	delete(b.state, key)
	b.mu.Unlock()
}

func (b *Breaker) Failed(key string, class errclass.Class) {
	if b == nil || !failoverClass(class) {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	st := b.state[key]
	if st == nil {
		st = &breakerState{}
		b.state[key] = st
	}
	st.run++
	st.class = class
	now := b.now()
	switch {
	case class == errclass.Auth || class == errclass.Quota || class == errclass.Permission:
		st.until = now.Add(authRest)
	case class == errclass.RateLimited:
		st.until = now.Add(rateRest)
	case st.run >= failRun:
		st.until = now.Add(failRest)
	}
}

func failoverClass(c errclass.Class) bool {
	switch c {
	case errclass.None, errclass.BadRequest, errclass.ContextLength, errclass.Unsupported, errclass.Canceled:
		return false
	}
	return true
}

// BreakerEntry is a resting model, for the console.
type BreakerEntry struct {
	Key   string         `json:"key"`
	Until time.Time      `json:"until"`
	Class errclass.Class `json:"class"`
}

// Snapshot lists the models resting now.
func (b *Breaker) Snapshot() []BreakerEntry {
	if b == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	var out []BreakerEntry
	for k, st := range b.state {
		if now.Before(st.until) {
			out = append(out, BreakerEntry{Key: k, Until: st.until, Class: st.class})
		}
	}
	return out
}
