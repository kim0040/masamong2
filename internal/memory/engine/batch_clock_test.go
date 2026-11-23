package engine

import (
	"context"
	"testing"
	"time"

	"masamong/internal/providers/openai"
)

// R07: a plan's CreatedAt and each group's intent SubmittedAt come from Engine.Now (nil = wall
// clock). The fixed time is synthetic and far from now (the seeded messages stay well behind it),
// so a wall-clock write would stand out. The submission's answer is lost on purpose: the group then
// keeps the intent reference the engine wrote before the call, which is where SubmittedAt is read.
func TestBatchPlanTimesComeFromTheInjectedClock(t *testing.T) {
	e, _ := newEngine(t, false)
	fixed := time.Now().Add(72 * time.Hour).UTC().Truncate(time.Second)
	e.Now = func() time.Time { return fixed }
	e.Store.Now = e.Now
	seedBatchRoom(t, e, batchRoom, 48, 0)
	fb := newFakeBatch(openai.BatchEmbed)
	fb.lost[0], fb.notTaken[0] = true, true
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.CreatedAt.Equal(fixed) {
		t.Fatalf("plan created at %v, want %v", plan.CreatedAt, fixed)
	}
	if len(plan.Groups) == 0 {
		t.Fatal("no group was prepared")
	}
	for i, g := range plan.Groups {
		if !g.Ref.SubmittedAt.Equal(fixed) {
			t.Fatalf("group %d submitted at %v, want the injected %v", i, g.Ref.SubmittedAt, fixed)
		}
	}
	var createdAt, updatedAt string
	if err := e.Store.DB.DB.QueryRow(`SELECT created_at, updated_at FROM memory_batch_plans WHERE room=?`, batchRoom).
		Scan(&createdAt, &updatedAt); err != nil {
		t.Fatal(err)
	}
	if want := fixed.Format(time.RFC3339); createdAt != want || updatedAt != want {
		t.Fatalf("plan row created_at=%q updated_at=%q, want %q", createdAt, updatedAt, want)
	}
}

// R07: a lost submission's settle wait is measured on the engine clock against the intent's stamp.
// The engine clock here is a day behind the wall's: on the wall clock the intent looks a day old and
// the settle long over, but on the engine's clock the submission is fresh and the host must not be
// asked yet.
func TestBatchReconcileSettleFollowsTheEngineClock(t *testing.T) {
	e, _ := newEngine(t, false)
	e.Now = func() time.Time { return time.Now().Add(-24 * time.Hour) }
	seedBatchRoom(t, e, batchRoom, 48, 0)
	fb := newFakeBatch(openai.BatchEmbed)
	fb.lost[0] = true // the answer was lost: the group keeps the intent the engine stamped
	fb.listed = true  // the host could answer if asked
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Groups) != 1 || plan.Groups[0].Submit == "" {
		t.Fatalf("no open submission to reconcile: %+v", plan.Groups)
	}
	old := batchReconcileSettle
	batchReconcileSettle = 30 * time.Second
	t.Cleanup(func() { batchReconcileSettle = old })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	if err := e.ReconcilePlan(ctx, &plan); err == nil {
		t.Fatal("the settle wait was skipped under an engine clock a day behind the wall's")
	}
	if fb.reconciles != 0 {
		t.Fatalf("the host was asked before its settle time: %d reconciles", fb.reconciles)
	}
}

// R07: the stamp an accepted batch keeps is the engine's intent stamp, not the adapter's wall
// time. A host that answers without an expiry is then measured against the give-up line on the
// engine's clock, and a fresh batch is not instantly abandoned under a clock ahead of the wall's.
func TestBatchAcceptedRefKeepsTheEngineStamp(t *testing.T) {
	e, _ := newEngine(t, false)
	fixed := time.Now().Add(100 * time.Hour).UTC().Truncate(time.Second)
	e.Now = func() time.Time { return fixed }
	e.Store.Now = e.Now
	seedBatchRoom(t, e, batchRoom, 48, 0)
	fb := newFakeBatch(openai.BatchEmbed) // accepts, then answers every poll "running" with no expiry
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Groups) != 1 {
		t.Fatalf("one group expected: %+v", plan.Groups)
	}
	g := &plan.Groups[0]
	if !g.Ref.SubmittedAt.Equal(fixed) {
		t.Fatalf("the accepted batch keeps a %v stamp, want the intent's %v", g.Ref.SubmittedAt, fixed)
	}
	open, err := e.pollPlan(context.Background(), &plan)
	if err != nil {
		t.Fatal(err)
	}
	if !open || g.Abandoned || g.State != openai.BatchRunning {
		t.Fatalf("a fresh batch was given up under the offset clock: open=%v state=%s abandoned=%v", open, g.State, g.Abandoned)
	}
}
