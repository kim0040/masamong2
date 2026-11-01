package application

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/hostprofile"
)

func receiveZodiac[T any](t *testing.T, ch <-chan T) T {
	t.Helper()
	select {
	case v := <-ch:
		return v
	case <-time.After(20 * time.Second):
		t.Fatal("fortune worker did not reach its checkpoint")
		var zero T
		return zero
	}
}

func releaseZodiacHold(t *testing.T, ch chan struct{}) func() {
	t.Helper()
	var once sync.Once
	release := func() { once.Do(func() { close(ch) }) }
	t.Cleanup(release)
	return release
}

// Release, cancel and join before deliveryService's cleanup closes the store. The hook cleanup runs
// later too, so an early assertion failure cannot leave a worker using a restored hook or a closed DB.
func zodiacWorkers(t *testing.T, release func()) (context.Context, *sync.WaitGroup) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	workers := &sync.WaitGroup{}
	t.Cleanup(func() {
		release()
		cancel()
		done := make(chan struct{})
		go func() { workers.Wait(); close(done) }()
		receiveZodiac(t, done)
	})
	return ctx, workers
}

// The slot channel is created from the host profile, not from the medium constant alone.
func TestZodiacSlotCountFollowsHostProfile(t *testing.T) {
	prev := hostprofile.Current()
	t.Cleanup(func() {
		hostprofile.Apply(prev)
		zodiacSlotOnce = sync.Once{}
		zodiacSlots = nil
	})
	for _, n := range []int{1, 2, 4} {
		p := prev
		p.FortuneGenerations = n
		hostprofile.Apply(p)
		zodiacSlotOnce = sync.Once{}
		zodiacSlots = nil
		if cap(ensureZodiacSlots()) != n {
			t.Fatalf("generations %d: cap %d", n, cap(zodiacSlots))
		}
	}
}

// No more than zodiacMaxGenerations model calls run at once; one that has to wait for a slot gives up with
// its own context.
func TestZodiacGenerationsAreBounded(t *testing.T) {
	s, _ := deliveryService(t)
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	var running, peak atomic.Int32
	release := make(chan struct{})
	unblock := releaseZodiacHold(t, release)
	workerCtx, workers := zodiacWorkers(t, unblock)
	started := make(chan struct{}, zodiacMaxGenerations)
	gen := func(ctx context.Context) (string, error) {
		n := running.Add(1)
		started <- struct{}{}
		defer running.Add(-1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		select {
		case <-release:
			return "text", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	type result struct {
		text string
		err  error
	}
	out := make(chan result, 3)
	for _, key := range []string{"k1", "k2"} {
		workers.Add(1)
		go func() {
			defer workers.Done()
			text, _, err := s.zodiacText(workerCtx, db, key, gen)
			out <- result{text, err}
		}()
	}
	for i := 0; i < zodiacMaxGenerations; i++ {
		receiveZodiac(t, started)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, _, err := s.zodiacText(ctx, db, "k3", gen); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a third generation: err = %v", err)
	}
	unblock()
	for i := 0; i < 2; i++ {
		if r := receiveZodiac(t, out); r.err != nil || r.text != "text" {
			t.Fatalf("result %+v", r)
		}
	}
	if peak.Load() > zodiacMaxGenerations {
		t.Fatalf("%d generations ran at once", peak.Load())
	}
}

// When the request that was generating a text runs out of time, an asker waiting for it takes over and
// still gets its text instead of the first request's timeout.
func TestZodiacWaiterTakesOverFromATimedOutGenerator(t *testing.T) {
	s, _ := deliveryService(t)
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	leaderCtx, cancelLeader := context.WithCancel(context.Background())
	joined := make(chan struct{}, 1)
	setZodiacHook(t, &zodiacTestHooks.joined, func(string) {
		select {
		case joined <- struct{}{}:
		default:
		}
	})
	workerCtx, workers := zodiacWorkers(t, cancelLeader)
	var generated atomic.Int32
	workers.Add(1)
	go func() {
		defer workers.Done()
		_, _, _ = s.zodiacText(leaderCtx, db, "takeover", func(ctx context.Context) (string, error) {
			generated.Add(1)
			close(started)
			<-ctx.Done()
			return "", ctx.Err()
		})
	}()
	receiveZodiac(t, started)
	got := make(chan string, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		text, _, err := s.zodiacText(workerCtx, db, "takeover", func(context.Context) (string, error) {
			generated.Add(1)
			return "second try", nil
		})
		if err != nil {
			text = "error: " + err.Error()
		}
		got <- text
	}()
	receiveZodiac(t, joined) // the waiter is on the leader's flight: cancelling now makes it take over
	cancelLeader()
	if text := receiveZodiac(t, got); text != "second try" {
		t.Fatalf("waiter got %q", text)
	}
	if generated.Load() != 2 {
		t.Fatalf("%d generations", generated.Load())
	}
}

// setZodiacHook installs a test hook until the test ends.
func setZodiacHook(t *testing.T, h *atomic.Pointer[func(key string)], f func(key string)) {
	t.Helper()
	h.Store(&f)
	t.Cleanup(func() { h.Store(nil) })
}

// A request whose cache miss is older than another request's finished generation must read the text that
// request stored, not generate a second one (one more model call and a different text for the same key).
func TestZodiacLateRequestReadsWhatTheFlightAheadStored(t *testing.T) {
	s, _ := deliveryService(t)
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	parked, resume := make(chan struct{}), make(chan struct{})
	unblock := releaseZodiacHold(t, resume)
	var first atomic.Bool
	setZodiacHook(t, &zodiacTestHooks.afterMiss, func(string) {
		if first.CompareAndSwap(false, true) { // the late request: the one that missed first
			close(parked)
			<-resume
		}
	})
	workerCtx, workers := zodiacWorkers(t, unblock)
	var generated atomic.Int32
	gen := func(text string) func(context.Context) (string, error) {
		return func(context.Context) (string, error) { generated.Add(1); return text, nil }
	}
	type result struct {
		text   string
		cached bool
		err    error
	}
	late := make(chan result, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		text, cached, err := s.zodiacText(workerCtx, db, "late", gen("second"))
		late <- result{text, cached, err}
	}()
	receiveZodiac(t, parked)
	text, cached, err := s.zodiacText(context.Background(), db, "late", gen("first"))
	if err != nil || text != "first" || cached {
		t.Fatalf("the request ahead: %q cached=%v err=%v", text, cached, err)
	}
	unblock()
	if r := receiveZodiac(t, late); r.err != nil || r.text != "first" || !r.cached {
		t.Fatalf("the late request: %+v", r)
	}
	if n := generated.Load(); n != 1 {
		t.Fatalf("%d model calls for one key", n)
	}
}

// A generator that waited for a slot reads the cache once it has the slot: the text may have been stored
// while it waited.
func TestZodiacGeneratorReadsTheCacheAfterItsSlot(t *testing.T) {
	s, _ := deliveryService(t)
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	release := make(chan struct{})
	unblock := releaseZodiacHold(t, release)
	atSlot := make(chan struct{})
	setZodiacHook(t, &zodiacTestHooks.beforeSlot, func(key string) {
		if key == "waits" {
			close(atSlot)
		}
	})
	workerCtx, workers := zodiacWorkers(t, unblock)
	holding := make(chan struct{}, zodiacMaxGenerations)
	hold := func(ctx context.Context) (string, error) {
		holding <- struct{}{}
		select {
		case <-release:
			return "held", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	done := make(chan error, zodiacMaxGenerations)
	for i := 0; i < zodiacMaxGenerations; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			_, _, err := s.zodiacText(workerCtx, db, "holder"+string(rune('a'+i)), hold)
			done <- err
		}()
	}
	for i := 0; i < zodiacMaxGenerations; i++ {
		receiveZodiac(t, holding) // every slot is taken
	}
	var generated atomic.Int32
	got := make(chan string, 1)
	workers.Add(1)
	go func() {
		defer workers.Done()
		text, cached, err := s.zodiacText(workerCtx, db, "waits", func(context.Context) (string, error) {
			generated.Add(1)
			return "generated", nil
		})
		if err != nil || !cached {
			text = "err=" + errString(err) + " cached=" + map[bool]string{true: "true", false: "false"}[cached]
		}
		got <- text
	}()
	receiveZodiac(t, atSlot) // owns the flight, both slots are taken: it can only wait
	if _, err := db.DB.Exec(`INSERT INTO fortune_cache(cache_key, content, created_at) VALUES('waits','stored meanwhile',?)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	unblock()
	if text := receiveZodiac(t, got); text != "stored meanwhile" {
		t.Fatalf("got %q", text)
	}
	if generated.Load() != 0 {
		t.Fatal("the model was called although the text was already stored")
	}
	for i := 0; i < zodiacMaxGenerations; i++ {
		if err := receiveZodiac(t, done); err != nil {
			t.Fatal(err)
		}
	}
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}
