package store

import (
	"testing"
	"time"

	"masamong/internal/memory/leaves"
	"masamong/internal/memory/search"
)

func TestLeafVectorDimsAndWidthPendingUseOnlyActiveRecipe(t *testing.T) {
	st := openStore(t)
	seedMsgs(t, st, msgRoom, 16, time.Now().Add(-48*time.Hour))
	recipe := leaves.DefaultRecipe().Version()
	ps, err := st.Pending([]string{msgRoom}, msgSpace, false, 0, recipe)
	if err != nil || len(ps) != 2 {
		t.Fatalf("fixture leaves=%d err=%v", len(ps), err)
	}
	for i, p := range ps {
		if err := st.SaveLeafVector(p.LeafID, msgSpace, search.EncodeFloat16(make([]float32, 256+256*i)), p.Body); err != nil {
			t.Fatal(err)
		}
	}
	if dims, err := st.LeafVectorDims(msgSpace, recipe); err != nil || dims != 0 {
		t.Fatalf("mixed widths must be unresolved: %d %v", dims, err)
	}
	if dims, err := st.LeafVectorMode(msgSpace, recipe); err != nil || dims != 256 {
		t.Fatalf("tied widths must choose a consistent target: %d %v", dims, err)
	}
	if rooms, err := st.LeafWidthRepairRooms(msgSpace, recipe, 512); err != nil || len(rooms) != 1 || rooms[0] != msgRoom {
		t.Fatalf("mismatched room was not selected: rooms=%d err=%v", len(rooms), err)
	}
	// The usual pending rule is unchanged; the repair predicate queues only the mismatched vector.
	if ps, err := st.Pending([]string{msgRoom}, msgSpace, false, 0, recipe); err != nil || len(ps) != 0 {
		t.Fatal("regular pending changed")
	}
	repair, err := st.PendingAtWidth([]string{msgRoom}, msgSpace, false, 0, 512, recipe)
	if err != nil || len(repair) != 1 || !repair[0].NeedLeaf || !repair[0].RefreshLeaf {
		t.Fatalf("width mismatch must enter pending: %+v %v", repair, err)
	}
	if err := st.SaveLeafVector(repair[0].LeafID, msgSpace, search.EncodeFloat16(make([]float32, 512)), repair[0].Body); err != nil {
		t.Fatal(err)
	}
	// A different archived recipe has a different width but must neither hold messages nor be repaired.
	if _, err := st.DB.DB.Exec(`INSERT INTO memory_leaves(leaf_id,room,recipe,first_observation_id,observation_ids,n,body,body_hash,speakers,closed,embed_space,leaf_vec,updated_at)
		SELECT 'archived-width',room,'archived-recipe',first_observation_id,observation_ids,n,body,body_hash,speakers,closed,embed_space,?,updated_at FROM memory_leaves LIMIT 1`, search.EncodeFloat16(make([]float32, 128))); err != nil {
		t.Fatal(err)
	}
	if dims, err := st.LeafVectorDims(msgSpace, recipe); err != nil || dims != 512 {
		t.Fatalf("active width must resolve after convergence: %d %v", dims, err)
	}
	if dims, err := st.LeafVectorMode(msgSpace, recipe); err != nil || dims != 512 {
		t.Fatalf("archived recipe changed the majority width: %d %v", dims, err)
	}
	if rooms, err := st.LeafWidthRepairRooms(msgSpace, recipe, 512); err != nil || len(rooms) != 0 {
		t.Fatalf("archived recipe queued a repair: rooms=%d err=%v", len(rooms), err)
	}
	if ps, err := st.PendingAtWidth([]string{msgRoom}, msgSpace, false, 0, 512, recipe); err != nil || len(ps) != 0 {
		t.Fatal("converged width must have no pending leaves")
	}
}
