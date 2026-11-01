package engine

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
)

func TestResidentMeasurementGlobalEligibilityAndFullFilter(t *testing.T) {
	e, _ := newEngine(t, false)
	t.Cleanup(e.Close)
	e.Models.EmbedDims = 16
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	rooms := []string{"discord:gate:A", "discord:gate:B", "discord:gate:C", "discord:gate:D"}
	var batch []corpus.Observation
	for ri, room := range rooms {
		for i := 0; i < 16; i++ {
			label := []string{"합성원명", "제외너비", "합성별칭", "제외미임베딩"}[ri]
			o := obs(room, i, label, "합성 캠핑 계획", base.Add(time.Duration(i)*time.Minute))
			o.ActorAccount = " same-account "
			batch = append(batch, o)
		}
	}
	if _, err := e.Store.InsertObservations(batch, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	// Each distinct ineligible case must disappear from global identity as well
	// as full Build: wrong width, unembedded/open, recipe/space, bad placement.
	for _, mutation := range []struct {
		sql  string
		args []any
	}{
		{`UPDATE memory_leaves SET leaf_vec=? WHERE room=?`, []any{search.EncodeFloat16([]float32{1, 0}), rooms[1]}},
		{`UPDATE memory_leaves SET leaf_vec=NULL,closed=0 WHERE room=?`, []any{rooms[3]}},
	} {
		if _, err := e.Store.DB.DB.Exec(mutation.sql, mutation.args...); err != nil {
			t.Fatal(err)
		}
	}
	var second string
	if err := e.Store.DB.DB.QueryRow(`SELECT leaf_id FROM memory_leaves WHERE room=? ORDER BY end_time DESC LIMIT 1`, rooms[2]).Scan(&second); err != nil {
		t.Fatal(err)
	}
	ids, _ := json.Marshal([]string{"missing-first", "missing-last"})
	if _, err := e.Store.DB.DB.Exec(`UPDATE memory_leaves SET observation_ids=? WHERE leaf_id=?`, string(ids), second); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Reload(); err != nil {
		t.Fatal(err)
	}
	full := e.Index()
	with := New(e.Store, nil, e.Models, e.Settings)
	t.Cleanup(with.Close)
	with.Settings.ResidentRoomsForMeasurement = rooms
	info, err := with.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if info["identity_input_rows"] != 64 || !reflect.DeepEqual(full.Leaves, with.Index().Leaves) || !reflect.DeepEqual(full.Messages, with.Index().Messages) {
		t.Fatal("full filter differs or omitted global ordering inputs")
	}
	for _, target := range []string{"합성별칭", "제외너비", "제외미임베딩"} {
		req := search.Request{Query: target + " 캠핑", QueryVector: vecFor("캠핑"), Intent: search.Intent{Targets: []string{target}}, Scope: search.Scope{Rooms: rooms[:1]}, Ranking: retrieval.Hybrid}
		a, err := full.Retrieve(t.Context(), req, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		b, err := with.Index().Retrieve(t.Context(), req, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, b) {
			t.Fatal("full residency identity or search changed")
		}
		with.Settings.ResidentRoomsForMeasurement = rooms[:1]
		if _, err := with.Reload(); err != nil {
			t.Fatal(err)
		}
		c, err := with.Index().Retrieve(t.Context(), req, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(a, c) {
			t.Fatal("out-of-scope alias eligibility differs under partial residency")
		}
		for _, m := range with.Index().Messages {
			if m.Room != rooms[0] {
				t.Fatal("nonresident message retained")
			}
		}
		if with.Index().LeafVecs.Dims != 16 {
			t.Fatal("partial subset selected its own dimensionality")
		}
		with.Settings.ResidentRoomsForMeasurement = rooms
		if _, err := with.Reload(); err != nil {
			t.Fatal(err)
		}
	}
	// B's rows use an incompatible width: the global first row still fixes 16,
	// and filtering to B must not resurrect the 2-dimensional rows.
	with.Settings.ResidentRoomsForMeasurement = rooms[1:2]
	if _, err := with.Reload(); err != nil {
		t.Fatal(err)
	}
	if len(with.Index().Leaves) != 0 {
		t.Fatal("nonresident first leaf did not fix global dimensions")
	}
	with.Settings.ResidentRoomsForMeasurement = []string{}
	if _, err := with.Reload(); err != nil {
		t.Fatal(err)
	}
	if len(with.Index().Messages) != 0 || len(with.Index().RoomSigs) != 0 {
		t.Fatal("empty filter interpreted as all rooms")
	}
}

func TestPlaceLeafMetaUsesCanonicalGlobalOrdering(t *testing.T) {
	// Merged rooms use original-room and observation ID as tie breakers, exactly
	// as LoadMessages/LoadIdentityInputs; content is irrelevant to ordering.
	e, _ := newEngine(t, false)
	t.Cleanup(e.Close)
	seed(t, e)
	rooms, err := e.Store.Rooms()
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range rooms {
		names = append(names, r.Room.Room)
	}
	full, err := e.Store.LoadMessages(names, 0)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := e.Store.LoadIdentityInputs(names)
	if err != nil {
		t.Fatal(err)
	}
	for i, m := range full {
		m.Content = ""
		if m != inputs[i] {
			t.Fatal("identity inputs changed ordering/native keys or retained bodies")
		}
	}
}
