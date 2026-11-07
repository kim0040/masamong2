package store

import (
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
)

// A summary that is only the body's opening (the model spent its budget thinking) stays as it is, with its vector: an
// earlier release cleared them at Open, and every one of them went back to the models on the next pass. No start may do
// that. The stand-in cached under the body is replaced when the real summary of that body is cached.
func TestFallbackSummariesAreNotCleared(t *testing.T) {
	st := openStore(t)
	o := mk(0, time.Now().Add(-time.Hour))
	if _, err := st.InsertObservations([]corpus.Observation{o}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncLeaves(o.SourceRoom, leaves.Recipe{Size: 8, Split: leaves.SplitFixed}, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	var leaf, body string
	if err := st.DB.DB.QueryRow(`SELECT leaf_id,body FROM memory_leaves`).Scan(&leaf, &body); err != nil {
		t.Fatal(err)
	}
	r := []rune(body)
	opening := string(r[:min(180, len(r))])
	rerun := func() {
		t.Helper()
		if _, err := st.DB.DB.Exec(`DELETE FROM memory_privacy_migration WHERE id=2`); err != nil {
			t.Fatal(err)
		}
		if err := st.migrateFallbackSummaries(); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.SaveSummary(leaf, opening, []byte{1, 2}, body); err != nil {
		t.Fatal(err)
	}
	if err := st.PutSummaryText("stand-in", leaf, body, opening); err != nil {
		t.Fatal(err)
	}
	rerun()
	var summary string
	var vec []byte
	_ = st.DB.DB.QueryRow(`SELECT summary, summary_vec FROM memory_leaves WHERE leaf_id=?`, leaf).Scan(&summary, &vec)
	if summary != opening || len(vec) != 2 {
		t.Fatalf("opening the store changed a stored summary: %q %v", summary, vec)
	}
	var marked int
	if err := st.DB.DB.QueryRow(`SELECT done FROM memory_privacy_migration WHERE id=2`).Scan(&marked); err != nil || marked != 1 {
		t.Fatalf("the marker: %d %v", marked, err)
	}
	if err := st.PutSummaryText("stand-in", leaf, body, "주말 캠핑 약속"); err != nil {
		t.Fatal(err)
	}
	if v, ok := st.CachedVector("stand-in"); !ok || string(v) != "주말 캠핑 약속" {
		t.Fatalf("the real summary replaces the cached stand-in: %q %v", v, ok)
	}

	if err := st.SaveSummary(leaf, "주말 캠핑 약속", []byte{1, 2}, body); err != nil {
		t.Fatal(err)
	}
	rerun()
	_ = st.DB.DB.QueryRow(`SELECT summary FROM memory_leaves WHERE leaf_id=?`, leaf).Scan(&summary)
	if summary != "주말 캠핑 약속" {
		t.Fatalf("a real summary stays: %q", summary)
	}
}
