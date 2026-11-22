package application

import (
	"context"
	"log/slog"
	"testing"

	"masamong/internal/action"
	"masamong/internal/config"
)

// forgetCaps drops what the service learned about its providers, as a new process would start.
func forgetCaps(s *Service) {
	s.caps.mu.Lock()
	s.caps.slots = nil
	s.caps.mu.Unlock()
}

// hookedService is privacyService with a test's hooks given to the constructor.
func hookedService(t *testing.T, hooks serviceHooks) *Service {
	t.Helper()
	s, err := openHooked(t.TempDir(), hooks)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if r := s.Execute(context.Background(), action.Request{Action: "setup.resume", Input: []byte(`{"instance":"hooked"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	return s
}

// twoServices opens two set-up services side by side, as the daemon runs the real instance and the
// console's practice instance in one process.
func twoServices(t *testing.T) (*Service, *Service) {
	t.Helper()
	return privacyService(t), privacyService(t)
}

// State that belongs to one service lives in that service: two services in one process never see
// each other's caches, flags or queues, and a closed service leaves nothing behind in package state.
func TestTwoServicesKeepTheirOwnState(t *testing.T) {
	a, b := twoServices(t)

	t.Run("capability caches", func(t *testing.T) {
		p := config.Provider{Kind: config.KindOpenAI, BaseURL: "http://127.0.0.1:1/v1"}
		if a.modelCaps("p", p) == b.modelCaps("p", p) {
			t.Fatal("two services share one capability cache")
		}
		if a.modelCaps("p", p) != a.modelCaps("p", p) {
			t.Fatal("a service must keep its own cache between calls")
		}
	})

	t.Run("price lists", func(t *testing.T) {
		if a.prices() == b.prices() {
			t.Fatal("two services share one price table")
		}
		a.prices().mu.Lock()
		a.prices().lists["p"] = &priceList{}
		a.prices().mu.Unlock()
		if b.prices().lists["p"] != nil {
			t.Fatal("a price list of one service shows up in the other")
		}
	})

	t.Run("consent cache", func(t *testing.T) {
		c := a.optOuts()
		c.mu.Lock()
		c.loaded, c.ids = true, map[string]bool{"100000000000000001": true}
		c.mu.Unlock()
		if b.optOuts() == c || b.optOuts().ids["100000000000000001"] {
			t.Fatal("one service's withdrawn people show up in the other")
		}
	})

	t.Run("room revisions", func(t *testing.T) {
		a.summariesRevised([]string{"discord:g:c"})
		if a.roomRevision("discord:g:c") != 1 || b.roomRevision("discord:g:c") != 0 {
			t.Fatal("an edit seen by one service counts in the other")
		}
	})

	t.Run("restart requests", func(t *testing.T) {
		a.requestLifecycle(lifecycleRestart)
		select {
		case l := <-b.LifecycleRequests():
			t.Fatalf("one service's %s request reached the other", l)
		default:
		}
		if l := <-a.LifecycleRequests(); l != lifecycleRestart {
			t.Fatalf("the request read back as %q", l)
		}
	})

	t.Run("live log", func(t *testing.T) {
		a.ring().add(logEntry{Msg: "only in a"})
		b.ring().mu.Lock()
		defer b.ring().mu.Unlock()
		for _, e := range b.ring().entries {
			if e.Msg == "only in a" {
				t.Fatal("one service's log line shows up in the other's live log")
			}
		}
	})

	t.Run("memory engine", func(t *testing.T) {
		ea, err := a.MemoryEngine()
		if err != nil {
			t.Fatal(err)
		}
		eb, err := b.MemoryEngine()
		if err != nil {
			t.Fatal(err)
		}
		if ea == eb {
			t.Fatal("two services share one memory engine")
		}
	})

	t.Run("one-at-a-time marks", func(t *testing.T) {
		// a usage_gc pass running in one service does not keep the other's from starting
		a.runs.retention.Store(true)
		defer a.runs.retention.Store(false)
		if !b.startRetentionPass(context.Background()) {
			t.Fatal("one service's running pass blocked the other's")
		}
		if a.startRetentionPass(context.Background()) {
			t.Fatal("a service started a second pass beside its running one")
		}
	})

	t.Run("tool usage", func(t *testing.T) {
		if a.toolUsage() == nil || a.toolUsage() == b.toolUsage() || a.toolUsage() != a.toolUsage() {
			t.Fatal("each service opens and keeps its own tool usage counter")
		}
	})
}

// The practice instance is found by its folder while it is open, and forgotten when it closes, so the
// real instance's deletions reach it and a closed one is not kept alive.
func TestPracticeRegistryFollowsOpenAndClose(t *testing.T) {
	s := privacyService(t)
	p, err := OpenPractice(context.Background(), s.DataRoot, "en", slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if got := openPractices.find(PracticeRoot(s.DataRoot)); got != p {
		t.Fatalf("open practice instance not found: %p, want %p", got, p)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if got := openPractices.find(PracticeRoot(s.DataRoot)); got != nil {
		t.Fatal("a closed practice instance is still registered")
	}
}
