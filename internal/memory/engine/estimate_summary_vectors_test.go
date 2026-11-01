package engine

import (
	"math"
	"testing"

	"masamong/internal/memory/store"
)

// Leaves that only lack a summary vector are embedded one call each by summarize. The estimate used to
// count them nowhere (0 s, $0), so the console's quick reindex let a pass of 199 such calls through its
// one-minute gate.
func TestEstimateCountsSummaryVectorsOnly(t *testing.T) {
	e, _ := newEngine(t, false)
	e.Settings.Summaries = true
	pending := make([]store.PendingLeaf, 199)
	for i := range pending {
		pending[i] = store.PendingLeaf{Body: "a short leaf", NeedSumVec: true}
	}
	est := e.estimate(nil, pending, nil)
	if est.SummaryVectors != 199 {
		t.Fatalf("summary vectors = %d, want 199", est.SummaryVectors)
	}
	// one embeddings call each, Workers at a time
	want := int(math.Ceil(math.Ceil(199/float64(max(1, e.Settings.Workers))) * e.rates().EmbedBatchSec))
	if est.Seconds != want || want == 0 {
		t.Fatalf("199 single embedding calls estimated at %d s, want %d", est.Seconds, want)
	}
	// on one worker (a 1-core box's default) that is far past the console's one-minute quick gate
	e.Settings.Workers = 1
	if est := e.estimate(nil, pending, nil); est.Seconds <= 60 {
		t.Fatalf("one worker: %d s", est.Seconds)
	}
	if est.EmbedUSD <= 0 || est.TotalUSD < est.EmbedUSD {
		t.Fatalf("summary vectors cost nothing: %+v", est)
	}
	// with summaries off the pass does not touch summary vectors, so neither does the estimate
	e.Settings.Summaries = false
	if est := e.estimate(nil, pending, nil); est.SummaryVectors != 0 || est.Seconds != 0 {
		t.Fatalf("summaries off: %+v", est)
	}
}
