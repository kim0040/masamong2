package search

import (
	"context"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/retrieval"
)

func synthIndex(t *testing.T) *Index {
	t.Helper()
	var msgs []corpus.Message
	add := func(room string, day, i int, who, text string) {
		at := time.Date(2026, 9, day, 10, i, 0, 0, time.UTC)
		msgs = append(msgs, corpus.Message{ObservationID: room + "/" + at.Format(time.RFC3339), Room: room, RoomOriginal: room,
			ActorLabel: who, ActorAccount: "a-" + who, Content: text, ParsedTime: at.Format(time.RFC3339)})
	}
	for i, l := range []string{"캠핑 가자", "가평 어때", "좋아", "고기 사올게", "텐트 챙겨", "비 오면 취소", "알겠어", "그럼 토요일"} {
		add("discord:1:1", 20, i, []string{"철수", "영희"}[i%2], l)
	}
	for i, l := range []string{"점심 마라탕", "또 마라탕", "ㅋㅋ", "짬뽕 먹자"} {
		add("discord:1:1", 22, i, "민수", l)
	}
	add("discord:1:2", 21, 0, "외부", "캠핑 가평 비밀 얘기")
	msgs = corpus.OrderByRoom(msgs)
	lv := leaves.SlicePerRoom(msgs, leaves.Recipe{Size: 8, Split: leaves.SplitFixed, WithPrefix: true}, 1000, "leaf")
	vecs := Matrix{Dims: 4}
	for i := range lv {
		v := []float32{1, float32(i), 0.5, 0.1}
		Normalize(v)
		vecs.Data = append(vecs.Data, v...)
	}
	ix, err := Build(Default(), msgs, lv, vecs, nil, Matrix{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(ix.Close)
	return ix
}

func TestRetrieveRespectsRoomScopeAndPacksRawEvidence(t *testing.T) {
	ix := synthIndex(t)
	q := []float32{1, 0, 0.5, 0.1}
	res, err := ix.Retrieve(context.Background(), Request{Query: "캠핑 가평", QueryVector: q, Ranking: retrieval.Hybrid,
		Scope: Scope{Rooms: []string{"discord:1:1"}}, TopK: 3}, nil, nil)
	if err != nil || len(res.Hits) == 0 {
		t.Fatalf("%v %+v", err, res)
	}
	for _, h := range res.Hits {
		if h.Room != "discord:1:1" {
			t.Fatalf("other room leaked: %s", h.Room)
		}
		for _, sp := range h.VisibleSpans {
			ex := []rune(h.Excerpt)
			if sp.ExcerptEnd > len(ex) || sp.ExcerptStart < 0 {
				t.Fatalf("span outside excerpt: %+v", sp)
			}
		}
	}
	none, _ := ix.Retrieve(context.Background(), Request{Query: "캠핑", QueryVector: q, Ranking: retrieval.Hybrid, Scope: Scope{}}, nil, nil)
	if len(none.Hits) != 0 {
		t.Fatal("empty scope must return nothing")
	}
}

func TestHardTimeWindowIsScope(t *testing.T) {
	ix := synthIndex(t)
	ref := &TimeReference{Mode: "replay", Now: Moment{T: time.Date(2026, 9, 23, 9, 0, 0, 0, time.UTC), Aware: true}, TZ: time.UTC}
	it := Intent{Kind: "event", Time: &TimeSpec{Kind: "yesterday"}, Source: "planner"}
	res, _ := ix.Retrieve(context.Background(), Request{Query: "마라탕 캠핑", QueryVector: []float32{1, 0, 0, 0}, Intent: it,
		Ranking: retrieval.Hybrid, Scope: Scope{Rooms: []string{"discord:1:1"}}, TimeReference: ref}, nil, nil)
	for _, h := range res.Hits {
		if h.Date != "2026-09-22" {
			t.Fatalf("hit outside yesterday: %+v", h.Date)
		}
	}
	unres, _ := ix.Retrieve(context.Background(), Request{Query: "마라탕", QueryVector: []float32{1, 0, 0, 0}, Intent: it,
		Ranking: retrieval.Hybrid, Scope: Scope{Rooms: []string{"discord:1:1"}}}, nil, nil)
	if len(unres.Hits) != 0 {
		t.Fatal("relative time without a reference must not guess")
	}
}

func TestResolveTimeScopeNaiveNeedsDeclaredZone(t *testing.T) {
	ref := &TimeReference{Mode: "live", Now: Moment{T: time.Date(2026, 9, 23, 1, 0, 0, 0, time.UTC), Aware: true}, TZ: time.FixedZone("KST", 9*3600)}
	s := ResolveTimeScope(&TimeSpec{Kind: "today"}, ref, false)
	if got := s.ObservationStatus("2026-09-23T09:30:00"); got != "unresolved" {
		t.Fatalf("naive record without declared zone: %s", got)
	}
	ref.NaiveTZ = time.FixedZone("KST", 9*3600)
	s = ResolveTimeScope(&TimeSpec{Kind: "today"}, ref, false)
	if got := s.ObservationStatus("2026-09-23T09:30:00"); got != "in" {
		t.Fatalf("declared KST record: %s", got)
	}
}

func TestFloat16RoundTrip(t *testing.T) {
	for _, f := range []float32{0, 1, -1, 0.333, 1e-5, 65504, -0.000061} {
		got := Float16ToFloat32(Float32ToFloat16(f))
		if d := got - f; d > 0.001*abs32(f)+1e-7 || d < -(0.001*abs32(f)+1e-7) {
			t.Fatalf("%g → %g", f, got)
		}
	}
}

func abs32(x float32) float32 {
	if x < 0 {
		return -x
	}
	return x
}

func TestExcludedLeavesNeverBecomeHits(t *testing.T) {
	ix := synthIndex(t)
	q := []float32{1, 0, 0.5, 0.1}
	req := Request{Query: "캠핑 가평", QueryVector: q, Ranking: retrieval.Hybrid, Scope: Scope{Rooms: []string{"discord:1:1"}}, TopK: 5}
	before, err := ix.Retrieve(context.Background(), req, nil, nil)
	if err != nil || len(before.Hits) == 0 {
		t.Fatalf("%v %+v", err, before)
	}
	gone := before.Hits[0].DocID
	req.Scope.Exclude = Set{gone: {}}
	after, _ := ix.Retrieve(context.Background(), req, nil, nil)
	for _, h := range after.Hits {
		if h.DocID == gone {
			t.Fatalf("excluded leaf %s returned", gone)
		}
		for _, m := range h.MergedLeafIDs {
			if m == gone {
				t.Fatalf("excluded leaf %s merged into a hit", gone)
			}
		}
	}
}

func TestContextOnlyHitIsDatedByWhatItShows(t *testing.T) {
	msg := func(id, at string) corpus.Message { return corpus.Message{ObservationID: id, ParsedTime: at} }
	ix := &Index{Messages: []corpus.Message{
		msg("a", "2026-07-25T21:00:00"), msg("b", "2026-07-25T21:05:00"), // the matched leaf (naive Kakao time)
		msg("c", "2026-08-11T00:30:00"), msg("d", "2026-09-06T08:00:00+09:00"),
	}}
	leaf := leaves.Leaf{Start: 0, End: 2, ObservationIDs: []string{"a", "b"}}
	// its own messages were shown by an earlier hit: only c and d are visible here
	p := Packed{Start: 0, End: 4, VisibleObservations: []string{"c", "d"}}
	if got := contextOnlyDate(ix, p, leaf); got != "2026-08-11~2026-09-06" {
		t.Fatalf("context-only date %q", got)
	}
	// a naive time keeps its own date (no host zone shift)
	if got := leafDate(ix, leaf); got != "2026-07-25" {
		t.Fatalf("leaf date %q", got)
	}
	// a hit that shows one of its own messages keeps the leaf date
	p.VisibleObservations = []string{"b", "c"}
	if got := contextOnlyDate(ix, p, leaf); got != "" {
		t.Fatalf("own message visible: %q", got)
	}
	p.VisibleObservations = []string{"c"}
	if got := contextOnlyDate(ix, p, leaf); got != "2026-08-11" {
		t.Fatalf("one visible day: %q", got)
	}
}
