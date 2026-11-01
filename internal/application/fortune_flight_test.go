package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/config"
)

func asCtx(ctx context.Context, svc *application.Service, act string, in map[string]any, inv *action.Invoker) action.Result {
	raw, _ := json.Marshal(in)
	return svc.Execute(ctx, action.Request{Action: act, Input: raw, Locale: "en", Invoker: inv})
}

// While one sign's text is being written by the model, a cached sign, another sign and a request whose own
// deadline passes are not held up by it, and a second asker of the same sign waits for that one text
// instead of paying for another.
func TestZodiacGenerationDoesNotHoldEveryoneElse(t *testing.T) {
	var calls atomic.Int32
	var block atomic.Bool
	started := make(chan struct{}, 1)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		if block.CompareAndSwap(true, false) {
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "**오늘의 흐름**: 85점"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.00001},
		})
	}))
	t.Cleanup(srv.Close)
	var releaseOnce sync.Once
	releaseModel := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseModel) // runs before srv.Close: a failed test must not leave the handler waiting
	svc := routedSvc(t, srv.URL, srv.URL)
	applyDoc(t, svc, func(d *config.Document) { d.Discord.Enabled = true })
	serveAnyServer(t, svc)
	query := func(ctx context.Context, sign, user string) action.Result {
		return asCtx(ctx, svc, "fortune.query", map[string]any{"what": "zodiac", "sign": sign}, person(user, true))
	}
	if r := query(context.Background(), "3", "flight-u1"); !r.OK { // warm the cache of sign 3
		t.Fatalf("warm-up: %+v", r)
	}

	block.Store(true)
	slow := make(chan action.Result, 1)
	go func() { slow <- query(context.Background(), "1", "flight-u2") }()
	select {
	case <-started:
	case <-time.After(10 * time.Second):
		t.Fatal("the model call never started")
	}
	// the asker of the same sign that waits for the text being written
	waiter := make(chan action.Result, 1)
	go func() { waiter <- query(context.Background(), "1", "flight-u6") }()

	within := func(name string, fn func() action.Result) action.Result {
		t.Helper()
		out := make(chan action.Result, 1)
		go func() { out <- fn() }()
		select {
		case r := <-out:
			return r
		case <-time.After(5 * time.Second):
			t.Fatalf("%s was held up by another sign's model call", name)
			return action.Result{}
		}
	}
	if r := within("a cached sign", func() action.Result { return query(context.Background(), "3", "flight-u3") }); !r.OK || r.Output["cached"] != true {
		t.Fatalf("cached sign: %+v", r)
	}
	if r := within("another sign", func() action.Result { return query(context.Background(), "2", "flight-u4") }); !r.OK || r.Output["cached"] != false {
		t.Fatalf("other sign: %+v", r)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if r := within("a request past its deadline", func() action.Result { return query(ctx, "1", "flight-u5") }); r.OK {
		t.Fatalf("a request whose deadline passed was answered: %+v", r)
	}

	releaseModel()
	select {
	case r := <-slow:
		if !r.OK || r.Output["cached"] != false {
			t.Fatalf("generating request: %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("generating request never finished")
	}
	select {
	case r := <-waiter:
		if !r.OK || r.Output["cached"] != true {
			t.Fatalf("waiting request: %+v", r)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("waiting request never finished")
	}
	if n := calls.Load(); n != 3 { // sign 3 once, sign 2 once, sign 1 once for both askers
		t.Fatalf("%d model calls, want 3", n)
	}
}
