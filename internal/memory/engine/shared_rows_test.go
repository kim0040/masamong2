package engine

import (
	"context"
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/search"
)

// This fails on the old layout: AppendRowQ allocates and copies even unchanged rows.
func TestRebuildSharesRowsWhileScoring(t *testing.T) {
	e, _ := newEngine(t, true)
	e.Settings.MessageVectors = true
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	old := e.Index()
	q := vecFor("camping test query")
	var wg sync.WaitGroup
	started := make(chan struct{}, 3)
	done := make(chan struct{})
	for _, matrix := range []search.Matrix{old.LeafVecs, old.SummaryVecs, old.MsgVecs} {
		wg.Add(1)
		go func(m search.Matrix) {
			defer wg.Done()
			started <- struct{}{}
			for {
				for i := 0; i < m.Rows(); i++ {
					_ = m.Dot(i, q)
				}
				select {
				case <-done:
					return
				default:
				}
			}
		}(matrix)
	}
	defer func() { close(done); wg.Wait() }()
	for i := 0; i < 3; i++ {
		<-started
	}
	if _, err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	next := e.Index()
	for _, l := range old.Leaves {
		a, sa, _ := old.QuantizedLeaf(l.DocID)
		b, sb, _ := next.QuantizedLeaf(l.DocID)
		if &a[0] != &b[0] || sa != sb {
			t.Fatal("unchanged leaf row was copied")
		}
		a, sa, ok := old.QuantizedSummary(l.DocID)
		if ok {
			b, sb, _ = next.QuantizedSummary(l.DocID)
			if &a[0] != &b[0] || sa != sb {
				t.Fatal("unchanged summary row was copied")
			}
		}
	}
}

// A fresh load reads and quantizes a bounded batch even when more than 500 leaves are searchable.
func TestFirstLoadBoundsVectorBatch(t *testing.T) {
	e, _ := newEngine(t, false)
	e.Settings.Recipe = leaves.Recipe{Size: 1, Split: leaves.SplitFixed, WithPrefix: true}
	const n = 1001
	var obsBatch []corpus.Observation
	for i := 0; i < n; i++ {
		obsBatch = append(obsBatch, obs("test:batch", i, "speaker", fmt.Sprintf("long enough message %d", i), time.Now().Add(-48*time.Hour).Add(time.Duration(i)*time.Minute)))
	}
	if _, err := e.Store.InsertObservations(obsBatch, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Store.SyncLeaves("test:batch", e.Settings.Recipe, time.Now(), nil); err != nil {
		t.Fatal(err)
	}
	raw := search.EncodeFloat16([]float32{1, 0, 0, 0})
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_leaves SET embed_space=?, leaf_vec=?`, e.EmbedSpace(), raw); err != nil {
		t.Fatal(err)
	}
	info, err := e.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if info["leaves"] != n || info["vector_fetch_peak"] != 500 {
		t.Fatalf("batch accounting: %v", info)
	}
	if info, err = e.Reload(); err != nil || info["vector_fetch_peak"] != 0 {
		t.Fatalf("unchanged build fetched blobs: %v %v", info, err)
	}
}

func TestSharedAndContiguousScoresAreIdentical(t *testing.T) {
	// The engine layout and the original scorer must use exactly the same operations, including ties.
	const dims = 1024
	a := search.NewQuantized(dims, 9)
	b := search.NewSharedQuantized(dims, 9)
	for i := 0; i < 9; i++ {
		v := make([]float32, dims)
		for j := range v {
			v[j] = float32((i+j*7)%29-14) / 127
		}
		a.AppendQuantized(v)
		b.AppendQuantized(v)
	}
	q := make([]float32, dims)
	for j := range q {
		q[j] = float32(j%11-5) / 31
	}
	for i := 0; i < a.Rows(); i++ {
		if math.Float64bits(a.Dot(i, q)) != math.Float64bits(b.Dot(i, q)) {
			t.Fatal("score bits changed")
		}
	}
}
