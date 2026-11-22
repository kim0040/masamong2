package application

import (
	"sync"
	"testing"

	"masamong/internal/clock"
	"masamong/internal/config"
)

// The daemon runs two Services at once (the real one and the console's practice instance). Each hands
// out its own event IDs, in order, without a data race between them (run with -race).
func TestEventIDsAreCountedPerService(t *testing.T) {
	a := &Service{Clock: clock.Real{}, watchers: map[chan Event]struct{}{}}
	b := &Service{Clock: clock.Real{}, watchers: map[chan Event]struct{}{}}
	ca, _ := a.Subscribe()
	cb, _ := b.Subscribe()
	const n = 30
	var wg sync.WaitGroup
	for _, s := range []*Service{a, b} {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < n; i++ {
				s.emit("config", "", nil)
			}
		}()
	}
	wg.Wait()
	for name, ch := range map[string]<-chan Event{"a": ca, "b": cb} {
		for want := int64(1); want <= n; want++ {
			if got := (<-ch).ID; got != want {
				t.Fatalf("service %s: event %d has ID %d", name, want, got)
			}
		}
	}
}

// Close drops the service's provider capability caches: the registry of them is a package variable and
// would otherwise keep every closed service (and its caches) alive.
func TestCloseForgetsTheCapabilityCaches(t *testing.T) {
	s := &Service{Clock: clock.Real{}, watchers: map[chan Event]struct{}{}}
	s.modelCaps("p", config.Provider{Kind: config.KindOpenAI, BaseURL: "http://127.0.0.1:1/v1"})
	if _, ok := capsStates.Load(s); !ok {
		t.Fatal("the cache should exist after use")
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, ok := capsStates.Load(s); ok {
		t.Fatal("Close must forget the capability caches")
	}
}
