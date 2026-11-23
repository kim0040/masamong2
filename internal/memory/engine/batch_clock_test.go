package engine

import (
	"context"
	"testing"
	"time"

	"masamong/internal/providers/openai"
)

// R07: a plan's CreatedAt comes from Engine.Now (nil = wall clock); each group's SubmittedAt is wall
// time, because it is compared with the times the host reports. The fixed engine time is synthetic and
// far from now, so a stamp on the wrong clock stands out. The submission's answer is lost on purpose:
// the group then keeps the intent reference the engine wrote before the call.
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
		if d := time.Since(g.Ref.SubmittedAt); d < 0 || d > time.Minute {
			t.Fatalf("group %d submitted at %v, want wall time near %v", i, g.Ref.SubmittedAt, time.Now())
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

// A lost submission's settle wait is measured on the wall clock against the intent's wall stamp. The
// engine clock here is a day behind: an engine stamp read on the wall clock would look a day old and
// skip the settle, so the host would be asked at once.
func TestBatchReconcileSettleIsOnOneClock(t *testing.T) {
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

// An accepted batch keeps the adapter's own stamp (wall time; the adapter prices and reconciles with
// it). A host that answers without an expiry is measured against the give-up line on the wall clock,
// so a fresh batch is not abandoned under an engine clock far ahead of the wall's.
func TestBatchAcceptedRefKeepsTheAdapterStamp(t *testing.T) {
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
	if want := fb.subs[0].ref.SubmittedAt; !g.Ref.SubmittedAt.Equal(want) {
		t.Fatalf("the accepted batch keeps a %v stamp, want the adapter's %v", g.Ref.SubmittedAt, want)
	}
	open, err := e.pollPlan(context.Background(), &plan)
	if err != nil {
		t.Fatal(err)
	}
	if !open || g.Abandoned || g.State != openai.BatchRunning {
		t.Fatalf("a fresh batch was given up under the offset clock: open=%v state=%s abandoned=%v", open, g.State, g.Abandoned)
	}
}

// Review round 2: a host's own expiry is host (wall) time. With the engine clock two days ahead, a
// batch the host says expires in a day is still running and must not be cancelled and abandoned
// (abandoning would send its still-pending items through the paid live path again).
func TestBatchHostExpiryIsReadOnTheWallClock(t *testing.T) {
	e, _ := newEngine(t, false)
	e.Now = func() time.Time { return time.Now().Add(48 * time.Hour) }
	e.Store.Now = e.Now
	seedBatchRoom(t, e, batchRoom, 48, 0)
	fb := newFakeBatch(openai.BatchEmbed)
	fb.expiresAt = time.Now().Add(24 * time.Hour)
	e.EmbedBatcher = fb
	plan, _, err := e.PrepareBatch(context.Background(), batchRoom, BatchOptions{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	open, err := e.pollPlan(context.Background(), &plan)
	if err != nil {
		t.Fatal(err)
	}
	g := plan.Groups[0]
	if !open || g.Abandoned || len(fb.cancels) != 0 {
		t.Fatalf("a batch the host still runs was abandoned: open=%v abandoned=%v cancels=%v", open, g.Abandoned, fb.cancels)
	}
}
