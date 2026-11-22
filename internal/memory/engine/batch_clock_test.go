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
