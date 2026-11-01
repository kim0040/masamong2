package search

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/retrieval"
)

// tinyIndex builds an index of two-message leaves. leafVecs has one row per leaf; msgVecs one row per
// message in message order (message 2i and 2i+1 belong to leaf i). Nothing in the text matches a
// question, so the lexical side stays empty and the leaf ranking is the dense one.
func tinyIndex(t *testing.T, room string, leafVecs, msgVecs [][]float32) *Index {
	t.Helper()
	var msgs []corpus.Message
	for i := range msgVecs {
		at := time.Date(2026, 9, 20, 10, i, 0, 0, time.UTC)
		msgs = append(msgs, corpus.Message{ObservationID: fmt.Sprintf("%s-o%02d", room, i), Room: room, RoomOriginal: room,
			ActorLabel: "speaker", ActorAccount: "a", Content: fmt.Sprintf("zz%02d", i), ParsedTime: at.Format(time.RFC3339)})
	}
	lv := leaves.SlicePerRoom(msgs, leaves.Recipe{Size: 2, Split: leaves.SplitFixed, WithPrefix: true}, 1000, "leaf")
	if len(lv) != len(leafVecs) {
		t.Fatalf("%d leaves for %d leaf vectors", len(lv), len(leafVecs))
	}
	lm := Matrix{Dims: len(leafVecs[0])}
	for _, v := range leafVecs {
		lm.Data = append(lm.Data, v...)
	}
	cfg := Default()
	ix, err := Build(cfg, msgs, lv, lm, nil, Matrix{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ix.Close)
	if msgVecs[0] != nil {
		mm := Matrix{Dims: len(msgVecs[0])}
		var rowOf, leafOf []int32
		for i, v := range msgVecs {
			mm.Data = append(mm.Data, v...)
			rowOf = append(rowOf, int32(i))
			leafOf = append(leafOf, int32(i/2))
		}
		if err := ix.SetMessageVectors(mm, rowOf, leafOf); err != nil {
			t.Fatal(err)
		}
	}
	return ix
}

func uv(v ...float32) []float32 { Normalize(v); return v }

// Leaf vectors rank L0 > L1 > L2 > L3 for the question (1,0); the single messages say L3 holds the best
// match and L2 the next one. With weight 1 the final order is worked out by hand from the RRF formula
// 1/(60+rank): L3 = 1/64+1/61, L2 = 1/63+1/62, L0 = 1/61, L1 = 1/62.
func TestMessageRunFusesAfterTheLeafFusion(t *testing.T) {
	leafVecs := [][]float32{uv(1, 0), uv(9, 4), uv(8, 6), uv(5, 9)}
	msgVecs := [][]float32{
		uv(2, 9), uv(1, 9), // L0: poor matches
		uv(1, 9), uv(2, 9), // L1: poor
		uv(7, 8), uv(3, 9), // L2: best 7,8 (cos .66)
		uv(9, 3), uv(1, 9), // L3: best 9,3 (cos .95)
	}
	ix := tinyIndex(t, "r", leafVecs, msgVecs)
	ids := func() []string {
		out := make([]string, len(ix.Leaves))
		for i, l := range ix.Leaves {
			out[i] = l.DocID
		}
		return out
	}()
	q := []float32{1, 0}
	gen := func(on bool, limit int, scale float64) genResult {
		ix.Cfg.Retrieval.MessageDense = MessageDenseConfig{Enabled: &on, Limit: limit, RRFWeight: 1}
		return ix.generate(context.Background(), nil, "unrelated words", q, Intent{}, nil, false, 100, 200, 150, 1.0, scale, nil)
	}
	off := gen(false, 0, 1)
	if got := off.fusion.IDs(); !reflect.DeepEqual(got, ids) {
		t.Fatalf("dense order without message vectors %v, want %v", got, ids)
	}
	if _, has := off.trace["message_dense"]; has {
		t.Fatal("trace carries message_dense while the setting is off")
	}

	on := gen(true, 2, 1)
	want := []string{ids[3], ids[2], ids[0], ids[1]}
	if got := on.fusion.IDs(); !reflect.DeepEqual(got, want) {
		t.Fatalf("fused order %v, want %v", got, want)
	}
	c := on.fusion.ByID(ids[3])
	if c.Sources["message_dense"] != 1 || c.Sources["leaf_dense"] != 4 {
		t.Fatalf("provenance of the winner: %v", c.Sources)
	}
	if want := 1.0/64 + 1.0/61; math.Abs(c.Score-want) > 1e-12 {
		t.Fatalf("score %g, want %g", c.Score, want)
	}
	if math.Abs(c.Raw["message_dense"]-0.9487) > 0.01 {
		t.Fatalf("raw message cosine %g", c.Raw["message_dense"])
	}
	if on.fusion.ByID(ids[0]).Sources["message_dense"] != 0 {
		t.Fatal("a leaf outside the message run is credited to it")
	}
	if n := on.fusion.SourceCounts()["message_dense"]; n != 2 {
		t.Fatalf("run length %d, want the limit 2", n)
	}
	md, ok := on.trace["message_dense"].(map[string]any)
	if !ok || md["messages_scored"] != 8 || md["leaves"] != 2 || md["dims"] != 2 {
		t.Fatalf("trace %v", on.trace["message_dense"])
	}
	if _, ok := md["ms"]; !ok {
		t.Fatal("trace lacks ms")
	}
	if !contains(on.trace["retrievers_used"].([]string), "message_dense") {
		t.Fatalf("retrievers_used %v", on.trace["retrievers_used"])
	}

	// recovery runs the same source with the dense scale: 2 leaves × 1.5 = 3
	rec := gen(true, 2, 1.5)
	if n := rec.fusion.SourceCounts()["message_dense"]; n != 3 {
		t.Fatalf("recovery run length %d, want 3", n)
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestMessageRunWeightMovesTheFinalOrder(t *testing.T) {
	leafVecs := [][]float32{uv(1, 0), uv(9, 4), uv(8, 6), uv(5, 9)}
	msgVecs := [][]float32{uv(2, 9), uv(1, 9), uv(1, 9), uv(2, 9), uv(7, 8), uv(3, 9), uv(9, 3), uv(1, 9)}
	ix := tinyIndex(t, "r", leafVecs, msgVecs)
	on := true
	ix.Cfg.Retrieval.MessageDense = MessageDenseConfig{Enabled: &on, Limit: 4, RRFWeight: 4}
	g := ix.generate(context.Background(), nil, "unrelated words", []float32{1, 0}, Intent{}, nil, false, 100, 200, 150, 1.0, 1.0, nil)
	// message order is L3, L2, L1?, L0? by best cosine: L3 .95, L2 .66, then the poor leaves
	got := g.fusion.IDs()
	if got[0] != ix.Leaves[3].DocID || got[1] != ix.Leaves[2].DocID {
		t.Fatalf("a heavy message run leads the order: %v", got)
	}
}

// The question is cut to the message dimensionality and normalised again; scoring it as-is would shrink
// every cosine by the length that was cut away.
func TestMessageQueryIsTruncatedAndRenormalised(t *testing.T) {
	ix := tinyIndex(t, "r", [][]float32{uv(1, 0, 0, 0)}, [][]float32{uv(1, 0), uv(0, 1)})
	ids, scores, scored := ix.messageDense([]float32{0.6, 0, 0.8, 0}, nil, 10)
	if scored != 2 || len(ids) != 1 {
		t.Fatalf("scored %d leaves %v", scored, ids)
	}
	if got := scores[ids[0]]; math.Abs(got-1) > 1e-6 {
		t.Fatalf("cosine %g: the first two components (0.6, 0) must be normalised to (1, 0) before scoring", got)
	}
	// a question shorter than the message vectors cannot be scored (another embedding space)
	if ids, _, n := ix.messageDense([]float32{1}, nil, 10); ids != nil || n != 0 {
		t.Fatalf("short question scored: %v %d", ids, n)
	}
}

// A leaf is ranked by its best message, ties go to the earlier message row, and only allowed leaves are
// scored at all.
func TestMessageDenseRanksLeavesByBestMessageWithinScope(t *testing.T) {
	leafVecs := [][]float32{uv(1, 0), uv(1, 0), uv(1, 0)}
	msgVecs := [][]float32{
		uv(1, 1), uv(3, 1), // L0 best .95 (second row)
		uv(3, 1), uv(1, 1), // L1 best .95 too, tie with L0 → L0's row comes first
		uv(0, 1), uv(1, 0), // L2 best 1.0
	}
	ix := tinyIndex(t, "r", leafVecs, msgVecs)
	L := func(i int) string { return ix.Leaves[i].DocID }
	ids, scores, scored := ix.messageDense([]float32{1, 0}, nil, 10)
	if want := []string{L(2), L(0), L(1)}; !reflect.DeepEqual(ids, want) || scored != 6 {
		t.Fatalf("order %v (scored %d), want %v", ids, scored, want)
	}
	if math.Abs(scores[L(2)]-1) > 1e-6 || math.Abs(scores[L(0)]-scores[L(1)]) > 1e-9 {
		t.Fatalf("scores %v", scores)
	}
	ids, _, scored = ix.messageDense([]float32{1, 0}, Set{L(0): {}, L(1): {}}, 10)
	if want := []string{L(0), L(1)}; !reflect.DeepEqual(ids, want) || scored != 4 {
		t.Fatalf("scoped order %v (scored %d), want %v", ids, scored, want)
	}
	if ids, _, scored = ix.messageDense([]float32{1, 0}, Set{}, 10); ids != nil || scored != 0 {
		t.Fatalf("an empty scope scored %d", scored)
	}
	if ids, _, _ = ix.messageDense([]float32{1, 0}, nil, 2); len(ids) != 2 {
		t.Fatalf("limit: %v", ids)
	}
}

func TestSetMessageVectorsRejectsMismatches(t *testing.T) {
	ix := tinyIndex(t, "r", [][]float32{uv(1, 0)}, [][]float32{nil, nil})
	m := Matrix{Dims: 2, Data: []float32{1, 0, 0, 1}}
	if err := ix.SetMessageVectors(m, []int32{0}, []int32{0, 0}); err == nil {
		t.Fatal("pair count mismatch accepted")
	}
	if err := ix.SetMessageVectors(m, []int32{0, 1}, []int32{0, 5}); err == nil {
		t.Fatal("a leaf position outside the leaves accepted")
	}
	if err := ix.SetMessageVectors(m, []int32{0, 2}, []int32{0, 0}); err == nil {
		t.Fatal("a row outside the matrix accepted")
	}
	if ix.HasMessageVectors() {
		t.Fatal("a rejected attach left vectors behind")
	}
	if err := ix.SetMessageVectors(m, []int32{0, 1}, []int32{0, 0}); err != nil || !ix.HasMessageVectors() {
		t.Fatalf("%v", err)
	}
}

func TestMessageDenseIsADenseFamily(t *testing.T) {
	if got := SourceFamily("message_dense"); got != "dense" {
		t.Fatalf("family %q", got)
	}
	// a leaf found by lexical search and by the message run reads as two families; one found by the
	// dense leaf run and the message run reads as one, so the run cannot inflate agreement by itself
	c := &Candidate{Sources: map[string]int{"leaf_dense": 1, "message_dense": 2}}
	if len(c.Families()) != 1 {
		t.Fatalf("families %v", c.Families())
	}
}

func cands(ids ...string) []*Candidate {
	out := make([]*Candidate, len(ids))
	for i, id := range ids {
		out[i] = &Candidate{DocID: id, Rank: i + 1}
	}
	return out
}

func seq(prefix string, from, to int) []string {
	var out []string
	for i := from; i < to; i++ {
		out = append(out, fmt.Sprintf("%s%02d", prefix, i))
	}
	return out
}

// The message run claims reserve slots last: leaf and lexical candidates (and the other runs) keep every
// slot they would have had without it.
func TestGuaranteePoolFillsMessageRunLast(t *testing.T) {
	order := seq("o", 0, 24)
	t.Run("a full reserve leaves nothing for the message run", func(t *testing.T) {
		// pool 10, 20% reserve = 2 slots, base = o00..o07
		f := &Fusion{Ordered: cands(order...), Runs: map[string][]string{
			"leaf_dense": {"o00", "o08", "o12"}, "leaf_lexical": {"o01", "o09", "o13"}, "message_dense": {"o20", "o21", "o22"}}}
		pool, extras, by := guaranteePool(f, 10, 3, 0.2)
		if !reflect.DeepEqual(extras, []string{"o08", "o09"}) || len(by["message_dense"]) != 0 {
			t.Fatalf("extras %v by %v", extras, by)
		}
		delete(f.Runs, "message_dense")
		pool2, extras2, _ := guaranteePool(f, 10, 3, 0.2)
		if !reflect.DeepEqual(pool, pool2) || !reflect.DeepEqual(extras, extras2) {
			t.Fatalf("the pool changed by adding the message run: %v vs %v", pool, pool2)
		}
	})
	t.Run("it takes what is left after runs that sort after its name", func(t *testing.T) {
		// pool 10, 40% reserve = 4 slots, base = o00..o05. "q1_dense" sorts after "message_dense", yet its
		// slots are claimed first.
		f := &Fusion{Ordered: cands(order...), Runs: map[string][]string{
			"leaf_dense": {"o00", "o06"}, "leaf_lexical": {"o01", "o07"}, "q1_dense": {"o18", "o19"}, "message_dense": {"o20", "o21", "o22"}}}
		_, extras, by := guaranteePool(f, 10, 3, 0.4)
		if want := []string{"o18", "o06", "o07", "o19"}; !reflect.DeepEqual(extras, want) {
			t.Fatalf("extras %v, want %v", extras, want)
		}
		if len(by["message_dense"]) != 0 {
			t.Fatalf("message run took %v although the reserve was full", by["message_dense"])
		}
	})
	t.Run("a reserve with room gives it the rest", func(t *testing.T) {
		// 50% reserve = 5 slots over a base of o00..o04
		f := &Fusion{Ordered: cands(order...), Runs: map[string][]string{
			"leaf_dense": {"o00", "o05"}, "leaf_lexical": {"o01", "o06"}, "message_dense": {"o20", "o21", "o22"}}}
		_, extras, by := guaranteePool(f, 10, 3, 0.5)
		if want := []string{"o05", "o06", "o20", "o21", "o22"}; !reflect.DeepEqual(extras, want) {
			t.Fatalf("extras %v, want %v", extras, want)
		}
		if !reflect.DeepEqual(by["message_dense"], []string{"o20", "o21", "o22"}) {
			t.Fatalf("by source %v", by)
		}
	})
}

// With the setting off — the default — a loaded message index changes nothing: the hits and the whole
// trace equal those of an index that never had message vectors, and nothing named message_dense appears.
// An enabled setting with no vectors loaded is also inert.
func TestMessageDenseOffIsIdentical(t *testing.T) {
	leafVecs := [][]float32{uv(1, 0), uv(9, 4), uv(8, 6), uv(5, 9)}
	msgVecs := [][]float32{uv(2, 9), uv(1, 9), uv(1, 9), uv(2, 9), uv(7, 8), uv(3, 9), uv(9, 3), uv(1, 9)}
	run := func(ix *Index, ranking retrieval.RankingMode) []byte {
		res, err := ix.Retrieve(context.Background(), Request{Query: "zz05 zz06", QueryVector: []float32{1, 0}, Ranking: ranking,
			Scope: Scope{Rooms: []string{"r"}}, TopK: 3}, nil, nil)
		if err != nil || len(res.Hits) == 0 {
			t.Fatalf("%v %+v", err, res)
		}
		raw, err := json.Marshal(res)
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	for _, ranking := range []retrieval.RankingMode{retrieval.Hybrid, retrieval.Auto} {
		plain := tinyIndex(t, "r", leafVecs, [][]float32{nil, nil, nil, nil, nil, nil, nil, nil})
		want := run(plain, ranking)

		loaded := tinyIndex(t, "r", leafVecs, msgVecs) // vectors attached, setting left at its default
		if got := run(loaded, ranking); string(got) != string(want) {
			t.Fatalf("%s: message vectors loaded with the setting off changed the output:\n%s\n%s", ranking, want, got)
		}
		off := false
		loaded.Cfg.Retrieval.MessageDense.Enabled = &off
		if got := run(loaded, ranking); string(got) != string(want) {
			t.Fatalf("%s: explicit false changed the output", ranking)
		}
		on := true
		plain.Cfg.Retrieval.MessageDense.Enabled = &on // enabled, but nothing is loaded
		if got := run(plain, ranking); string(got) != string(want) {
			t.Fatalf("%s: enabled without vectors changed the output", ranking)
		}
		if strings.Contains(string(want), "message_dense") {
			t.Fatalf("%s: the default output mentions message_dense: %s", ranking, want)
		}
	}
	// the setting is invisible in a default config's JSON, so stored configs read and write as before
	raw, _ := json.Marshal(Default())
	if strings.Contains(string(raw), "message_dense") {
		t.Fatalf("default config JSON carries message_dense: %s", raw)
	}
	var cfg Config
	if err := json.Unmarshal([]byte(`{"retrieval":{"message_dense":{"enabled":true,"limit":50,"rrf_weight":2}}}`), &cfg); err != nil {
		t.Fatal(err)
	}
	if !cfg.messageDenseOn() || cfg.messageDenseLimit() != 50 || cfg.messageDenseWeight() != 2 {
		t.Fatalf("parsed %+v", cfg.Retrieval.MessageDense)
	}
	// measured on real data: 1.0 hurt span (flow) questions without a reranker, 0.1 did not
	d := Default()
	if d.messageDenseOn() || d.messageDenseLimit() != 200 || d.messageDenseWeight() != 0.1 || DefaultMessageDenseWeight != 0.1 {
		t.Fatalf("defaults %+v", d.Retrieval.MessageDense)
	}
}

// Turned on, a retrieve brings the leaf only a single message reveals into the pool and traces the run.
func TestRetrieveWithMessageDenseSurfacesTheBuriedLeaf(t *testing.T) {
	// 12 leaves whose averaged vectors point away from the question; leaf 9 holds one matching message
	var leafVecs, msgVecs [][]float32
	for i := 0; i < 12; i++ {
		leafVecs = append(leafVecs, uv(1, float32(i+1)*0.5))
		msgVecs = append(msgVecs, uv(0.2, 1), uv(0.1, 1))
	}
	msgVecs[18] = uv(1, 0.05) // leaf 9, first message
	ix := tinyIndex(t, "r", leafVecs, msgVecs)
	req := Request{Query: "unrelated words", QueryVector: []float32{1, 0}, Ranking: retrieval.Hybrid, Scope: Scope{Rooms: []string{"r"}}, TopK: 1}
	buried := ix.Leaves[9].DocID
	pos := func(res Result) int {
		for i, id := range res.Trace["fusion_ids"].([]string) {
			if id == buried {
				return i
			}
		}
		return -1
	}
	base, err := ix.Retrieve(context.Background(), req, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	on := true
	ix.Cfg.Retrieval.MessageDense = MessageDenseConfig{Enabled: &on}
	got, err := ix.Retrieve(context.Background(), req, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if pos(got) < 0 || (pos(base) >= 0 && pos(got) >= pos(base)) {
		t.Fatalf("buried leaf at %d with message vectors, %d without", pos(got), pos(base))
	}
	if _, ok := got.Trace["message_dense"]; !ok {
		t.Fatal("no message_dense trace")
	}
	if md := got.Trace["message_dense"].(map[string]any); md["messages_scored"] != 24 {
		t.Fatalf("trace %v", md)
	}
}

// A row kept in the database (EncodeQuantizedRow) is exactly the row the in-memory matrix holds
// (AppendQuantized), so the index loads it without a second rounding.
func TestQuantizedRowRoundTripEqualsTheInMemoryQuantisation(t *testing.T) {
	rng := []float32{0.31, -0.5, 0.07, 0, 0.9, -0.9, 0.0001, 0.123456, -0.7071}
	for _, v := range [][]float32{rng, {0, 0, 0, 0}, {1}, {-0.25, 0.25}, uv(3, 1, 4, 1, 5, 9, 2, 6)} {
		m := NewQuantized(len(v), 1)
		m.AppendQuantized(v)
		wantQ, wantScale := m.RowQ(0)
		raw := EncodeQuantizedRow(v)
		if len(raw) != len(v)+4 {
			t.Fatalf("%d bytes for %d components", len(raw), len(v))
		}
		q, scale, ok := DecodeQuantizedRow(raw, nil)
		if !ok || scale != wantScale || !reflect.DeepEqual(q, wantQ) {
			t.Fatalf("round trip %v: q %v scale %v, want %v %v", v, q, scale, wantQ, wantScale)
		}
		// decoding into a reused buffer gives the same row
		buf := make([]int8, 64)
		if q2, s2, ok := DecodeQuantizedRow(raw, buf); !ok || s2 != wantScale || !reflect.DeepEqual(q2, wantQ) {
			t.Fatalf("reused buffer: %v %v", q2, s2)
		}
	}
	if _, _, ok := DecodeQuantizedRow([]byte{1, 2, 3}, nil); ok {
		t.Fatal("a row without room for a scale decoded")
	}
}
