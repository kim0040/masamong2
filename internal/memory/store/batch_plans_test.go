package store

import (
	"encoding/json"
	"errors"
	"testing"
)

// R2: a plan left with an empty generation column (by the release that added the column without filling
// it, or written before plans had one) gets one at the next start: the one its JSON names, else a new one
// written to the column and the JSON together. A plan whose JSON cannot be read is left alone.
func TestBatchPlanGenerationBackfill(t *testing.T) {
	st := openStore(t)
	named := "0123456789abcdef0123456789abcdef"
	for room, plan := range map[string]string{
		"kakao:named":   `{"version":1,"room":"kakao:named","generation":"` + named + `","state":"waiting"}`,
		"kakao:none":    `{"version":1,"room":"kakao:none","state":"draining"}`,
		"kakao:garbled": `{not json`,
	} {
		if _, err := st.DB.DB.Exec(`INSERT INTO memory_batch_plans(room, generation, state, items, created_at, updated_at, plan) VALUES(?,'','waiting',1,'t','t',?)`, room, plan); err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateBatchPlans(st.DB); err != nil {
		t.Fatal(err)
	}
	gen := func(room string) (column, inJSON string) {
		t.Helper()
		var raw string
		if err := st.DB.DB.QueryRow(`SELECT generation, plan FROM memory_batch_plans WHERE room=?`, room).Scan(&column, &raw); err != nil {
			t.Fatal(err)
		}
		var p struct {
			Generation string `json:"generation"`
		}
		_ = json.Unmarshal([]byte(raw), &p)
		return column, p.Generation
	}
	if c, j := gen("kakao:named"); c != named || j != named {
		t.Fatalf("the JSON's generation fills the column: %q %q", c, j)
	}
	if c, j := gen("kakao:none"); len(c) != 32 || c != j {
		t.Fatalf("a new generation goes to the column and the JSON: %q %q", c, j)
	}
	if c, _ := gen("kakao:garbled"); c != "" {
		t.Fatalf("an unreadable plan is left for the engine to drop: %q", c)
	}
	// a backfilled plan takes compare-and-set writes of its generation
	if err := st.SaveBatchPlan("kakao:named", named, BatchPlanRecord{State: "applied", CreatedAt: "t", Plan: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
}

// R6: only InsertBatchPlan creates a plan; SaveBatchPlan and CommitBatchApply update the stored
// generation only, so a late write of a deleted or replaced plan changes nothing.
func TestBatchPlanInsertOnlyAndUpdateOnly(t *testing.T) {
	st := openStore(t)
	rec := BatchPlanRecord{State: "waiting", Items: 1, CreatedAt: "t", Plan: []byte(`{}`)}
	if err := st.SaveBatchPlan("kakao:r", "g1", rec); !errors.Is(err, ErrBatchPlanMissing) {
		t.Fatalf("an update never creates a plan: %v", err)
	}
	if err := st.InsertBatchPlan("kakao:r", "g1", rec); err != nil {
		t.Fatal(err)
	}
	if err := st.InsertBatchPlan("kakao:r", "g2", rec); !errors.Is(err, ErrBatchPlanExists) {
		t.Fatalf("a second plan for the room is refused: %v", err)
	}
	if err := st.SaveBatchPlan("kakao:r", "g2", rec); !errors.Is(err, ErrBatchPlanStale) {
		t.Fatalf("another generation is not written: %v", err)
	}
	if deleted, err := st.DeleteBatchPlanGeneration("kakao:r", "g1"); err != nil || !deleted {
		t.Fatalf("%v %v", deleted, err)
	}
	commit := func(BatchWritten) (BatchPlanRecord, error) { return rec, nil }
	if err := st.CommitBatchApply("kakao:r", "g1", BatchWrites{}, []BatchSpend{{Kind: "embed", USD: 1, Calls: 1}}, commit); !errors.Is(err, ErrBatchPlanMissing) {
		t.Fatalf("a late apply of a deleted plan writes nothing: %v", err)
	}
	if err := st.SaveBatchPlan("kakao:r", "g1", rec); !errors.Is(err, ErrBatchPlanMissing) {
		t.Fatalf("a late save of a deleted plan does not bring it back: %v", err)
	}
	var plans, spends int
	_ = st.DB.DB.QueryRow(`SELECT count(*) FROM memory_batch_plans`).Scan(&plans)
	_ = st.DB.DB.QueryRow(`SELECT count(*) FROM memory_spend`).Scan(&spends)
	if plans != 0 || spends != 0 {
		t.Fatalf("nothing written: plans %d spends %d", plans, spends)
	}
}
