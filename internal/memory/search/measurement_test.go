package search

import (
	"encoding/json"
	"reflect"
	"testing"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/retrieval"
)

func measurementFixture() ([]corpus.Message, []leaves.Leaf, Matrix) {
	msgs := []corpus.Message{
		{ObservationID: "m0", Room: "discord:test:A", RoomOriginal: "discord:test:A", ActorLabel: "합성원명", ActorAccount: " account ", Content: "합성 캠핑 텐트", ParsedTime: "2025-01-01T12:00:00Z"},
		{ObservationID: "m1", Room: "discord:test:A", RoomOriginal: "discord:test:A", ActorLabel: "", ActorAccount: "", Content: "합성 캠핑 계획", ParsedTime: "2025-01-01T12:01:00Z"},
		{ObservationID: "m2", Room: "discord:test:B", RoomOriginal: "discord:test:B", ActorLabel: "합성별칭", ActorAccount: "account", Content: "합성 일정", ParsedTime: "2025-01-02T12:00:00Z"},
		{ObservationID: "m3", Room: "kakao:test:C", RoomOriginal: "kakao:test:C", ActorLabel: "합성별칭", ActorAccount: "account", Content: "합성 책", ParsedTime: "2025-01-03T12:00:00Z"},
		// A raw/open-tail label must never enter identity without an eligible leaf.
		{ObservationID: "m4", Room: "discord:test:D", ActorLabel: "제외별칭", ActorAccount: "account", Content: "합성 열린 꼬리"},
	}
	lv := []leaves.Leaf{
		{DocID: "a", Room: msgs[0].Room, Start: 0, End: 2, Body: "합성원명 캠핑 텐트", ObservationIDs: []string{"m0", "m1"}, Speakers: []string{"합성원명", "?"}},
		{DocID: "b", Room: msgs[2].Room, Start: 2, End: 3, Body: "합성별칭 일정", ObservationIDs: []string{"m2"}, Speakers: []string{"합성별칭"}},
		{DocID: "c", Room: msgs[3].Room, Start: 3, End: 4, Body: "합성별칭 책", ObservationIDs: []string{"m3"}, Speakers: []string{"합성별칭"}},
	}
	return msgs, lv, Matrix{Dims: 2, Data: []float32{1, 0, 0, 1, 0.5, 0.5}}
}

// Map equality deliberately checks sets, not serialized ordering.
func TestMeasurementIdentitySetEquivalence(t *testing.T) {
	msgs, lv, vecs := measurementFixture()
	cfg := Default()
	old, err := Build(cfg, msgs, lv, vecs, nil, Matrix{})
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	inputs := NewIdentityInputs(msgs, lv)
	full, err := BuildWithIdentity(cfg, msgs, lv, vecs, nil, Matrix{}, inputs)
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	old.lazy()
	full.lazy()
	if !reflect.DeepEqual(old.identity, full.identity) || !reflect.DeepEqual(old.aliasTable(), full.aliasTable()) {
		t.Fatal("full identity sets differ from Build")
	}
	part, err := BuildWithIdentity(cfg, msgs[:2], lv[:1], Matrix{Dims: 2, Data: vecs.Data[:2]}, nil, Matrix{}, inputs)
	if err != nil {
		t.Fatal(err)
	}
	defer part.Close()
	part.lazy()
	if !reflect.DeepEqual(old.identity.labelsByKey, part.identity.labelsByKey) || !reflect.DeepEqual(old.identity.keysByLabel, part.identity.keysByLabel) || !reflect.DeepEqual(old.aliasTable(), part.aliasTable()) {
		t.Fatal("global identity/aliases changed with residency")
	}
	for _, ids := range part.identity.leavesByKey {
		for id := range ids {
			if id != "a" {
				t.Fatal("nonresident leaf retained in identity")
			}
		}
	}
	if _, ok := part.identity.keysByLabel["제외별칭"]; ok {
		t.Fatal("open tail became an identity input")
	}
	if len(inputs.table.labelsByKey) != 3 {
		t.Fatal("platform/account normalization changed")
	}
}

func TestMeasurementSyntheticParityAndDetachedHook(t *testing.T) {
	msgs, lv, vecs := measurementFixture()
	inputs := NewIdentityInputs(msgs, lv)
	old, err := Build(Default(), msgs, lv, vecs, nil, Matrix{})
	if err != nil {
		t.Fatal(err)
	}
	defer old.Close()
	full, err := BuildWithIdentity(Default(), msgs, lv, vecs, nil, Matrix{}, inputs)
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	for _, mode := range []retrieval.RankingMode{retrieval.Hybrid, retrieval.Auto, retrieval.Rerank} {
		req := Request{Query: "합성별칭 캠핑", QueryVector: []float32{1, 0}, Intent: Intent{Targets: []string{"합성별칭"}}, Scope: Scope{Rooms: []string{msgs[0].Room}}, Ranking: mode}
		a, err := old.Retrieve(t.Context(), req, nil, fakeRerank{true})
		if err != nil {
			t.Fatal(err)
		}
		b, err := full.Retrieve(t.Context(), req, nil, fakeRerank{true})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatal("synthetic full identity parity failed")
		}
		stages := 0
		req.OnCandidatesForMeasurement = func(m CandidateMeasurement) {
			stages++
			if len(m.Candidates) > 0 {
				m.Candidates[0].DocID = "corrupt"
				m.Candidates[0].Sources["corrupt"] = 999
			}
			for k, ids := range m.Runs {
				if len(ids) > 0 {
					ids[0] = "corrupt"
				}
				delete(m.Runs, k)
			}
			if len(m.Pool) > 0 {
				m.Pool[0] = "corrupt"
			}
		}
		c, err := old.Retrieve(t.Context(), req, nil, fakeRerank{true})
		if err != nil {
			t.Fatal(err)
		}
		aa, _ := json.Marshal(a)
		cc, _ := json.Marshal(c)
		if string(aa) != string(cc) {
			t.Fatal("measurement hook changed search/Trace bytes")
		}
		if stages < 1 || (mode == retrieval.Auto && stages != 2) {
			t.Fatalf("missing auto recovery snapshots: %d", stages)
		}
	}
}

func TestMeasurementResidencySequence(t *testing.T) {
	msgs, lv, vecs := measurementFixture()
	inputs := NewIdentityInputs(msgs, lv)
	var before Result
	for i, n := range []int{2, 3, 2} {
		ix, err := BuildWithIdentity(Default(), msgs[:lv[n-1].End], lv[:n], Matrix{Dims: 2, Data: vecs.Data[:n*2]}, nil, Matrix{}, inputs)
		if err != nil {
			t.Fatal(err)
		}
		got, err := ix.Retrieve(t.Context(), Request{Query: "합성 캠핑", QueryVector: []float32{1, 0}, Scope: Scope{Rooms: []string{msgs[0].Room}}, Ranking: retrieval.Hybrid}, nil, nil)
		ix.Close()
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			before = got
		}
		if i == 2 && !reflect.DeepEqual(before, got) {
			t.Fatal("A/B -> A/B/C -> A/B changed A results")
		}
	}
}

func TestMeasurementSandboxPartialLoaderParity(t *testing.T) {
	msgs, lv, vecs := measurementFixture()
	full, err := buildSandboxWithRooms(Default(), msgs, lv, vecs, nil, Matrix{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer full.Close()
	all, err := buildSandboxWithRooms(Default(), msgs, lv, vecs, nil, Matrix{}, []string{msgs[0].Room, msgs[2].Room, msgs[3].Room, msgs[4].Room})
	if err != nil {
		t.Fatal(err)
	}
	defer all.Close()
	full.lazy()
	all.lazy()
	if !reflect.DeepEqual(full.identity, all.identity) || !reflect.DeepEqual(full.Leaves, all.Leaves) || !reflect.DeepEqual(full.Messages, all.Messages) {
		t.Fatal("sandbox full-set filter differs")
	}
	part, err := buildSandboxWithRooms(Default(), msgs, lv, vecs, nil, Matrix{}, []string{msgs[0].Room})
	if err != nil {
		t.Fatal(err)
	}
	defer part.Close()
	if len(part.Messages) != 2 || len(part.Leaves) != 1 || !reflect.DeepEqual(full.aliasTable(), part.aliasTable()) {
		t.Fatal("sandbox partial loader lost global identity or retained other rooms")
	}
	manual, err := BuildWithIdentity(Default(), msgs[:2], lv[:1], Matrix{Dims: 2, Data: vecs.Data[:2]}, nil, Matrix{}, NewIdentityInputs(msgs, lv))
	if err != nil {
		t.Fatal(err)
	}
	defer manual.Close()
	req := Request{Query: "합성별칭 캠핑", QueryVector: []float32{1, 0}, Scope: Scope{All: true}, Intent: Intent{Targets: []string{"합성별칭"}}, Ranking: retrieval.Hybrid}
	a, err := part.Retrieve(t.Context(), req, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, err := manual.Retrieve(t.Context(), req, nil, nil)
	if err != nil || !reflect.DeepEqual(a, b) {
		t.Fatal("synthetic partial sandbox loader parity failed")
	}
}
