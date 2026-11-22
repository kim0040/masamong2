package application

import (
	"testing"

	"masamong/internal/clock"
	"masamong/internal/config"
)

// forgetCaps drops what the service learned about its providers, as a new process would start.
func forgetCaps(s *Service) {
	s.caps.mu.Lock()
	s.caps.slots = nil
	s.caps.mu.Unlock()
}

// twoServices opens two services side by side, as the daemon runs the real instance and the console's
// practice instance in one process.
func twoServices(t *testing.T) (*Service, *Service) {
	t.Helper()
	a := &Service{Clock: clock.Real{}, watchers: map[chan Event]struct{}{}}
	b := &Service{Clock: clock.Real{}, watchers: map[chan Event]struct{}{}}
	t.Cleanup(func() { _ = a.Close(); _ = b.Close() })
	return a, b
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
}
