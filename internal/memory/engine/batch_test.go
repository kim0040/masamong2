package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
)

// fakeBatch is a provider batch API in memory: it accepts submissions, reports the state a test sets
// and answers every item (embeddings from vecFor, summaries named after the item) unless told to drop
// some or fail.
type fakeBatch struct {
	mu        sync.Mutex
	support   map[openai.BatchKind]bool
	max       int
	subs      []*fakeSub
	state     openai.BatchState // every batch's state (default running)
	drop      map[string]bool   // item ids answered with nothing
	failItems map[string]bool   // item ids answered with an error
	partial   bool              // only the first half of each batch's items come back
	pollErr   error
	polls     atomic.Int32
	cancels   []string
	cleanups  []string
	onPoll    func() // runs on every poll (outside the lock)
}

type fakeSub struct {
	ref   openai.BatchRef
	kind  openai.BatchKind
	model string
	items []openai.BatchItem
}

func newFakeBatch(kinds ...openai.BatchKind) *fakeBatch {
	f := &fakeBatch{support: map[openai.BatchKind]bool{}, state: openai.BatchRunning, drop: map[string]bool{}, failItems: map[string]bool{}}
	for _, k := range kinds {
		f.support[k] = true
	}
	return f
}

func (f *fakeBatch) BatchSupport(kind openai.BatchKind) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.support[kind]
}

func (f *fakeBatch) BatchMaxItems(openai.BatchKind) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.max
}

func (f *fakeBatch) SubmitBatch(_ context.Context, kind openai.BatchKind, model string, items []openai.BatchItem) (openai.BatchRef, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.support[kind] {
		return openai.BatchRef{}, openai.ErrBatchUnsupported
	}
	ref := openai.BatchRef{API: "fake", BaseURL: "fake://host", ID: fmt.Sprintf("batch_%d", len(f.subs)+1), Kind: kind,
		Model: model, Count: len(items), SubmittedAt: time.Now()}
	f.subs = append(f.subs, &fakeSub{ref: ref, kind: kind, model: model, items: append([]openai.BatchItem(nil), items...)})
	return ref, nil
}

func (f *fakeBatch) sub(id string) *fakeSub {
	for _, s := range f.subs {
		if s.ref.ID == id {
			return s
		}
	}
	return nil
}

func (f *fakeBatch) PollBatch(_ context.Context, ref openai.BatchRef) (openai.BatchStatus, error) {
	f.polls.Add(1)
	f.mu.Lock()
	hook := f.onPoll
	f.mu.Unlock()
	if hook != nil {
		hook()
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.pollErr != nil {
		return openai.BatchStatus{}, f.pollErr
	}
	s := f.sub(ref.ID)
	if s == nil {
		return openai.BatchStatus{}, errors.New("no such batch")
	}
	st := openai.BatchStatus{State: f.state, Total: len(s.items)}
	if f.state.Final() {
		st.Completed = len(s.items)
	}
	return st, nil
}

func (f *fakeBatch) BatchResults(_ context.Context, ref openai.BatchRef) ([]openai.BatchOutcome, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s := f.sub(ref.ID)
	if s == nil {
		return nil, errors.New("no such batch")
	}
	items := s.items
	if f.partial {
		items = items[:len(items)/2]
	}
	var out []openai.BatchOutcome
	for _, it := range items {
		switch {
		case f.drop[it.ID]:
			continue
		case f.failItems[it.ID]:
			out = append(out, openai.BatchOutcome{ID: it.ID, Err: errors.New("item rejected")})
		case s.kind == openai.BatchEmbed:
			cost := 0.0000005
			res := &openai.EmbedResult{PromptTokens: 5, CostUSD: &cost}
			for _, in := range it.Input {
				res.Vectors = append(res.Vectors, vecFor(in))
			}
			out = append(out, openai.BatchOutcome{ID: it.ID, Embed: res})
		default:
			cost := 0.00002
			out = append(out, openai.BatchOutcome{ID: it.ID, Chat: &openai.ChatResult{Content: "배치 요약 " + it.ID, PromptTokens: 30,
				CompletionTokens: 6, CostUSD: &cost}})
		}
	}
	return out, nil
}

func (f *fakeBatch) CancelBatch(_ context.Context, ref openai.BatchRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancels = append(f.cancels, ref.ID)
	return nil
}

func (f *fakeBatch) CleanupBatch(_ context.Context, ref openai.BatchRef) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cleanups = append(f.cleanups, ref.ID)
	return nil
}

func (f *fakeBatch) set(state openai.BatchState) {
	f.mu.Lock()
	f.state = state
	f.mu.Unlock()
}

func (f *fakeBatch) submissions() []*fakeSub {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]*fakeSub(nil), f.subs...)
}

// fastBatch shortens the waits for a test.
func fastBatch(t *testing.T) {
	t.Helper()
	first, mx, cw, cp, rr := batchPollFirst, batchPollMax, batchCancelWait, batchCancelPoll, batchResultRetry
	batchPollFirst, batchPollMax, batchCancelWait, batchCancelPoll, batchResultRetry = time.Millisecond, 5*time.Millisecond, 200*time.Millisecond, time.Millisecond, time.Millisecond
	t.Cleanup(func() {
		batchPollFirst, batchPollMax, batchCancelWait, batchCancelPoll, batchResultRetry = first, mx, cw, cp, rr
	})
}

const batchRoom = "kakao:batch-room"

// seedBatchRoom stores n messages one minute apart, two days ago (closed leaves of 8).
func seedBatchRoom(t *testing.T, e *Engine, room string, n, from int) {
	t.Helper()
	base := time.Now().Add(-48 * time.Hour).Add(time.Duration(from) * time.Minute)
	var batch []corpus.Observation
	for i := 0; i < n; i++ {
		batch = append(batch, obs(room, from+i, []string{"민지", "도윤"}[i%2], fmt.Sprintf("동아리 %d번째 이야기: 다음 모임 장소와 시간 정하기", from+i),
			base.Add(time.Duration(i)*time.Minute)))
	}
	if _, err := e.Store.InsertObservations(batch, nil); err != nil {
		t.Fatal(err)
	}
}

func countLeaves(t *testing.T, e *Engine, where string) int {
	t.Helper()
	var n int
	if err := e.Store.DB.DB.QueryRow(`SELECT count(*) FROM memory_leaves WHERE room=? AND closed=1 AND `+where, batchRoom).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// runBatch drives a plan to its end the way the prepare job does.
func runBatch(t *testing.T, e *Engine, plan *BatchPlan) IndexReport {
	t.Helper()
	ctx := context.Background()
	if err := e.WaitBatch(ctx, plan, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.ApplyBatch(ctx, plan); err != nil {
		t.Fatal(err)
	}
	rep, err := e.FinishBatch(ctx, plan, nil)
	if err != nil {
		t.Fatal(err)
	}
	return rep
}

// Prepare submits one batch per group the host can batch, split at the host's item limit, stores the
// plan (the room is then held), and leaves the summary role, whose host cannot batch, live.
func TestBatchPrepareSubmitsGroupsSplitsAndLeavesUnsupportedLive(t *testing.T) {
	e, fp := newEngine(t, true)
	e.Settings.EmbedBatch = 2
	seedBatchRoom(t, e, batchRoom, 48, 0) // 6 closed leaves → 3 embedding requests of 2
	emb := newFakeBatch(openai.BatchEmbed)
	emb.max = 2
	sum := newFakeBatch() // the summary host has no batch API
	e.EmbedBatcher, e.SummaryBatcher = emb, sum
	plan, rep, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{BudgetUSD: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	subs := emb.submissions()
	if len(subs) != 2 || len(subs[0].items) != 2 || len(subs[1].items) != 1 || subs[0].kind != openai.BatchEmbed || subs[0].model != "fake-embed" {
		t.Fatalf("want 2 embed batches of 2+1 requests, got %d: %+v", len(subs), subs)
	}
	for _, s := range subs {
		for _, it := range s.items {
			if len(it.Input) == 0 || len(it.Input) > 2 || it.Chat != nil {
				t.Fatalf("embed request %q carries %d inputs", it.ID, len(it.Input))
			}
		}
	}
	if len(sum.submissions()) != 0 {
		t.Fatal("an unsupported host must not be sent a batch")
	}
	if plan.BatchItems != 6 || plan.LiveItems != 6 || rep.BatchItems != 6 || rep.LiveItems != 6 || rep.Mode != "batch" {
		t.Fatalf("items batch=%d live=%d report %+v", plan.BatchItems, plan.LiveItems, rep)
	}
	if rep.Estimate == nil || rep.Estimate.BatchUSD == nil || *rep.Estimate.BatchUSD >= rep.Estimate.TotalUSD {
		t.Fatalf("a batch estimate must be cheaper than live: %+v", rep.Estimate)
	}
	if fp.embedCalls.Load()+fp.chatCalls.Load() != 0 {
		t.Fatal("preparing a batch must not call the provider live")
	}
	stored, ok, err := e.LoadBatchPlan(batchRoom)
	if err != nil || !ok || len(stored.Groups) != 2 || stored.Groups[0].Ref.ID != subs[0].ref.ID {
		t.Fatalf("plan not stored: %v %v %+v", ok, err, stored)
	}
	held, err := e.BatchWaitingRooms()
	if err != nil || !held[batchRoom] {
		t.Fatalf("a waiting plan holds its room: %v %v", held, err)
	}
	// the stored plan has ids and hashes only, never the text
	raw, _, _ := e.Store.BatchPlan(batchRoom)
	if strings.Contains(string(raw), "동아리") {
		t.Fatal("the stored plan must not contain message text")
	}
}

// Apply saves vectors and summaries the way the live pass does and skips a leaf whose text changed
// while the batch ran.
func TestBatchApplySavesResultsAndSkipsStale(t *testing.T) {
	fastBatch(t)
	e, fp := newEngine(t, true)
	seedBatchRoom(t, e, batchRoom, 32, 0) // 4 leaves
	fb := newFakeBatch(openai.BatchEmbed, openai.BatchChat)
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || len(plan.Groups) != 2 || plan.BatchItems != 8 {
		t.Fatalf("prepare: %v %+v", err, plan)
	}
	changed := plan.Groups[0].Items[0].Targets[0].ID
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_leaves SET body=body||' (수정됨)' WHERE leaf_id=?`, changed); err != nil {
		t.Fatal(err)
	}
	fb.set(openai.BatchDone)
	if err := e.WaitBatch(context.Background(), &plan, nil); err != nil {
		t.Fatal(err)
	}
	applied, err := e.ApplyBatch(context.Background(), &plan)
	if err != nil {
		t.Fatal(err)
	}
	if applied.Saved != 6 || applied.Stale != 2 || applied.Failed != 0 || applied.SpentUSD <= 0 {
		t.Fatalf("applied %+v (want 3 leaves + 3 summaries saved, the changed leaf's two results stale)", applied)
	}
	if n := countLeaves(t, e, `leaf_vec IS NOT NULL AND embed_space='`+e.EmbedSpace()+`'`); n != 3 {
		t.Fatalf("leaf vectors saved: %d", n)
	}
	if n := countLeaves(t, e, `summary LIKE '배치 요약 %' AND summary_vec IS NULL`); n != 3 {
		t.Fatalf("summaries saved without their vector (the remainder embeds it): %d", n)
	}
	var cached int
	_ = e.Store.DB.DB.QueryRow(`SELECT count(*) FROM memory_embed_cache WHERE kind='summary_text'`).Scan(&cached)
	if cached != 3 {
		t.Fatalf("summary texts cached: %d", cached)
	}
	if fp.embedCalls.Load()+fp.chatCalls.Load() != 0 {
		t.Fatal("applying must not call the provider")
	}
	// applying the same group again (another run) changes nothing
	again, err := e.Apply(context.Background(), &plan, 0, nil)
	if err != nil || again != (BatchApplied{}) {
		t.Fatalf("second apply %+v %v", again, err)
	}
	var spent float64
	_ = e.Store.DB.DB.QueryRow(`SELECT coalesce(sum(usd),0) FROM memory_spend`).Scan(&spent)
	if spent < applied.SpentUSD-1e-12 || spent > applied.SpentUSD+1e-12 {
		t.Fatalf("spend recorded %v, outcomes cost %v", spent, applied.SpentUSD)
	}
}

// After the results are applied the live remainder pays only for what is left: the summary vectors
// and the item the batch did not answer.
func TestBatchRemainderRunsLiveForLeftoversOnly(t *testing.T) {
	fastBatch(t)
	e, fp := newEngine(t, true)
	seedBatchRoom(t, e, batchRoom, 32, 0) // 4 leaves
	fb := newFakeBatch(openai.BatchEmbed, openai.BatchChat)
	e.EmbedBatcher = fb
	var calls []CallInfo
	var mu sync.Mutex
	e.OnCall = func(_ context.Context, c CallInfo) { mu.Lock(); calls = append(calls, c); mu.Unlock() }
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{BudgetUSD: 1}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fb.drop["s0"] = true // one summary never comes back
	fb.set(openai.BatchDone)
	rep := runBatch(t, e, &plan)
	if fp.chatCalls.Load() != 1 {
		t.Fatalf("live summary calls %d, want 1 (the missing one)", fp.chatCalls.Load())
	}
	// summary vectors are embedded live, one per distinct summary; leaves are not embedded again
	if got := fp.embedCalls.Load(); got != 4 {
		t.Fatalf("live embedding calls %d, want 4 summary vectors", got)
	}
	if rep.Mode != "batch" || rep.BatchItems != 8 || rep.BatchFailed != 1 || rep.LeafVectors != 4 || rep.Summaries != 4 || rep.SummaryVectors != 4 || rep.StoppedBy != "" {
		t.Fatalf("report %+v", rep)
	}
	if n := countLeaves(t, e, `summary_vec IS NOT NULL AND leaf_vec IS NOT NULL`); n != 4 {
		t.Fatalf("leaves complete: %d of 4", n)
	}
	if _, ok, _ := e.LoadBatchPlan(batchRoom); ok {
		t.Fatal("a finished plan must be deleted")
	}
	if len(fb.cleanups) != 2 {
		t.Fatalf("cleanup of both batches: %v", fb.cleanups)
	}
	if ix := e.Index(); ix == nil || len(ix.Rooms()) == 0 || rep.Reload["leaves"] != 4 {
		t.Fatalf("the applied leaves must be loaded: %v", rep.Reload)
	}
	var batchCost float64
	mu.Lock()
	for _, c := range calls {
		batchCost += c.CostUSD
	}
	mu.Unlock()
	if rep.SpentUSD <= plan.SpentUSD || rep.SpentUSD < batchCost-1e-12 || rep.SpentUSD > batchCost+1e-12 {
		t.Fatalf("spent %v; batch %v; ledger %v", rep.SpentUSD, plan.SpentUSD, batchCost)
	}
	held, _ := e.BatchWaitingRooms()
	if held[batchRoom] {
		t.Fatal("the room must be released")
	}
}

// A restart (a new engine on the same database) finds the stored plan and polls it; nothing is
// submitted again.
func TestBatchResumeAfterRestartPollsWithoutResubmitting(t *testing.T) {
	fastBatch(t)
	e, _ := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 24, 0)
	fb := newFakeBatch(openai.BatchEmbed)
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || len(plan.Groups) != 1 {
		t.Fatalf("%v %+v", err, plan)
	}
	restarted := New(e.Store, e.LLM, e.Models, e.Settings)
	restarted.EmbedBatcher = fb
	again, rep, err := restarted.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || len(fb.submissions()) != 1 || len(again.Groups) != 1 || again.Groups[0].Ref.ID != plan.Groups[0].Ref.ID || rep.BatchItems != 3 {
		t.Fatalf("resume must reuse the stored plan: %v subs=%d %+v", err, len(fb.submissions()), again)
	}
	before := fb.polls.Load()
	fb.set(openai.BatchDone)
	out := runBatch(t, restarted, &again)
	if fb.polls.Load() == before || len(fb.submissions()) != 1 || out.LeafVectors != 3 {
		t.Fatalf("polled %d, submitted %d, report %+v", fb.polls.Load()-before, len(fb.submissions()), out)
	}
}

// A failed batch, an expired one with half its results and one cancelled at the host all end the same
// way: what came back is saved, everything else runs live.
func TestBatchFailedExpiredOrCancelledRunsTheRestLive(t *testing.T) {
	for _, tc := range []struct {
		state   openai.BatchState
		partial bool
		live    int32 // live embedding calls (EmbedBatch 1: one per leaf not answered)
	}{
		{openai.BatchFailed, false, 4},
		{openai.BatchExpired, true, 2},
		{openai.BatchCancelled, true, 2},
	} {
		t.Run(string(tc.state), func(t *testing.T) {
			fastBatch(t)
			e, fp := newEngine(t, false)
			e.Settings.EmbedBatch = 1
			seedBatchRoom(t, e, batchRoom, 32, 0)
			fb := newFakeBatch(openai.BatchEmbed)
			fb.partial = tc.partial
			e.EmbedBatcher = fb
			plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
			if err != nil || plan.BatchItems != 4 {
				t.Fatalf("%v %+v", err, plan)
			}
			fb.set(tc.state)
			rep := runBatch(t, e, &plan)
			if fp.embedCalls.Load() != tc.live || rep.LeafVectors != 4 || rep.BatchFailed != int(tc.live) || rep.StoppedBy != "" {
				t.Fatalf("live calls %d (want %d), report %+v", fp.embedCalls.Load(), tc.live, rep)
			}
			if n := countLeaves(t, e, `leaf_vec IS NOT NULL`); n != 4 {
				t.Fatalf("leaves embedded %d", n)
			}
		})
	}
}

// A host that cannot be polled any more (ErrBatchUnsupported, as after the provider changed) ends the
// wait instead of polling forever, and its items run live.
func TestBatchUnpollableBatchIsGivenUp(t *testing.T) {
	fastBatch(t)
	e, fp := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	fb := newFakeBatch(openai.BatchEmbed)
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || len(plan.Groups) != 1 {
		t.Fatal(err)
	}
	fb.pollErr = openai.ErrBatchUnsupported
	rep := runBatch(t, e, &plan)
	if rep.LeafVectors != 2 || rep.BatchFailed != 2 || fp.embedCalls.Load() == 0 {
		t.Fatalf("report %+v live calls %d", rep, fp.embedCalls.Load())
	}
}

// The submit is checked against the whole batch estimate: over budget, nothing is sent.
func TestBatchBudgetStopSubmitsNothing(t *testing.T) {
	e, fp := newEngine(t, true)
	seedBatchRoom(t, e, batchRoom, 32, 0)
	fb := newFakeBatch(openai.BatchEmbed, openai.BatchChat)
	e.EmbedBatcher = fb
	plan, rep, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{BudgetUSD: 1e-9}, nil)
	if err != nil || rep.StoppedBy != "budget" || len(plan.Groups) != 0 || len(fb.submissions()) != 0 || fp.embedCalls.Load() != 0 {
		t.Fatalf("budget stop: %v %+v subs=%d", err, rep, len(fb.submissions()))
	}
	if rep.Estimate == nil || rep.Estimate.BatchUSD == nil || *rep.Estimate.BatchUSD <= 1e-9 {
		t.Fatalf("the stop reports the batch estimate: %+v", rep.Estimate)
	}
	if _, ok, _ := e.LoadBatchPlan(batchRoom); ok {
		t.Fatal("no plan after a budget stop")
	}
	// the live remainder stops at the budget the batch already spent, and still loads what was applied
	plan = BatchPlan{Version: batchPlanVersion, Room: batchRoom, State: BatchPlanWaiting, Space: e.EmbedSpace(), EmbedModel: e.Models.Embed,
		BudgetUSD: 0.001, SpentUSD: 0.001, CreatedAt: time.Now()}
	if err := e.saveBatchPlan(&plan); err != nil {
		t.Fatal(err)
	}
	out, err := e.FinishBatch(context.Background(), &plan, nil)
	if err != nil || out.StoppedBy != "budget" || fp.embedCalls.Load() != 0 || out.SpentUSD != 0.001 {
		t.Fatalf("remainder over budget: %v %+v calls=%d", err, out, fp.embedCalls.Load())
	}
}

// The wait holds no lock: another room is indexed live while the batch is running.
func TestBatchWaitDoesNotHoldTheIndexLock(t *testing.T) {
	fastBatch(t)
	e, _ := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	seedBatchRoom(t, e, "kakao:other-room", 16, 100)
	fb := newFakeBatch(openai.BatchEmbed)
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || len(plan.Groups) != 1 {
		t.Fatal(err)
	}
	polled := make(chan struct{}, 1)
	fb.onPoll = func() {
		select {
		case polled <- struct{}{}:
		default:
		}
	}
	waited := make(chan error, 1)
	go func() { waited <- e.WaitBatch(context.Background(), &plan, nil) }()
	<-polled
	done := make(chan IndexReport, 1)
	go func() {
		rep, _ := e.IndexRooms(context.Background(), IndexOptions{Rooms: []string{"kakao:other-room"}}, nil)
		done <- rep
	}()
	select {
	case rep := <-done:
		if rep.LeafVectors != 2 {
			t.Fatalf("other room: %+v", rep)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("another room's pass waited for the batch")
	}
	select {
	case <-waited:
		t.Fatal("the wait ended while the batch was still running")
	default:
	}
	fb.set(openai.BatchDone)
	if err := <-waited; err != nil {
		t.Fatal(err)
	}
}

// A cancel from the console cancels the open batches, applies what finished and deletes the plan.
func TestBatchCancelPlanAppliesFinishedAndDeletes(t *testing.T) {
	fastBatch(t)
	e, fp := newEngine(t, true)
	seedBatchRoom(t, e, batchRoom, 32, 0)
	emb := newFakeBatch(openai.BatchEmbed)
	sum := newFakeBatch(openai.BatchChat)
	e.EmbedBatcher, e.SummaryBatcher = emb, sum
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || len(plan.Groups) != 2 {
		t.Fatalf("%v %+v", err, plan)
	}
	emb.set(openai.BatchDone) // the leaf vectors finished; the summaries are still running
	for i := 0; i < 4; i++ {
		sum.drop[fmt.Sprintf("s%d", i)] = true // and none of them will have finished
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := e.WaitBatch(ctx, &plan, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("a cancelled wait returns the context error: %v", err)
	}
	go func() { time.Sleep(20 * time.Millisecond); sum.set(openai.BatchCancelled) }() // the host takes a moment
	applied, err := e.CancelPlan(context.Background(), &plan)
	if err != nil || applied.Saved != 4 {
		t.Fatalf("cancel: %v %+v", err, applied)
	}
	if len(sum.cancels) == 0 || len(emb.cancels) != 0 {
		t.Fatalf("cancel only the open batch: emb=%v sum=%v", emb.cancels, sum.cancels)
	}
	if _, ok, _ := e.LoadBatchPlan(batchRoom); ok {
		t.Fatal("a cancelled plan must be deleted")
	}
	if fp.embedCalls.Load()+fp.chatCalls.Load() != 0 {
		t.Fatal("a cancel runs nothing live")
	}
	if n := countLeaves(t, e, `leaf_vec IS NOT NULL`); n != 4 {
		t.Fatalf("finished leaf vectors applied: %d", n)
	}
}

// Message vectors go in the batch with the leaves. A model without MRL and no leaf embedded yet has no
// message width at submit: the leaf results set it before the message results are applied.
func TestBatchMessageVectors(t *testing.T) {
	fastBatch(t)
	e, fp := newEngine(t, false)
	e.Settings.MessageVectors = true
	seedBatchRoom(t, e, batchRoom, 16, 0)
	fb := newFakeBatch(openai.BatchEmbed)
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || len(plan.Groups) != 2 || plan.Groups[1].Purpose != purposeMessage || plan.MessageSpace != "" {
		t.Fatalf("width unknown: messages batched, space decided at apply: %v %+v", err, plan)
	}
	fb.set(openai.BatchDone)
	rep := runBatch(t, e, &plan)
	if rep.MessageVectors != 16 || fp.embedCalls.Load() != 0 || e.MessageSpace() == "" {
		t.Fatalf("messages saved from the batch at the leaves' width: live=%d %+v", fp.embedCalls.Load(), rep)
	}
	// a width that cannot be settled (the leaf results never came) makes the message results stale, and
	// the remainder embeds the messages live once the leaves are in
	e2, fp2 := newEngine(t, false)
	e2.Settings.MessageVectors = true
	seedBatchRoom(t, e2, batchRoom, 16, 0)
	fb2 := newFakeBatch(openai.BatchEmbed)
	e2.EmbedBatcher = fb2
	plan2, _, err := e2.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || len(plan2.Groups) != 2 {
		t.Fatal(err)
	}
	fb2.drop["l0"] = true
	fb2.set(openai.BatchDone)
	rep2 := runBatch(t, e2, &plan2)
	if rep2.MessageVectors != 16 || rep2.LeafVectors != 2 || fp2.embedCalls.Load() == 0 {
		t.Fatalf("leaf results missing: everything runs live: %+v live=%d", rep2, fp2.embedCalls.Load())
	}
	// new messages in a room whose width is known now: both groups are batched
	seedBatchRoom(t, e, batchRoom, 16, 200)
	fb.set(openai.BatchRunning)
	live := fp.embedCalls.Load()
	plan, _, err = e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || len(plan.Groups) != 2 || plan.Groups[1].Purpose != purposeMessage || plan.MessageSpace == "" {
		t.Fatalf("messages batched: %v %+v", err, plan)
	}
	fb.set(openai.BatchDone)
	rep = runBatch(t, e, &plan)
	if fp.embedCalls.Load() != live || rep.MessageVectors != 16 || rep.LeafVectors != 2 {
		t.Fatalf("batched messages saved without live calls: live=%d report %+v", fp.embedCalls.Load()-live, rep)
	}
}

// The batch path sends the live path's requests: a Korean summary prompt goes out byte-identical, and
// the embedding inputs are the same texts in the same order.
func TestBatchRequestsMatchLiveByteForByte(t *testing.T) {
	if !strings.Contains(lang.For("ko").Memory.SummaryPrompt, "{body}") {
		t.Fatal("the Korean summary prompt must exist")
	}
	var mu sync.Mutex
	var liveChat [][]byte
	var liveInputs []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		mu.Lock()
		if r.URL.Path == "/chat/completions" {
			liveChat = append(liveChat, body)
		}
		mu.Unlock()
		if r.URL.Path == "/embeddings" {
			var req struct {
				Input []string `json:"input"`
			}
			_ = json.Unmarshal(body, &req)
			mu.Lock()
			liveInputs = append(liveInputs, req.Input...)
			mu.Unlock()
		}
		(&fakeProvider{}).handler().ServeHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	e, _ := newEngine(t, true)
	e.Settings.Workers = 1 // one request at a time: the live order is the leaf order
	cred, _ := providers.Resolve("env:FAKE_KEY")
	e.LLM = openai.New(providers.NewClient(5*time.Second, nil), srv.URL, cred)
	seedBatchRoom(t, e, batchRoom, 8, 0) // one leaf
	if e.Settings.SummaryPrompt != lang.For("ko").Memory.SummaryPrompt {
		t.Fatal("the engine must use the Korean pack's prompt by default")
	}
	fb := newFakeBatch(openai.BatchEmbed, openai.BatchChat)
	e.EmbedBatcher = fb
	if _, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	var batchChat *openai.ChatRequest
	var batchInputs []string
	for _, s := range fb.submissions() {
		for _, it := range s.items {
			if it.Chat != nil {
				batchChat = it.Chat
			}
			batchInputs = append(batchInputs, it.Input...)
		}
	}
	if batchChat == nil || !strings.Contains(batchChat.Messages[0].Content, "동아리") {
		t.Fatalf("no summary request in the batch: %+v", batchChat)
	}
	// the live pass on the same leaf
	if err := e.Store.DeleteBatchPlan(batchRoom); err != nil {
		t.Fatal(err)
	}
	if _, err := e.IndexRooms(context.Background(), IndexOptions{Rooms: []string{batchRoom}}, nil); err != nil {
		t.Fatal(err)
	}
	// the batch's request, sent the live way
	if _, err := e.LLM.Chat(context.Background(), *batchChat); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(liveChat) != 2 || !bytes.Equal(liveChat[0], liveChat[1]) {
		t.Fatalf("the batch summary request differs from the live one:\nlive  %s\nbatch %s", liveChat[0], liveChat[len(liveChat)-1])
	}
	if len(liveInputs) < len(batchInputs) || strings.Join(liveInputs[:len(batchInputs)], "\x00") != strings.Join(batchInputs, "\x00") {
		t.Fatalf("embedding inputs differ: live %d batch %d", len(liveInputs), len(batchInputs))
	}
}

// A live pass leaves a room whose batch is at the provider alone, also when it is the only room asked for
// (an empty room list means every room to the message query), and indexes the other rooms as usual.
func TestBatchHeldRoomIsLeftOutOfLivePasses(t *testing.T) {
	fastBatch(t)
	e, fp := newEngine(t, false)
	e.Settings.MessageVectors = true
	const other = "kakao:batch-other"
	// the other room indexed live first: the message width is known, so live passes embed messages
	seedBatchRoom(t, e, other, 16, 0)
	if rep, err := e.IndexRooms(context.Background(), IndexOptions{Rooms: []string{other}}, nil); err != nil || rep.MessageVectors != 16 {
		t.Fatalf("%+v %v", rep, err)
	}
	seedBatchRoom(t, e, batchRoom, 16, 0)
	fb := newFakeBatch(openai.BatchEmbed)
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || len(plan.Groups) != 2 {
		t.Fatalf("%v %+v", err, plan)
	}
	calls := fp.embedCalls.Load()
	rep, err := e.IndexRooms(context.Background(), IndexOptions{Rooms: []string{batchRoom}}, nil)
	if err != nil || len(rep.BatchWaiting) != 1 || rep.BatchWaiting[0] != batchRoom || fp.embedCalls.Load() != calls || rep.MessageVectors != 0 {
		t.Fatalf("held room alone: live calls %d→%d %+v %v", calls, fp.embedCalls.Load(), rep, err)
	}
	seedBatchRoom(t, e, other, 8, 30) // new messages in the other room
	rep, err = e.IndexRooms(context.Background(), IndexOptions{}, nil)
	if err != nil || len(rep.BatchWaiting) != 1 || rep.LeafVectors == 0 {
		t.Fatalf("all rooms: the other room is indexed: %+v %v", rep, err)
	}
	if n := countLeaves(t, e, "leaf_vec IS NULL"); n != 2 {
		t.Fatalf("held room leaves embedded live: %d pending, want 2", n)
	}
	fb.set(openai.BatchDone)
	if rep := runBatch(t, e, &plan); rep.LeafVectors != 2 || rep.MessageVectors != 16 || rep.BatchFailed != 0 {
		t.Fatalf("batch results %+v", rep)
	}
}
