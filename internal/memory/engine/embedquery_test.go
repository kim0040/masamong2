package engine

import (
	"context"
	"errors"
	"net/http"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/errclass"
)

// Whoever asks for a query's embedding while it is under way waits for that call: the provider is asked
// once, and it is on the books once.
func TestConcurrentQueriesShareOneEmbedding(t *testing.T) {
	e, fp := newEngine(t, false)
	fp.embedDelay.Store(int64(200 * time.Millisecond)) // under HedgeAfter: one call means one, not a hedge
	var recorded atomic.Int32
	e.OnCall = func(_ context.Context, c CallInfo) {
		if c.Role == "embed" {
			recorded.Add(1)
		}
	}
	const callers = 5
	got := make([][]float32, callers)
	var wg sync.WaitGroup
	for i := range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			v, err := e.EmbedQuery(context.Background(), "캠핑 어디로 가")
			if err != nil {
				t.Error(err)
			}
			got[i] = v
		}()
	}
	wg.Wait()
	if n := fp.embedCalls.Load(); n != 1 {
		t.Fatalf("%d embedding calls", n)
	}
	if n := recorded.Load(); n != 1 {
		t.Fatalf("%d calls on the ledger", n)
	}
	for _, v := range got[1:] {
		if !slices.Equal(v, got[0]) {
			t.Fatal("callers got different vectors")
		}
	}
	got[0][0] = 42 // a caller's copy is its own
	again, err := e.EmbedQuery(context.Background(), "캠핑 어디로 가")
	if err != nil || again[0] == 42 {
		t.Fatalf("cached vector changed through a caller's copy: %v %v", again, err)
	}
}

// indexed is an engine with a loaded index, where a search and so a warmed query can run.
func indexed(t *testing.T) (*Engine, *fakeProvider) {
	t.Helper()
	e, fp := newEngine(t, false)
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	if e.Index() == nil {
		t.Fatal("no index")
	}
	return e, fp
}

// A warmed query is in memory only until a search asks for it: a message nobody searched with leaves no
// vector in the database. The search that follows pays nothing and keeps it.
func TestWarmQueryReachesTheDatabaseOnlyWhenSearched(t *testing.T) {
	e, fp := indexed(t)
	const q = "처음 보는 질문"
	before := fp.embedCalls.Load()
	e.WarmQuery(context.Background(), q)
	if got := fp.embedCalls.Load() - before; got != 1 {
		t.Fatalf("%d embedding calls", got)
	}
	key := e.queryKey(q)
	if _, ok := e.Store.CachedVector(key); ok {
		t.Fatal("a warmed query reached the database")
	}
	if _, err := e.EmbedQuery(context.Background(), q); err != nil {
		t.Fatal(err)
	}
	if got := fp.embedCalls.Load() - before; got != 1 {
		t.Fatalf("the search embedded again: %d calls", got)
	}
	if _, ok := e.Store.CachedVector(key); !ok { // the same lookup finds it now: it was absent above for a reason
		t.Fatal("the searched query did not reach the database")
	}
}

// A warmed embedding given up (the plan searches nothing) stops, and costs nothing beyond what it asked.
func TestGivingUpAWarmedQueryStopsIt(t *testing.T) {
	e, fp := indexed(t)
	fp.embedDelay.Store(int64(300 * time.Millisecond))
	var classes []errclass.Class
	var mu sync.Mutex
	e.OnCall = func(_ context.Context, c CallInfo) {
		if c.Role == "embed" {
			mu.Lock()
			classes = append(classes, c.Class)
			mu.Unlock()
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	started := time.Now()
	e.WarmQuery(ctx, "취소될 질문")
	if took := time.Since(started); took >= 300*time.Millisecond {
		t.Fatalf("giving up took %v", took)
	}
	if _, ok := e.qcache[e.queryKey("취소될 질문")]; ok {
		t.Fatal("a canceled embedding left a vector")
	}
	mu.Lock()
	defer mu.Unlock()
	if len(classes) != 1 || classes[0] != errclass.Canceled {
		t.Fatalf("the ledger has %v, want the one canceled call", classes)
	}
}

// A caller waiting for an embedding whose own caller gave it up does not fail with it: it asks itself.
func TestWaitingCallerAsksWhenTheCallItJoinedIsGivenUp(t *testing.T) {
	e, fp := indexed(t)
	before := fp.embedCalls.Load()
	fp.embedDelay.Store(int64(300 * time.Millisecond))
	warmCtx, giveUp := context.WithCancel(context.Background())
	defer giveUp()
	warmed := make(chan struct{})
	go func() { e.WarmQuery(warmCtx, "같은 질문"); close(warmed) }()
	for fp.embedCalls.Load() == before {
		time.Sleep(time.Millisecond)
	}
	time.AfterFunc(50*time.Millisecond, giveUp)
	v, err := e.EmbedQuery(context.Background(), "같은 질문")
	<-warmed
	if err != nil || len(v) == 0 {
		t.Fatalf("%v %v", v, err)
	}
	if got := fp.embedCalls.Load() - before; got != 2 {
		t.Fatalf("%d embedding calls, want the given-up one and its own", got)
	}
}

// A waiting caller's own context ends its wait; the call it waited for goes on for the others.
func TestWaitingCallerFollowsItsOwnContext(t *testing.T) {
	e, fp := indexed(t)
	before := fp.embedCalls.Load()
	fp.embedDelay.Store(int64(300 * time.Millisecond))
	first := make(chan error, 1)
	go func() {
		_, err := e.EmbedQuery(context.Background(), "기다리는 질문")
		first <- err
	}()
	for fp.embedCalls.Load() == before {
		time.Sleep(time.Millisecond)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := e.EmbedQuery(ctx, "기다리는 질문"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the waiter got %v", err)
	}
	if err := <-first; err != nil {
		t.Fatalf("the call it waited for failed: %v", err)
	}
}

// A failed warmed embedding is forgotten: the search asks again and works.
func TestFailedWarmQueryIsIgnored(t *testing.T) {
	e, fp := indexed(t)
	fp.embedStatus.Store(http.StatusServiceUnavailable)
	e.WarmQuery(context.Background(), "실패할 질문")
	fp.embedStatus.Store(0)
	if v, err := e.EmbedQuery(context.Background(), "실패할 질문"); err != nil || len(v) == 0 {
		t.Fatalf("%v %v", v, err)
	}
}

// No index, no search: a query is not sent out for nothing.
func TestWarmQueryWaitsForAnIndex(t *testing.T) {
	e, fp := newEngine(t, false)
	e.WarmQuery(context.Background(), "색인이 없을 때")
	if n := fp.embedCalls.Load(); n != 0 {
		t.Fatalf("%d embedding calls", n)
	}
}
