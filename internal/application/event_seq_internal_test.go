package application

import (
	"sync"
	"testing"

	"masamong/internal/clock"
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
