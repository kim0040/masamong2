package engine

import (
	"context"
	"errors"
	"strings"
	"testing"

	"masamong/internal/providers/openai"
)

// These pin what PrepareBatch reports on the paths the other batch tests do not take, so splitting it into
// stages keeps every one of them.

// A retired engine prepares nothing.
func TestBatchPrepareOnARetiredEngineRefuses(t *testing.T) {
	e, _ := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	e.EmbedBatcher = newFakeBatch(openai.BatchEmbed)
	e.Retire(nil)
	plan, rep, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if !errors.Is(err, errRetired) || plan.Generation != "" || rep.Mode != "batch" || rep.Pending != 0 {
		t.Fatalf("%v %+v %+v", err, plan, rep)
	}
}

// Without a configured provider nothing is sent and nothing is stored: the report says why, with the
// estimate of the work that waits.
func TestBatchPrepareWithoutAProviderStopsBeforeSubmitting(t *testing.T) {
	e, _ := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	fb := newFakeBatch(openai.BatchEmbed)
	e.EmbedBatcher = fb
	e.LLM = nil
	plan, rep, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || plan.Generation != "" || plan.LiveItems != 0 || plan.BatchItems != 0 {
		t.Fatalf("%v %+v", err, plan)
	}
	if rep.StoppedBy != "provider_not_configured" || rep.Estimate == nil || rep.Pending != 2 || rep.LiveItems != 0 {
		t.Fatalf("report %+v", rep)
	}
	if len(fb.submissions()) != 0 {
		t.Fatal("nothing is submitted without a provider")
	}
	if _, ok, _ := e.LoadBatchPlan(batchRoom); ok {
		t.Fatal("no plan holds the room")
	}
}

// A preparation cancelled before its first submission sends nothing, stores no plan and hands the whole
// selection to the caller's live pass, which the cancel stops in turn.
func TestBatchPrepareCancelledBeforeSubmittingSendsNothing(t *testing.T) {
	e, fp := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 16, 0) // 2 closed leaves, one embedding request
	fb := newFakeBatch(openai.BatchEmbed)
	e.EmbedBatcher = fb
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan, rep, err := e.PrepareBatch(ctx, batchRoom, BatchOptions{BudgetUSD: 1, Limit: 5}, nil)
	if err != nil || len(plan.Groups) != 0 || plan.Generation != "" || plan.Room != batchRoom || plan.LiveItems != 2 || plan.BudgetUSD != 1 || plan.Limit != 5 {
		t.Fatalf("%v %+v", err, plan)
	}
	if plan.Workset == nil || len(plan.Workset.Leaves) != 2 {
		t.Fatalf("the limited selection goes to the live pass: %+v", plan.Workset)
	}
	if rep.StoppedBy != "cancelled" || rep.LiveItems != 2 || rep.BatchItems != 0 {
		t.Fatalf("report %+v", rep)
	}
	if len(fb.submissions()) != 0 || fp.embedCalls.Load() != 0 {
		t.Fatal("a cancelled preparation sends nothing")
	}
	if _, ok, _ := e.LoadBatchPlan(batchRoom); ok {
		t.Fatal("no plan holds the room")
	}
}

// A preparation cancelled during the summary probe stops quietly: nothing is sent and the caller gets the
// unsent selection.
func TestBatchPrepareCancelledDuringTheProbeSendsNothing(t *testing.T) {
	e, _ := newEngine(t, true)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	sum := &shapeFake{fakeBatch: newFakeBatch(openai.BatchChat), learned: map[string]bool{}}
	emb := newFakeBatch(openai.BatchEmbed)
	e.EmbedBatcher, e.SummaryBatcher = emb, sum
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	plan, rep, err := e.PrepareBatch(ctx, batchRoom, BatchOptions{}, nil)
	if err != nil || len(plan.Groups) != 0 || plan.Generation != "" || plan.Room != batchRoom || plan.Summaries != 0 {
		t.Fatalf("%v %+v", err, plan)
	}
	if rep.StoppedBy != "cancelled" {
		t.Fatalf("report %+v", rep)
	}
	if len(emb.submissions())+len(sum.submissions()) != 0 {
		t.Fatal("a cancelled probe sends no batch")
	}
	if _, ok, _ := e.LoadBatchPlan(batchRoom); ok {
		t.Fatal("no plan holds the room")
	}
}

// When the intent cannot be stored, nothing is sent and nothing runs live: the error comes back.
func TestBatchPrepareIntentNotStoredSendsNothing(t *testing.T) {
	e, fp := newEngine(t, true)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	fb := newFakeBatch(openai.BatchEmbed, openai.BatchChat)
	e.EmbedBatcher = fb
	if _, err := e.Store.DB.DB.Exec(`CREATE TRIGGER fail_plan_inserts BEFORE INSERT ON memory_batch_plans BEGIN SELECT RAISE(ABORT, 'disk full'); END`); err != nil {
		t.Fatal(err)
	}
	plan, rep, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err == nil || !strings.Contains(err.Error(), "disk full") || errors.Is(err, ErrBatchSubmitRefused) {
		t.Fatalf("the store error comes back: %v", err)
	}
	if len(plan.Groups) != 0 || plan.Generation != "" || plan.LiveItems != 0 || rep.StoppedBy != "" {
		t.Fatalf("%+v %+v", plan, rep)
	}
	if len(fb.submissions()) != 0 || fb.calls != 0 || fp.embedCalls.Load()+fp.chatCalls.Load() != 0 {
		t.Fatal("nothing is sent, nothing runs live")
	}
}

// A submission whose answer was lost keeps the input file the host reported, so the lookup can find it,
// and the next group is still submitted.
func TestBatchPrepareLostAnswerKeepsTheInputFile(t *testing.T) {
	e, fp := newEngine(t, true)
	seedBatchRoom(t, e, batchRoom, 16, 0) // 2 leaves: one embedding request, two summaries
	fb := newFakeBatch(openai.BatchEmbed, openai.BatchChat)
	fb.submitErr = func(n int) error {
		if n == 0 {
			return &openai.SubmitUnknownError{Err: errors.New("connection reset"), Ref: openai.BatchRef{InputFile: "file_lost_1"}}
		}
		return nil
	}
	e.EmbedBatcher = fb
	plan, rep, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || len(plan.Groups) != 2 || plan.State != BatchPlanWaiting {
		t.Fatalf("%v %+v", err, plan)
	}
	g := plan.Groups[0]
	if g.Submit != submitUnknown || g.Ref.InputFile != "file_lost_1" || g.Ref.SubmitKey == "" || g.Error == "" {
		t.Fatalf("lost group %+v", g)
	}
	if plan.Groups[1].Submit != "" || plan.Groups[1].Ref.ID == "" {
		t.Fatalf("the next group is submitted: %+v", plan.Groups[1])
	}
	if plan.BatchItems != 4 || plan.LiveItems != 0 || rep.BatchItems != 4 || fp.embedCalls.Load()+fp.chatCalls.Load() != 0 {
		t.Fatalf("items batch=%d live=%d report %+v", plan.BatchItems, plan.LiveItems, rep)
	}
	stored, ok, _ := e.LoadBatchPlan(batchRoom)
	if !ok || stored.Groups[0].Ref.InputFile != "file_lost_1" {
		t.Fatalf("the stored plan keeps the input file: %v %+v", ok, stored.Groups)
	}
}

// The progress callback hears of the batch once, with the requests sent.
func TestBatchPrepareReportsTheRequestsSent(t *testing.T) {
	e, _ := newEngine(t, false)
	e.Settings.EmbedBatch = 2
	seedBatchRoom(t, e, batchRoom, 48, 0) // 6 leaves → 3 requests
	e.EmbedBatcher = newFakeBatch(openai.BatchEmbed)
	var got []int
	_, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, func(stage string, done, total int) {
		if stage == "batch" {
			got = append(got, done, total)
		}
	})
	if err != nil || len(got) != 2 || got[0] != 0 || got[1] != 3 {
		t.Fatalf("%v %v", err, got)
	}
}
