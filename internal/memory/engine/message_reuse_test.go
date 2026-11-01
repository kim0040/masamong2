package engine

import (
	"context"
	"math"
	"testing"
	"time"

	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
)

func TestRebuildReusesMessageRowsAndReadsOnlyChanges(t *testing.T) {
	e, _ := messageEngine(t)
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	first := e.Index()
	info, err := e.Reload()
	if err != nil || info["message_vectors_reused"] != seededEligible || info["message_vectors_read"] != 0 {
		t.Fatalf("%v %v", info, err)
	}
	next := e.Index()
	for id, old := range first.MessageRows {
		a, sa := first.MsgVecs.RowQ(int(old.Row))
		b, sb := next.MsgVecs.RowQ(int(next.MessageRows[id].Row))
		if &a[0] != &b[0] || sa != sb {
			t.Fatal("unchanged message row was copied")
		}
	}
	// Re-embedding identical text can yield different bytes; its revision, not just text hash, matters.
	const id = "discord:1:10#0"
	var content, label string
	if err := e.Store.DB.DB.QueryRow(`SELECT content, actor_label FROM memory_observations WHERE observation_id=?`, id).Scan(&content, &label); err != nil {
		t.Fatal(err)
	}
	v := make([]float32, next.MsgVecs.Dims)
	v[0] = 1
	if n, err := e.Store.SaveMessageVectors([]store.MessageVector{{ObservationID: id, Space: e.MessageSpace(), TextHash: store.MessageTextHash(store.MessageText(label, content)), Vec: search.EncodeQuantizedRow(v)}}); err != nil || n != 1 {
		t.Fatalf("%d %v", n, err)
	}
	info, err = e.Reload()
	if err != nil || info["message_vectors_reused"] != seededEligible-1 || info["message_vectors_read"] != 1 {
		t.Fatalf("%v %v", info, err)
	}
	row := e.Index().MessageRows[id].Row
	if math.Float64bits(e.Index().MsgVecs.Dot(int(row), v)) != math.Float64bits(float64(float32(1.0/127))*127) {
		t.Fatal("changed bytes were not loaded")
	}
	fresh := New(e.Store, e.LLM, e.Models, e.Settings)
	defer fresh.Retire(nil)
	if _, err := fresh.Reload(); err != nil {
		t.Fatal(err)
	}
	for id, r := range e.Index().MessageRows {
		f := fresh.Index().MessageRows[id]
		if e.Index().MsgVecs.Dot(int(r.Row), v) != fresh.Index().MsgVecs.Dot(int(f.Row), v) {
			t.Fatal("incremental and fresh scores differ")
		}
	}
}

func TestReusedMessageRowsDropErasedEditedRenamedAndChangedSpace(t *testing.T) {
	for _, mutation := range []string{"erase", "edit", "rename", "space"} {
		t.Run(mutation, func(t *testing.T) {
			e, _ := messageEngine(t)
			seed(t, e)
			if mutation == "edit" {
				if _, err := e.Store.DB.DB.Exec(`UPDATE memory_observations SET native_message_id='test-edit' WHERE observation_id='discord:1:10#0'`); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := e.Reload(); err != nil {
				t.Fatal(err)
			} // exercise a reused snapshot, not just a first load
			const id = "discord:1:10#0"
			old := e.Index()
			if _, ok := old.MessageRows[id]; !ok {
				t.Fatal("missing initial row")
			}
			switch mutation {
			case "erase":
				if _, err := e.Store.DeleteObservationsContext(context.Background(), []string{id}, time.Now().UTC().Format(time.RFC3339)); err != nil {
					t.Fatal(err)
				}
			case "edit":
				if _, ok, err := e.Store.EditMessage("discord", "test-edit", "edited message", time.Now().UTC().Format(time.RFC3339)); err != nil || !ok {
					t.Fatalf("edit applied=%v (%v)", ok, err)
				}
			case "rename":
				if _, err := e.Store.DB.DB.Exec(`UPDATE memory_observations SET actor_label='renamed' WHERE observation_id=?`, id); err != nil {
					t.Fatal(err)
				}
			case "space":
				e.Models.Embed = "other-model"
			}
			if _, err := e.Reload(); err != nil {
				t.Fatal(err)
			}
			if _, ok := e.Index().MessageRows[id]; ok {
				t.Fatal("invalidated message still has a scorable row")
			}
			// Old searches are allowed to finish on their immutable snapshot; the new one has no pair to it.
			if old.MsgVecs.Rows() != seededEligible {
				t.Fatal("old snapshot mutated")
			}
		})
	}
}

// Uses only the pre-existing matrix API, so this also demonstrates the regression on the base commit.
func TestRebuildDoesNotCopyMessageRows(t *testing.T) {
	e, _ := messageEngine(t)
	seed(t, e)
	if _, err := e.IndexRooms(context.Background(), IndexOptions{BudgetUSD: 1}, nil); err != nil {
		t.Fatal(err)
	}
	old := e.Index().MsgVecs
	info, err := e.Reload()
	if err != nil {
		t.Fatal(err)
	}
	if info["message_vectors_reused"] != old.Rows() {
		t.Fatal("unchanged message vectors were read again")
	}
	next := e.Index().MsgVecs
	for i := 0; i < old.Rows(); i++ {
		a, _ := old.RowQ(i)
		found := false
		for j := 0; j < next.Rows(); j++ {
			b, _ := next.RowQ(j)
			if &a[0] == &b[0] {
				found = true
				break
			}
		}
		if !found {
			t.Fatal("unchanged message row was copied")
		}
	}
}
