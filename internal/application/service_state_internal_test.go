package application

import (
	"testing"

	"masamong/internal/config"
)

// forgetCaps drops what the service learned about its providers, as a new process would start.
func forgetCaps(s *Service) {
	s.caps.mu.Lock()
	s.caps.slots = nil
	s.caps.mu.Unlock()
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

	t.Run("tool usage", func(t *testing.T) {
		if a.toolUsage() == nil || a.toolUsage() == b.toolUsage() || a.toolUsage() != a.toolUsage() {
			t.Fatal("each service opens and keeps its own tool usage counter")
		}
	})
}
