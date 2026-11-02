package engine

import (
	"context"
	"errors"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/leaves"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
	"masamong/internal/storage"
)

// engineAt is newEngine on a database file the test names, so it can be closed and opened again.
func engineAt(t *testing.T, path string, summaries bool) (*Engine, *fakeProvider, *storage.Store) {
	t.Helper()
	db, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.Open(db)
	if err != nil {
		_ = db.Close()
		t.Fatal(err)
	}
	fp := &fakeProvider{}
	srv := httptest.NewServer(fp.handler())
	t.Cleanup(srv.Close)
	t.Setenv("FAKE_KEY", "k")
	cred, _ := providers.Resolve("env:FAKE_KEY")
	a := openai.New(providers.NewClient(5*time.Second, nil), srv.URL, cred)
	e := New(st, a, Models{Embed: "fake-embed", Rerank: "fake-rerank", Summary: "fake-summary"},
		Settings{Search: search.Default(), Recipe: leaves.DefaultRecipe(), Summaries: summaries, RefTZ: time.UTC, HedgeAfter: time.Second})
	return e, fp, db
}

// R1: a batch the host accepted is never cancelled, cleaned up or forgotten when the plan write after it
// fails. A failure that clears is tried again; one that stays fails the preparation with the stored intent
// kept (the room held), and the next run looks the batch up by its key instead of sending it again.
func TestBatchAcceptedBatchSurvivesAFailedPlanSave(t *testing.T) {
	fastBatch(t)
	t.Run("a failure that clears", func(t *testing.T) {
		e, fp := newEngine(t, false)
		seedBatchRoom(t, e, batchRoom, 16, 0)
		fb := newFakeBatch(openai.BatchEmbed)
		e.EmbedBatcher = fb
		failures := 0
		t.Cleanup(func() { planSaveFault = nil })
		fb.submitErr = func(int) error {
			// the writes after the create call fail twice (a locked database), then work
			planSaveFault = func(*BatchPlan) error {
				if failures < 2 {
					failures++
					return errors.New("database is locked")
				}
				return nil
			}
			return nil
		}
		plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
		planSaveFault = nil
		if err != nil || failures != 2 || len(plan.Groups) != 1 || plan.Groups[0].Ref.ID == "" || plan.Groups[0].Submit != "" {
			t.Fatalf("the accepted batch is kept: %v failures=%d %+v", err, failures, plan.Groups)
		}
		if len(fb.cancels)+len(fb.cleanups) != 0 {
			t.Fatalf("an accepted batch is never taken back: cancels %v cleanups %v", fb.cancels, fb.cleanups)
		}
		stored, ok, _ := e.LoadBatchPlan(batchRoom)
		if !ok || len(stored.Groups) != 1 || stored.Groups[0].Ref.ID != plan.Groups[0].Ref.ID || stored.Groups[0].Submit != "" {
			t.Fatalf("the retried write stored the batch: %v %+v", ok, stored.Groups)
		}
		fb.set(openai.BatchDone)
		if rep := runBatch(t, e, &plan); rep.LeafVectors != 2 || fb.calls != 1 || fp.embedCalls.Load() != 0 {
			t.Fatalf("collected once, nothing live: %+v calls=%d live=%d", rep, fb.calls, fp.embedCalls.Load())
		}
	})
	t.Run("a failure that stays", func(t *testing.T) {
		e, fp := newEngine(t, false)
		seedBatchRoom(t, e, batchRoom, 16, 0)
		fb := newFakeBatch(openai.BatchEmbed)
		fb.listed = true
		e.EmbedBatcher = fb
		fb.submitErr = func(int) error {
			// the intent is stored; from now on every update of the plan fails (a full disk)
			_, err := e.Store.DB.DB.Exec(`CREATE TRIGGER fail_plan_writes BEFORE UPDATE ON memory_batch_plans BEGIN SELECT RAISE(ABORT, 'disk full'); END`)
			return err
		}
		plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
		if err == nil || len(plan.Groups) != 1 || plan.Groups[0].Ref.ID == "" {
			t.Fatalf("the preparation fails and still knows the batch: %v %+v", err, plan.Groups)
		}
		if len(fb.cancels)+len(fb.cleanups) != 0 {
			t.Fatalf("an accepted batch is never taken back: cancels %v cleanups %v", fb.cancels, fb.cleanups)
		}
		stored, ok, _ := e.LoadBatchPlan(batchRoom)
		if !ok || len(stored.Groups) != 1 || stored.Groups[0].Submit != submitPending || stored.Generation != plan.Generation {
			t.Fatalf("the stored intent stays: %v %+v", ok, stored)
		}
		if held, _ := e.BatchWaitingRooms(); !held[batchRoom] {
			t.Fatal("the room stays held")
		}
		// the disk recovers: the next run finds the batch by its key and collects it, never sending it again
		if _, err := e.Store.DB.DB.Exec(`DROP TRIGGER fail_plan_writes`); err != nil {
			t.Fatal(err)
		}
		restarted := New(e.Store, e.LLM, e.Models, e.Settings)
		restarted.EmbedBatcher = fb
		again, _, err := restarted.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
		if err != nil || again.Generation != plan.Generation {
			t.Fatalf("resume: %v %+v", err, again)
		}
		fb.set(openai.BatchDone)
		rep := runBatch(t, restarted, &again)
		if fb.calls != 1 || fb.reconciles != 1 || fp.embedCalls.Load() != 0 || rep.LeafVectors != 2 {
			t.Fatalf("create calls %d, lookups %d, live %d, report %+v", fb.calls, fb.reconciles, fp.embedCalls.Load(), rep)
		}
	})
}

// R2: a database written before plans had a generation (no column, none in the JSON) holding active plans
// is migrated so they can be finished and cancelled: the column and the JSON get the same generation.
func TestBatchLegacyPlanWithoutAGenerationFinishesAndCancels(t *testing.T) {
	fastBatch(t)
	path := filepath.Join(t.TempDir(), "m.db")
	other := "kakao:batch-room-2"
	e, _, db := engineAt(t, path, false)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	seedBatchRoom(t, e, other, 16, 0)
	fb := newFakeBatch(openai.BatchEmbed)
	e.EmbedBatcher = fb
	for _, room := range []string{batchRoom, other} {
		if _, _, err := e.PrepareBatch(context.Background(), room, BatchOptions{}, nil); err != nil {
			t.Fatal(err)
		}
	}
	// turn the plans into what the earlier release stored: the table without the column, the JSON without it
	for _, q := range []string{
		`CREATE TABLE legacy_plans AS SELECT room, state, items, created_at, updated_at, json_remove(plan, '$.generation') AS plan FROM memory_batch_plans`,
		`DROP TABLE memory_batch_plans`,
		`CREATE TABLE memory_batch_plans (room TEXT PRIMARY KEY, state TEXT NOT NULL, items INTEGER NOT NULL DEFAULT 0, created_at TEXT NOT NULL, updated_at TEXT NOT NULL, plan TEXT NOT NULL)`,
		`INSERT INTO memory_batch_plans SELECT room, state, items, created_at, updated_at, plan FROM legacy_plans`,
		`DROP TABLE legacy_plans`,
	} {
		if _, err := db.DB.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	// the upgraded release opens it
	e2, fp, db2 := engineAt(t, path, false)
	t.Cleanup(func() { _ = db2.Close() })
	e2.EmbedBatcher = fb
	var gens int
	if err := db2.DB.QueryRow(`SELECT count(*) FROM memory_batch_plans WHERE generation <> '' AND json_extract(plan, '$.generation') = generation`).Scan(&gens); err != nil || gens != 2 {
		t.Fatalf("both plans get one generation in the column and the JSON: %d %v", gens, err)
	}
	plan, ok, err := e2.LoadBatchPlan(batchRoom)
	if err != nil || !ok || len(plan.Generation) != 32 {
		t.Fatalf("legacy plan: %v %v %+v", ok, err, plan)
	}
	fb.set(openai.BatchDone)
	if rep := runBatch(t, e2, &plan); rep.LeafVectors != 2 || fp.embedCalls.Load() != 0 {
		t.Fatalf("the legacy plan finishes: %+v live=%d", rep, fp.embedCalls.Load())
	}
	if _, ok, _ := e2.LoadBatchPlan(batchRoom); ok {
		t.Fatal("the finished legacy plan is deleted")
	}
	second, ok, _ := e2.LoadBatchPlan(other)
	if !ok {
		t.Fatal("the second legacy plan is stored")
	}
	if _, done, err := e2.CancelPlan(context.Background(), &second); err != nil || !done {
		t.Fatalf("the legacy plan cancels: done=%v %v", done, err)
	}
	if held, _ := e2.BatchWaitingRooms(); held[batchRoom] || held[other] {
		t.Fatalf("both rooms are released: %v", held)
	}
}

// R5: a summary probe that fails (a rate limit here) sends nothing and moves nothing to the live price:
// the preparation fails with the reason and no plan holds the room.
func TestBatchFailedProbeSendsNothingAndRunsNothingLive(t *testing.T) {
	e, fp := newEngine(t, true)
	e.LLM.Client.MaxRetries = 0
	seedBatchRoom(t, e, batchRoom, 32, 0) // 4 leaves
	emb := newFakeBatch(openai.BatchEmbed)
	sum := &shapeFake{fakeBatch: newFakeBatch(openai.BatchChat), learned: map[string]bool{}}
	e.EmbedBatcher, e.SummaryBatcher = emb, sum
	fp.chatStatus.Store(429)
	plan, rep, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if !errors.Is(err, ErrBatchSubmitRefused) || !strings.Contains(err.Error(), "first live request") {
		t.Fatalf("the probe's failure ends the preparation: %v", err)
	}
	if len(plan.Groups) != 0 || plan.LiveItems != 0 || rep.LiveItems != 0 || len(emb.submissions())+len(sum.submissions()) != 0 {
		t.Fatalf("nothing sent, nothing planned live: %+v %+v subs=%d", plan, rep, len(emb.submissions())+len(sum.submissions()))
	}
	if fp.chatCalls.Load() != 1 || fp.embedCalls.Load() != 0 {
		t.Fatalf("only the probe was called: chat=%d embed=%d", fp.chatCalls.Load(), fp.embedCalls.Load())
	}
	if held, _ := e.BatchWaitingRooms(); held[batchRoom] {
		t.Fatal("no plan holds the room")
	}
}

// R6: an operator's discard while a draining plan's unknown submission is being looked up is not undone
// by the lookup's write: the plan stays deleted and the drain ends.
func TestBatchDiscardDuringReconcileDoesNotRecreateThePlan(t *testing.T) {
	fastBatch(t)
	e, _ := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	fb := newFakeBatch(openai.BatchEmbed)
	fb.lost[0] = true // and no list: the host cannot tell
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || len(plan.Groups) != 1 || plan.Groups[0].Submit != submitUnknown {
		t.Fatalf("%v %+v", err, plan.Groups)
	}
	if _, done, err := e.CancelPlan(context.Background(), &plan); err != nil || done {
		t.Fatalf("an unknown submission keeps the plan draining: done=%v %v", done, err)
	}
	fb.onReconcile = func() {
		if err := e.DiscardBatch(context.Background(), batchRoom); err != nil {
			t.Error(err)
		}
	}
	if _, err := e.DrainBatch(context.Background(), &plan); err != nil {
		t.Fatalf("the drain of a discarded plan ends quietly: %v", err)
	}
	if _, ok, _ := e.LoadBatchPlan(batchRoom); ok {
		t.Fatal("a discarded plan must not be stored again")
	}
	if held, _ := e.BatchWaitingRooms(); held[batchRoom] {
		t.Fatal("the discarded room is released")
	}
}

// R6: a drain waiting for a batch that keeps running ends when its plan is discarded.
func TestBatchDiscardEndsADrainThatWaits(t *testing.T) {
	fastBatch(t)
	e, _ := newEngine(t, false)
	seedBatchRoom(t, e, batchRoom, 16, 0)
	fb := newFakeBatch(openai.BatchEmbed) // ignores the cancel: its batch keeps running
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, done, err := e.CancelPlan(context.Background(), &plan); err != nil || done {
		t.Fatalf("done=%v %v", done, err)
	}
	drained := make(chan error, 1)
	go func() { _, err := e.DrainBatch(context.Background(), &plan); drained <- err }()
	before := fb.polls.Load()
	for deadline := time.Now().Add(5 * time.Second); fb.polls.Load() < before+2; {
		if time.Now().After(deadline) {
			t.Fatal("the drain does not poll")
		}
		time.Sleep(time.Millisecond)
	}
	if err := e.DiscardBatch(context.Background(), batchRoom); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-drained:
		if err != nil {
			t.Fatalf("the drain of a discarded plan ends quietly: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a drain of a discarded plan must end")
	}
	if _, ok, _ := e.LoadBatchPlan(batchRoom); ok {
		t.Fatal("the plan stays discarded")
	}
}

// R8: the summary probe's full price is part of the estimate the budget is checked against: a budget that
// covers the batch at the batch price but not the probe on top stops before anything is paid.
func TestBatchEstimateIncludesTheProbeAtTheLivePrice(t *testing.T) {
	learned, _ := newEngine(t, true)
	seedBatchRoom(t, learned, batchRoom, 32, 0)
	learned.EmbedBatcher = newFakeBatch(openai.BatchEmbed)
	learned.SummaryBatcher = &shapeFake{fakeBatch: newFakeBatch(openai.BatchChat), learned: map[string]bool{"fake-summary": true}}
	_, rep, err := learned.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil || rep.Estimate == nil || rep.Estimate.BatchUSD == nil {
		t.Fatalf("%v %+v", err, rep.Estimate)
	}
	withoutProbe := *rep.Estimate.BatchUSD
	perSummary := rep.Estimate.summaryChatUSD / float64(rep.Estimate.Summaries)

	e, fp := newEngine(t, true)
	seedBatchRoom(t, e, batchRoom, 32, 0)
	emb := newFakeBatch(openai.BatchEmbed)
	sum := &shapeFake{fakeBatch: newFakeBatch(openai.BatchChat), learned: map[string]bool{}}
	e.EmbedBatcher, e.SummaryBatcher = emb, sum
	_, rep, err = e.PrepareBatch(context.Background(), batchRoom, BatchOptions{BudgetUSD: withoutProbe + perSummary*0.25}, nil)
	if err != nil || rep.StoppedBy != "budget" {
		t.Fatalf("the probe at the live price is over the budget: %v %+v", err, rep)
	}
	if got := *rep.Estimate.BatchUSD; got < withoutProbe+perSummary*0.5-1e-12 {
		t.Fatalf("the estimate carries the probe's live price: %v, without it %v", got, withoutProbe)
	}
	if fp.chatCalls.Load() != 0 || len(emb.submissions())+len(sum.submissions()) != 0 {
		t.Fatalf("nothing paid: chat=%d subs=%d", fp.chatCalls.Load(), len(emb.submissions())+len(sum.submissions()))
	}
}
