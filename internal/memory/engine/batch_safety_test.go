package engine

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"masamong/internal/providers"
	"masamong/internal/providers/openai"
)

// shapeFake is a batch host that knows its models' chat shapes (an *openai.Adapter does).
type shapeFake struct {
	*fakeBatch
	learned  map[string]bool
	refusals []openai.ChatRequest
}

func (s *shapeFake) ShapeLearned(model string) bool { return s.learned[model] }

func (s *shapeFake) LearnRefusal(req openai.ChatRequest, _ error) bool {
	s.refusals = append(s.refusals, req)
	return true
}

func spendTotal(t *testing.T, e *Engine) (usd float64, calls int) {
	t.Helper()
	if err := e.Store.DB.DB.QueryRow(`SELECT coalesce(sum(usd),0), coalesce(sum(calls),0) FROM memory_spend`).Scan(&usd, &calls); err != nil {
		t.Fatal(err)
	}
	return usd, calls
}

// Finding 1: the submission intent is stored before the create call, so a process that stops during the
// call finds it at restart instead of sending the batch again.
func TestBatchSubmitIntentIsStoredBeforeTheCall(t *testing.T) {
	e, _ := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	fb := newFakeBatch(openai.BatchEmbed)
	e.EmbedBatcher = fb
	var during BatchPlan
	var found bool
	fb.submitErr = func(int) error {
		during, found, _ = e.LoadBatchPlan(batchRoom)
		return nil
	}
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || len(plan.Groups) != 1 {
		t.Fatalf("%v %+v", err, plan)
	}
	if !found || len(during.Groups) != 1 || during.Groups[0].Submit != submitPending || during.Groups[0].Ref.SubmitKey == "" ||
		during.Generation != plan.Generation {
		t.Fatalf("the intent must be stored before the call: %v %+v", found, during)
	}
	if plan.Groups[0].Submit != "" || plan.Groups[0].Ref.SubmitKey != during.Groups[0].Ref.SubmitKey {
		t.Fatalf("after the answer the group is submitted under the same key: %+v", plan.Groups[0])
	}
}

// Finding 1: a create call whose answer was lost is looked up at the host, not sent again, and never run
// live; found, the batch is collected as usual.
func TestBatchLostSubmitAnswerIsFoundNotResent(t *testing.T) {
	fastBatch(t)
	e, fp := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	fb := newFakeBatch(openai.BatchEmbed)
	fb.lost[0], fb.listed = true, true
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || len(plan.Groups) != 1 || plan.Groups[0].Submit != submitUnknown || plan.Groups[0].Ref.ID != "" {
		t.Fatalf("the lost answer leaves the group unknown: %v %+v", err, plan)
	}
	if held, _ := e.BatchWaitingRooms(); !held[batchRoom] {
		t.Fatal("an unknown submission holds the room")
	}
	// a restart finds the same plan; the wait settles the submission first
	restarted := New(e.Store, e.LLM, e.Models, e.Settings)
	restarted.EmbedBatcher = fb
	again, _, err := restarted.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || again.Generation != plan.Generation {
		t.Fatalf("resume: %v %+v", err, again)
	}
	fb.set(openai.BatchDone)
	rep := runBatch(t, restarted, &again)
	if fb.calls != 1 || fb.reconciles != 1 || fp.embedCalls.Load() != 0 || rep.LeafVectors != 2 || rep.BatchItems != 2 {
		t.Fatalf("create calls %d, lookups %d, live %d, report %+v", fb.calls, fb.reconciles, fp.embedCalls.Load(), rep)
	}
}

// Finding 1: a lost answer the host's list shows was never taken drops the group without live calls; the
// other group, submitted meanwhile, is collected and the job is told to prepare again.
func TestBatchLostSubmitNeverTakenIsDroppedNotRunLive(t *testing.T) {
	fastBatch(t)
	e, fp := newEngine(t, true)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	fb := newFakeBatch(openai.BatchEmbed, openai.BatchChat)
	fb.lost[0], fb.notTaken[0], fb.listed = true, true, true
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || len(plan.Groups) != 2 || plan.Groups[0].Submit != submitUnknown || plan.Groups[1].Ref.ID == "" {
		t.Fatalf("the summaries are still submitted: %v %+v", err, plan.Groups)
	}
	err = e.WaitBatch(context.Background(), &plan, nil)
	if !errors.Is(err, ErrBatchSubmitRefused) || len(plan.Groups) != 1 || plan.State != BatchPlanDraining {
		t.Fatalf("dropped, the rest drains: %v %+v", err, plan)
	}
	fb.set(openai.BatchDone)
	if _, err := e.DrainBatch(context.Background(), &plan); err != nil {
		t.Fatal(err)
	}
	if fp.embedCalls.Load()+fp.chatCalls.Load() != 0 || countLeaves(t, e, `summary LIKE '배치 요약 %'`) != 2 || fb.calls != 2 {
		t.Fatalf("live %d, create calls %d", fp.embedCalls.Load()+fp.chatCalls.Load(), fb.calls)
	}
	if _, ok, _ := e.LoadBatchPlan(batchRoom); ok {
		t.Fatal("drained plan deleted")
	}
}

// Finding 1: a host that cannot say whether it took the batch leaves the plan submit_unknown with an
// operator message; the room stays held, nothing runs live, and only a discard releases it.
func TestBatchUnknownSubmitHoldsTheRoomUntilDiscarded(t *testing.T) {
	fastBatch(t)
	e, fp := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	fb := newFakeBatch(openai.BatchEmbed)
	fb.lost[0] = true // and listed false: the host keeps no list
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	err = e.WaitBatch(context.Background(), &plan, nil)
	if !errors.Is(err, ErrBatchSubmitUnknown) || !strings.Contains(err.Error(), "discard_batch") {
		t.Fatalf("want an operator-facing unknown error, got %v", err)
	}
	stored, ok, _ := e.LoadBatchPlan(batchRoom)
	if !ok || stored.State != BatchPlanSubmitUnknown || stored.Reason == "" {
		t.Fatalf("stored %v %+v", ok, stored)
	}
	if _, err := e.FinishBatch(context.Background(), &stored, nil); !errors.Is(err, ErrBatchSubmitUnknown) {
		t.Fatalf("an unknown plan is never finished live: %v", err)
	}
	if rep, _ := e.IndexRooms(context.Background(), IndexOptions{Rooms: []string{batchRoom}}, nil); rep.LeafVectors != 0 || fp.embedCalls.Load() != 0 {
		t.Fatalf("a live pass leaves the held room alone: %+v", rep)
	}
	if fb.calls != 1 {
		t.Fatalf("sent again: %d", fb.calls)
	}
	if err := e.DiscardBatch(context.Background(), batchRoom); err != nil {
		t.Fatal(err)
	}
	if held, _ := e.BatchWaitingRooms(); held[batchRoom] {
		t.Fatal("a discard releases the room")
	}
}

// Finding D: only ErrBatchUnsupported sends work live. A refusal (a rate limit, a server's 4xx, the
// daily gate) stops the preparation without live calls: with nothing sent there is no plan; with an
// earlier batch sent, the plan drains (collected, nothing live).
func TestBatchRefusedSubmitNeverRunsLive(t *testing.T) {
	refusal := &openai.APIError{Status: 429, Message: "rate limited"}
	t.Run("nothing sent", func(t *testing.T) {
		e, fp := newEngine(t, true)
		seedBatchRoom(t, e, batchRoom, 16, 0)
		fb := newFakeBatch(openai.BatchEmbed, openai.BatchChat)
		fb.submitErr = func(int) error { return refusal }
		e.EmbedBatcher = fb
		plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
		if !errors.Is(err, ErrBatchSubmitRefused) || len(plan.Groups) != 0 || fp.embedCalls.Load()+fp.chatCalls.Load() != 0 {
			t.Fatalf("%v %+v live=%d", err, plan, fp.embedCalls.Load()+fp.chatCalls.Load())
		}
		if _, ok, _ := e.LoadBatchPlan(batchRoom); ok {
			t.Fatal("no plan holds the room after a refusal with nothing sent")
		}
	})
	t.Run("after a batch was sent", func(t *testing.T) {
		fastBatch(t)
		e, fp := newEngine(t, true)
		seedBatchRoom(t, e, batchRoom, 16, 0)
		fb := newFakeBatch(openai.BatchEmbed, openai.BatchChat)
		fb.submitErr = func(n int) error {
			if n == 1 {
				return refusal // the summary batch
			}
			return nil
		}
		e.EmbedBatcher = fb
		plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
		if !errors.Is(err, ErrBatchSubmitRefused) || len(plan.Groups) != 1 || plan.State != BatchPlanDraining || plan.LiveItems != 0 {
			t.Fatalf("%v %+v", err, plan)
		}
		if _, err := e.FinishBatch(context.Background(), &plan, nil); !errors.Is(err, ErrBatchDraining) {
			t.Fatalf("a draining plan is not finished live: %v", err)
		}
		fb.set(openai.BatchDone)
		if _, err := e.DrainBatch(context.Background(), &plan); err != nil {
			t.Fatal(err)
		}
		if fp.embedCalls.Load()+fp.chatCalls.Load() != 0 || countLeaves(t, e, `leaf_vec IS NOT NULL`) != 2 || countLeaves(t, e, `summary != ''`) != 0 {
			t.Fatal("the sent batch is collected; the refused summaries are not run live")
		}
		if _, ok, _ := e.LoadBatchPlan(batchRoom); ok {
			t.Fatal("a drained plan is deleted")
		}
	})
	t.Run("a model that cannot batch runs live", func(t *testing.T) {
		fastBatch(t)
		e, fp := newEngine(t, true)
		seedBatchRoom(t, e, batchRoom, 16, 0)
		fb := newFakeBatch(openai.BatchEmbed, openai.BatchChat)
		fb.submitErr = func(n int) error {
			if n == 1 {
				return errors.Join(openai.ErrBatchUnsupported, errors.New("model has no batch variant"))
			}
			return nil
		}
		e.EmbedBatcher = fb
		plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
		if err != nil || len(plan.Groups) != 1 || plan.LiveItems != 2 || plan.State != BatchPlanWaiting {
			t.Fatalf("%v %+v", err, plan)
		}
		fb.set(openai.BatchDone)
		rep := runBatch(t, e, &plan)
		if fp.chatCalls.Load() != 2 || rep.Summaries != 2 {
			t.Fatalf("the unsupported summaries run live: chat=%d %+v", fp.chatCalls.Load(), rep)
		}
	})
}

// Finding 2: a late Apply, Finish or cleanup of a replaced plan (another generation of the room) changes
// nothing of the newer plan, and a plan whose group is not the stored one is refused.
func TestBatchLateCallsOnAReplacedPlanChangeNothing(t *testing.T) {
	fastBatch(t)
	e, _ := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	fb := newFakeBatch(openai.BatchEmbed)
	e.EmbedBatcher = fb
	old, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fb.set(openai.BatchDone)
	if err := e.WaitBatch(context.Background(), &old, nil); err != nil {
		t.Fatal(err)
	}
	stale := old // a run still holding the old plan
	if err := e.Store.DeleteBatchPlan(batchRoom); err != nil {
		t.Fatal(err)
	}
	fb.set(openai.BatchRunning)
	newer, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || newer.Generation == stale.Generation || len(newer.Groups) != 1 {
		t.Fatalf("%v %+v", err, newer)
	}
	outcomes, _ := fb.BatchResults(context.Background(), stale.Groups[0].Ref)
	if _, err := e.Apply(context.Background(), &stale, 0, outcomes); !errors.Is(err, ErrBatchPlanReplaced) {
		t.Fatalf("a late apply: %v", err)
	}
	if n := countLeaves(t, e, `leaf_vec IS NOT NULL`); n != 0 {
		t.Fatalf("a late apply saved %d vectors", n)
	}
	cleanups := len(fb.cleanups)
	if _, err := e.FinishBatch(context.Background(), &stale, nil); !errors.Is(err, ErrBatchPlanGone) {
		t.Fatalf("a late finish: %v", err)
	}
	if err := e.finishDrain(context.Background(), &stale, BatchApplied{}); err != nil {
		t.Fatal(err)
	}
	stored, ok, _ := e.LoadBatchPlan(batchRoom)
	if !ok || stored.Generation != newer.Generation || len(fb.cleanups) != cleanups {
		t.Fatalf("the newer plan stays untouched: %v %+v cleanups=%v", ok, stored, fb.cleanups)
	}
	// the stored generation with a group that is not the stored group
	forged := newer
	forged.Groups = append([]BatchGroup(nil), newer.Groups...)
	forged.Groups[0].Ref.ID = stale.Groups[0].Ref.ID
	forged.Groups[0].State = openai.BatchDone
	if _, err := e.Apply(context.Background(), &forged, 0, outcomes); !errors.Is(err, ErrBatchPlanReplaced) {
		t.Fatalf("another group's results: %v", err)
	}
	if n := countLeaves(t, e, `leaf_vec IS NOT NULL`); n != 0 {
		t.Fatal("another group's results were applied")
	}
}

// Finding 3: a cancel keeps the plan draining and the room held while a batch is still open at a host
// that cannot cancel (OpenRouter); the drain collects it to its end and applies its results, live nothing.
func TestBatchCancelDrainsUntilEveryBatchEnds(t *testing.T) {
	fastBatch(t)
	e, fp := newEngine(t, true)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	emb := newFakeBatch(openai.BatchEmbed)
	sum := newFakeBatch(openai.BatchChat) // ignores the cancel: its batch keeps running
	e.EmbedBatcher, e.SummaryBatcher = emb, sum
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || len(plan.Groups) != 2 {
		t.Fatalf("%v %+v", err, plan)
	}
	emb.set(openai.BatchDone)
	applied, done, err := e.CancelPlan(context.Background(), &plan)
	if err != nil || done || applied.Saved != 2 {
		t.Fatalf("cancel with a batch still running: done=%v %+v %v", done, applied, err)
	}
	stored, ok, _ := e.LoadBatchPlan(batchRoom)
	if !ok || stored.State != BatchPlanDraining || !stored.Drain {
		t.Fatalf("the plan drains: %v %+v", ok, stored)
	}
	if held, _ := e.BatchWaitingRooms(); !held[batchRoom] {
		t.Fatal("a draining plan holds the room")
	}
	if len(sum.cleanups) != 0 {
		t.Fatal("results not read yet must not be cleaned up")
	}
	drained := make(chan error, 1)
	go func() { _, err := e.DrainBatch(context.Background(), &stored); drained <- err }()
	time.Sleep(20 * time.Millisecond)
	sum.set(openai.BatchDone)
	if err := <-drained; err != nil {
		t.Fatal(err)
	}
	if n := countLeaves(t, e, `summary LIKE '배치 요약 %'`); n != 2 {
		t.Fatalf("the drained summaries are applied: %d", n)
	}
	if _, ok, _ := e.LoadBatchPlan(batchRoom); ok || fp.embedCalls.Load()+fp.chatCalls.Load() != 0 {
		t.Fatalf("drained plan deleted (%v), live calls %d", ok, fp.embedCalls.Load()+fp.chatCalls.Load())
	}
	if len(sum.cleanups) != 1 || len(emb.cleanups) != 1 {
		t.Fatalf("both cleaned up once read: %v %v", emb.cleanups, sum.cleanups)
	}
}

// Finding 4 and R7: an apply that crashes after its results are written but before its checkpoint is
// committed leaves nothing (results, counters, receipt and cost go in one transaction); run again, it saves
// the vectors, counts them in the plan and its cost once, and the finish loads them into an index that was
// already loaded.
func TestBatchReapplyAfterACrashCountsTheCostOnce(t *testing.T) {
	fastBatch(t)
	e, _ := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	if _, err := e.Reload(); err != nil || e.Index() == nil {
		t.Fatalf("an index is loaded before the batch: %v", err)
	}
	fb := newFakeBatch(openai.BatchEmbed)
	e.EmbedBatcher = fb
	var calls int
	e.OnCall = func(context.Context, CallInfo) { calls++ }
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fb.set(openai.BatchDone)
	if err := e.WaitBatch(context.Background(), &plan, nil); err != nil {
		t.Fatal(err)
	}
	crash := errors.New("crash before the checkpoint")
	beforeApplyCommit = func() error { return crash }
	_, err = e.ApplyBatch(context.Background(), &plan)
	beforeApplyCommit = nil
	if !errors.Is(err, crash) {
		t.Fatalf("%v", err)
	}
	if usd, n := spendTotal(t, e); usd != 0 || n != 0 || calls != 0 {
		t.Fatalf("nothing is counted before the checkpoint: %v %d calls=%d", usd, n, calls)
	}
	if n := countLeaves(t, e, `leaf_vec IS NOT NULL`); n != 0 {
		t.Fatalf("the crashed apply's vectors are rolled back with it: %d", n)
	}
	stored, _, _ := e.LoadBatchPlan(batchRoom)
	if stored.Groups[0].Applied || stored.LeafVectors != 0 {
		t.Fatalf("no checkpoint: %+v", stored)
	}
	mergePolled(&stored, &plan)
	again, err := e.ApplyBatch(context.Background(), &stored)
	if err != nil {
		t.Fatal(err)
	}
	usd, n := spendTotal(t, e)
	if n != 1 || calls != 1 || usd <= 0 || usd != again.SpentUSD || stored.SpentUSD != usd {
		t.Fatalf("one embedding request counted once: usd=%v calls=%d log=%d applied=%+v plan=%v", usd, n, calls, again, stored.SpentUSD)
	}
	if again.Saved != 2 || again.Stale != 0 || stored.LeafVectors != 2 || stored.BatchStale != 0 || countLeaves(t, e, `leaf_vec IS NOT NULL`) != 2 {
		t.Fatalf("the retry saves and counts both vectors: applied %+v, plan vectors %d stale %d, stored %d",
			again, stored.LeafVectors, stored.BatchStale, countLeaves(t, e, `leaf_vec IS NOT NULL`))
	}
	if durable, _, _ := e.LoadBatchPlan(batchRoom); durable.LeafVectors != 2 || !durable.Groups[0].Applied {
		t.Fatalf("the counts are stored with the checkpoint: %+v", durable)
	}
	if _, err := e.Apply(context.Background(), &stored, 0, nil); err != nil {
		t.Fatal(err)
	}
	if usd2, n2 := spendTotal(t, e); usd2 != usd || n2 != n || calls != 1 {
		t.Fatal("an applied group is not counted again")
	}
	rep, err := e.FinishBatch(context.Background(), &stored, nil)
	if err != nil || rep.LeafVectors != 2 || rep.Reload["leaves"] != 2 {
		t.Fatalf("the finish reports and loads the saved vectors: %v %+v", err, rep)
	}
}

// Finding 5: results that cannot be read for a moment are read again later (the plan stays, nothing runs
// live, nothing is cleaned up); a permanent failure ends the run with the plan and the results kept.
func TestBatchResultsReadAgainAfterATransientFailure(t *testing.T) {
	t.Run("transient", func(t *testing.T) {
		fastBatch(t)
		e, fp := newEngine(t, false)
		seedBatchRoom(t, e, batchRoom, 16, 0)
		fb := newFakeBatch(openai.BatchEmbed)
		e.EmbedBatcher = fb
		plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		fails := 2
		fb.resultsErr = func() error {
			if fails > 0 {
				fails--
				return &providers.Error{Kind: "connection", Err: errors.New("reset")}
			}
			return nil
		}
		fb.set(openai.BatchDone)
		if err := e.WaitBatch(context.Background(), &plan, nil); err != nil {
			t.Fatal(err)
		}
		applied, err := e.CollectBatch(context.Background(), &plan)
		if err != nil || applied.Saved != 2 || applied.Failed != 0 || fb.results != 3 || fp.embedCalls.Load() != 0 {
			t.Fatalf("read on the third try: %v %+v reads=%d live=%d", err, applied, fb.results, fp.embedCalls.Load())
		}
	})
	t.Run("permanent", func(t *testing.T) {
		fastBatch(t)
		e, fp := newEngine(t, false)
		seedBatchRoom(t, e, batchRoom, 16, 0)
		fb := newFakeBatch(openai.BatchEmbed)
		e.EmbedBatcher = fb
		plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		fb.resultsErr = func() error { return &openai.APIError{Status: 403, Message: "forbidden"} }
		fb.set(openai.BatchDone)
		if err := e.WaitBatch(context.Background(), &plan, nil); err != nil {
			t.Fatal(err)
		}
		_, err = e.CollectBatch(context.Background(), &plan)
		if !errors.Is(err, ErrBatchResultsUnreadable) || fb.results != 1 {
			t.Fatalf("a refused read ends at once: %v reads=%d", err, fb.results)
		}
		if _, err := e.FinishBatch(context.Background(), &plan, nil); err == nil {
			t.Fatal("a plan with unread results is not finished")
		}
		stored, ok, _ := e.LoadBatchPlan(batchRoom)
		if !ok || stored.Groups[0].Applied || len(fb.cleanups) != 0 || fp.embedCalls.Load() != 0 {
			t.Fatalf("plan and remote results kept, nothing live: %v %+v cleanups=%v", ok, stored.Groups, fb.cleanups)
		}
	})
}

// Finding 6: a batch is polled and read at the provider it was submitted to, whatever the role uses now;
// when that provider is gone the wait stops at once with a clear error and nothing runs live.
func TestBatchPollsTheProviderItWasSubmittedTo(t *testing.T) {
	fastBatch(t)
	e, fp := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	first := newFakeBatch(openai.BatchEmbed)
	e.EmbedBatcher = first
	e.CallProviders.Embed = "first"
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || plan.Groups[0].Provider.ID != "first" {
		t.Fatalf("%v %+v", err, plan.Groups)
	}
	raw, _, _ := e.Store.BatchPlan(batchRoom)
	if !strings.Contains(string(raw), `"provider":{"id":"first"`) {
		t.Fatalf("the provider identity is stored: %s", raw)
	}
	// the embed role moves to another provider (the role's own adapter, no injected client)
	e.EmbedBatcher = nil
	e.CallProviders.Embed = "second"
	before := fp.embedCalls.Load()
	err = e.WaitBatch(context.Background(), &plan, nil)
	if !errors.Is(err, ErrBatchProviderGone) || !strings.Contains(err.Error(), `"first"`) || first.polls.Load() != 0 {
		t.Fatalf("without a resolver the moved role is refused: %v (first polled %d)", err, first.polls.Load())
	}
	// the application resolves the submitting provider by id
	var asked []BatchProvider
	e.ResolveBatcher = func(p BatchProvider) (BatchClient, error) {
		asked = append(asked, p)
		if p.ID == "first" {
			return first, nil
		}
		return nil, errors.New("not configured")
	}
	first.set(openai.BatchDone)
	rep := runBatch(t, e, &plan)
	if rep.LeafVectors != 2 || first.polls.Load() == 0 || first.results != 1 || len(first.cleanups) != 1 || len(asked) == 0 || asked[0].ID != "first" {
		t.Fatalf("the first provider serves its batch: %+v polls=%d asked=%v", rep, first.polls.Load(), asked)
	}
	if fp.embedCalls.Load() != before {
		t.Fatal("nothing was embedded live")
	}
	// a provider removed from the config: a clear stop, nothing live
	seedBatchRoom(t, e, batchRoom, 16, 100)
	e.ResolveBatcher = nil
	e.EmbedBatcher, e.CallProviders.Embed = first, "first"
	first.set(openai.BatchRunning)
	plan, _, err = e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	e.EmbedBatcher = nil
	e.ResolveBatcher = func(BatchProvider) (BatchClient, error) { return nil, errors.New("provider removed") }
	live := fp.embedCalls.Load()
	start := time.Now()
	if err := e.WaitBatch(context.Background(), &plan, nil); !errors.Is(err, ErrBatchProviderGone) || time.Since(start) > time.Second {
		t.Fatalf("a removed provider stops the wait at once: %v", err)
	}
	if fp.embedCalls.Load() != live {
		t.Fatal("nothing runs live")
	}
}

// Finding 7: the live remainder of a limited preparation stays within the leaves the preparation chose;
// it does not move on to the next leaves at the live price.
func TestBatchRemainderStaysInTheWorkset(t *testing.T) {
	fastBatch(t)
	e, fp := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 32, 0) // 4 leaves
	fb := newFakeBatch(openai.BatchEmbed)
	e.EmbedBatcher = fb
	e.Settings.EmbedBatch = 1
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{Limit: 2}, nil)
	if err != nil || plan.BatchItems != 2 || plan.Workset == nil || len(plan.Workset.Leaves) != 2 {
		t.Fatalf("%v %+v", err, plan)
	}
	fb.drop["l1"] = true // one of the two comes back without a result
	fb.set(openai.BatchDone)
	rep := runBatch(t, e, &plan)
	if fp.embedCalls.Load() != 1 || rep.LeafVectors != 2 {
		t.Fatalf("live: only the workset's missing leaf (%d calls), report %+v", fp.embedCalls.Load(), rep)
	}
	if n := countLeaves(t, e, `leaf_vec IS NULL`); n != 2 {
		t.Fatalf("leaves outside the workset stay for the next preparation: %d", n)
	}
}

// Finding 8: the estimate uses the host's discount for the model, not a constant half.
func TestBatchEstimateUsesTheHostDiscountPerModel(t *testing.T) {
	e, _ := newEngine(t, true)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	fb := newFakeBatch(openai.BatchEmbed, openai.BatchChat)
	fb.discount = 1 // a model the host bills at the live price in a batch
	e.EmbedBatcher = fb
	_, rep, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || rep.Estimate == nil || rep.Estimate.BatchUSD == nil {
		t.Fatalf("%v %+v", err, rep.Estimate)
	}
	if got, want := *rep.Estimate.BatchUSD, rep.Estimate.TotalUSD; got < want-1e-12 || got > want+1e-12 {
		t.Fatalf("no discount for the model: batch %v, live %v", got, want)
	}
}

// Finding 9: a workset no host can batch is the normal live pass with its usual budget behaviour, not a
// budget stop against an all-at-once batch estimate.
func TestBatchUnbatchableWorksetRunsLiveWithTheUsualBudget(t *testing.T) {
	e, fp := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	e.EmbedBatcher = newFakeBatch() // no batch API at all
	plan, rep, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{BudgetUSD: 1e-9}, nil)
	if err != nil || len(plan.Groups) != 0 || rep.StoppedBy != "" || plan.LiveItems != 2 || fp.embedCalls.Load() != 0 {
		t.Fatalf("hand it to the live pass: %v %+v %+v", err, plan, rep)
	}
	if _, ok, _ := e.LoadBatchPlan(batchRoom); ok {
		t.Fatal("no plan")
	}
}

// Finding A: a summary batch for a model whose request shape is not learned yet is preceded by one live
// summary through the normal shaped path (saved, counted live); the batch carries the rest. When every
// batch item is refused for its fields, the refusal is learned.
func TestBatchSummaryProbeLearnsTheShapeFirst(t *testing.T) {
	fastBatch(t)
	e, fp := newEngine(t, true)
	seedBatchRoom(t, e, batchRoom, 32, 0) // 4 leaves
	sum := &shapeFake{fakeBatch: newFakeBatch(openai.BatchChat), learned: map[string]bool{}}
	e.EmbedBatcher, e.SummaryBatcher = newFakeBatch(openai.BatchEmbed), sum
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	subs := sum.submissions()
	if fp.chatCalls.Load() != 1 || len(subs) != 1 || len(subs[0].items) != 3 || plan.LiveItems != 1 || plan.Summaries != 1 || plan.SpentUSD <= 0 {
		t.Fatalf("one live probe, three batched: chat=%d subs=%d plan %+v", fp.chatCalls.Load(), len(subs), plan)
	}
	if n := countLeaves(t, e, `summary != ''`); n != 1 {
		t.Fatalf("the probe's summary is saved: %d", n)
	}
	// every item refused for a field: the shape learns it
	sum.failErr = &openai.APIError{Status: 400, Message: "Unsupported parameter: 'reasoning_effort'"}
	for _, it := range subs[0].items {
		sum.failItems[it.ID] = true
	}
	e.EmbedBatcher.(*fakeBatch).set(openai.BatchDone)
	sum.set(openai.BatchDone)
	if err := e.WaitBatch(context.Background(), &plan, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CollectBatch(context.Background(), &plan); err != nil {
		t.Fatal(err)
	}
	if len(sum.refusals) != 1 || sum.refusals[0].Model != e.Models.Summary {
		t.Fatalf("the refusal is learned once for the summary model: %+v", sum.refusals)
	}
	// a learned model is batched without a probe
	sum.learned[e.Models.Summary] = true
	e2, fp2 := newEngine(t, true)
	seedBatchRoom(t, e2, batchRoom, 16, 0)
	e2.EmbedBatcher, e2.SummaryBatcher = newFakeBatch(openai.BatchEmbed), sum
	if _, _, err := e2.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil); err != nil || fp2.chatCalls.Load() != 0 {
		t.Fatalf("no probe for a learned shape: %v chat=%d", err, fp2.chatCalls.Load())
	}
}

// Finding B: a remainder that stops on a cancel keeps the applied plan, so the next run finishes it.
func TestBatchFinishKeepsThePlanWhenTheRemainderIsCancelled(t *testing.T) {
	fastBatch(t)
	e, _ := newEngine(t, false)
	e.Settings.EmbedBatch = 1
	seedBatchRoom(t, e, batchRoom, 16, 0)
	fb := newFakeBatch(openai.BatchEmbed)
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fb.drop["l0"] = true // left for the remainder
	fb.set(openai.BatchDone)
	if err := e.WaitBatch(context.Background(), &plan, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.CollectBatch(context.Background(), &plan); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := e.FinishBatch(ctx, &plan, nil); err == nil {
		t.Fatal("a cancelled remainder is not a finished preparation")
	}
	stored, ok, _ := e.LoadBatchPlan(batchRoom)
	if !ok || stored.State != BatchPlanApplied {
		t.Fatalf("the applied plan stays: %v %+v", ok, stored.State)
	}
	if rep, err := e.FinishBatch(context.Background(), &stored, nil); err != nil || rep.LeafVectors != 2 {
		t.Fatalf("the next run finishes it: %v %+v", err, rep)
	}
}

// Finding E: embedding results without usage (Gemini) are priced from the inputs' estimated tokens at
// the batch price, not a constant per input.
func TestBatchEmbedCostWithoutUsage(t *testing.T) {
	fastBatch(t)
	e, _ := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	fb := newFakeBatch(openai.BatchEmbed)
	fb.noUsage, fb.embedPrice = true, 1e-6
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || plan.Groups[0].Items[0].Tokens <= 0 {
		t.Fatalf("items carry their estimated tokens: %v %+v", err, plan.Groups)
	}
	fb.set(openai.BatchDone)
	if err := e.WaitBatch(context.Background(), &plan, nil); err != nil {
		t.Fatal(err)
	}
	applied, err := e.CollectBatch(context.Background(), &plan)
	if err != nil {
		t.Fatal(err)
	}
	want := float64(plan.Groups[0].Items[0].Tokens) * 1e-6 * 0.5
	if applied.SpentUSD < want-1e-15 || applied.SpentUSD > want+1e-15 {
		t.Fatalf("spent %v, want %v (estimated tokens at half price)", applied.SpentUSD, want)
	}
}

// Finding F: the items left live at submit are stored with the plan, so a resumed run reports them.
func TestBatchLiveItemsAreStoredWithThePlan(t *testing.T) {
	e, _ := newEngine(t, true)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	e.EmbedBatcher, e.SummaryBatcher = newFakeBatch(openai.BatchEmbed), newFakeBatch()
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || plan.LiveItems != 2 {
		t.Fatalf("%v %+v", err, plan)
	}
	stored, ok, _ := e.LoadBatchPlan(batchRoom)
	if !ok || stored.LiveItems != 2 || stored.BatchItems != 2 {
		t.Fatalf("stored %+v", stored)
	}
}
