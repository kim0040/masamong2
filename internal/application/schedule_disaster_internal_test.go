package application

import (
	"context"
	"runtime"
	"testing"
	"time"
)

// The disaster tick's "one at a time" mark belongs to the service, like the other scheduler passes:
// a running tick on one service never blocks the other's, a second claim while the flag is held is
// refused, and a worker admission refused by a closing service leaves no flag behind.
func TestDisasterTickFlightIsPerService(t *testing.T) {
	a, b := twoServices(t)
	a.runs.disasterTick.Store(true)
	defer a.runs.disasterTick.Store(false)
	if !b.startDisasterTick(context.Background()) {
		t.Fatal("one service's running disaster tick blocked the other's")
	}
	// the tick ends and its flag resets before the next claim
	for deadline := time.Now().Add(5 * time.Second); b.runs.disasterTick.Load(); {
		if time.Now().After(deadline) {
			t.Fatal("the disaster tick's flag was not reset when it ended")
		}
		runtime.Gosched()
	}
	if a.startDisasterTick(context.Background()) {
		t.Fatal("a second disaster tick ran beside a claimed flag")
	}
	b.lifeMu.Lock()
	b.closing = true
	b.lifeMu.Unlock()
	if b.startDisasterTick(context.Background()) {
		t.Fatal("a closing service ran a disaster tick")
	}
	if b.runs.disasterTick.Load() {
		t.Fatal("a refused worker admission left the disaster flag set")
	}
}
