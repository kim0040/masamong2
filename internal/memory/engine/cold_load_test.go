package engine

import (
	"context"
	"reflect"
	"testing"

	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
)

// The reload counters make the performance contract observable without flaky
// timing assertions: the base commit does two message scans and ID BLOB lookups.
func TestColdLoadStreamsVectorsAndPreservesSearch(t *testing.T) {
	e, _ := newEngine(t, true)
	t.Cleanup(e.Close)
	e.Models.EmbedDims = 16
	e.Settings.MessageVectors = true
	on := true
	e.Settings.Search.Retrieval.MessageDense.Enabled = &on
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	// A hot reload still exercises the canonical metadata/selective-read path.
	if _, err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	want := e.Index()
	fresh := New(e.Store, nil, e.Models, e.Settings)
	t.Cleanup(fresh.Close)
	info, err := fresh.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if info["leaf_vectors_streamed"] != len(want.Leaves) || info["message_vectors_streamed"] != seededEligible {
		t.Fatalf("first load did not use one bounded full scan: %v", info)
	}
	for _, q := range []string{"캠핑", "토요일 출발", "마라탕", "없는말"} {
		req := search.Request{Query: q, QueryVector: vecFor(q), Scope: search.Scope{All: true}, Ranking: retrieval.Hybrid, TopK: 10}
		a, err := want.Retrieve(t.Context(), req, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, err := fresh.Index().Retrieve(t.Context(), req, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		// Execution time is diagnostic, not part of ranking parity.
		for _, res := range []search.Result{a, b} {
			if trace, ok := res.Trace["message_dense"].(map[string]any); ok {
				delete(trace, "ms")
			}
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatal("cold and canonical search scores/order differ")
		}
	}
	info, err = fresh.Reload()
	if err != nil || info["leaf_vectors_streamed"] != 0 || info["message_vectors_streamed"] != 0 || info["message_vectors_read"] != 0 {
		t.Fatalf("hot reload stopped reusing rows: %v %v", info, err)
	}
}

func TestColdLoadMixedMessageWidthsKeepsCanonicalSelection(t *testing.T) {
	e, _ := messageEngine(t)
	t.Cleanup(e.Close)
	e.Models.EmbedDims = 16
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	// The first observation in canonical ID order is now an unusual width.
	// An unordered cold scan must retry the old path instead of letting its
	// physical first row decide which dimensions all other rows must have.
	raw := search.EncodeQuantizedRow([]float32{1, 0})
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_message_vectors SET vec=? WHERE observation_id=(SELECT MIN(observation_id) FROM memory_message_vectors)`, raw); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	fresh := New(e.Store, nil, e.Models, e.Settings)
	t.Cleanup(fresh.Close)
	info, err := fresh.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Index().MsgVecs.Dims != e.Index().MsgVecs.Dims || fresh.Index().MsgVecs.Dims != 2 {
		t.Fatal("canonical width changed")
	}
	if !reflect.DeepEqual(fresh.Index().MessageRows, e.Index().MessageRows) || info["message_vectors_streamed"] != 0 {
		t.Fatal("mixed-width fallback changed rows")
	}
}

func TestColdLoadFallsBackForShortRowsInLegacySpace(t *testing.T) {
	e, _ := messageEngine(t)
	t.Cleanup(e.Close)
	e.Models.EmbedDims = 1024
	e.Models.EmbedTruncatable = true
	e.Settings.MessageDims = 1024 // the old binary's message width and space name
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	// Simulate 512-dimensional rows left in the old 1024-dimensional space. The cold
	// streaming path must retry through the metadata path, as errColdMessageDims intended.
	raw := search.EncodeQuantizedRow(make([]float32, 512))
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_message_vectors SET vec=? WHERE embed_space=?`, raw, e.MessageSpace()); err != nil {
		t.Fatal(err)
	}
	fresh := New(e.Store, nil, e.Models, e.Settings)
	t.Cleanup(fresh.Close)
	info, err := fresh.Reload()
	if err != nil || info["message_vectors_streamed"] != 0 || fresh.Index().MsgVecs.Dims != 512 || fresh.Index().MsgVecs.Rows() != seededEligible {
		t.Fatalf("legacy-width fallback failed: info=%v dims=%d rows=%d err=%v", info, fresh.Index().MsgVecs.Dims, fresh.Index().MsgVecs.Rows(), err)
	}
}
