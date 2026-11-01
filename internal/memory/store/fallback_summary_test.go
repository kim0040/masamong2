package store

import (
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
)

// A summary that is only the body's opening (the model spent its budget thinking) is cleared once so the
// next pass summarizes the leaf again; a real summary stays. The stand-in cached under the body is
// replaced when the real summary of that body is cached.
func TestFallbackSummariesAreClearedOnce(t *testing.T) {
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
	if summary != "" || vec != nil {
		t.Fatalf("the stand-in summary is cleared for the next pass: %q %v", summary, vec)
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
