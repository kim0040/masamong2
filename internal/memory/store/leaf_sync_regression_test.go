package store

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/storage"
)

// Keep this regression independent of checkpoint helpers so the exact test can
// also run against the pre-optimization commit. A generous allocation bound
// distinguishes a tiny tail pass from a full room read without timing flakes.
func TestSyncLeavesAppendAllocationBound(t *testing.T) {
	st := openStore(t)
	room := "discord:1:2"
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var obs []corpus.Observation
	for i := 0; i < 10010; i++ {
		o := mk(0, at.Add(time.Duration(i)*time.Second))
		o.ObservationID, o.NativeMessageID = fmt.Sprintf("alloc-o%09d", i), fmt.Sprintf("alloc-m%09d", i)
		obs = append(obs, o)
	}
	if _, err := st.InsertObservations(obs[:10000], nil); err != nil {
		t.Fatal(err)
	}
	r := leaves.DefaultRecipe()
	if _, err := st.SyncLeaves(room, r, at, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertObservations(obs[10000:], nil); err != nil {
		t.Fatal(err)
	}
	allocs := testing.AllocsPerRun(1, func() {
		if _, err := st.SyncLeaves(room, r, at, nil); err != nil {
			t.Fatal(err)
		}
	})
	if allocs > 20000 {
		t.Fatalf("small append reread the room: %.0f allocations, want <=20000", allocs)
	}
	t.Logf("tail resync: %.0f allocations", allocs)
}

func TestSyncLeavesEqualBodyRefreshesObservationIDs(t *testing.T) {
	st := openStore(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	obs := []corpus.Observation{mk(0, at), mk(1, at.Add(time.Minute)), mk(2, at.Add(2*time.Minute))}
	for i := range obs {
		obs[i].Content, obs[i].ActorLabel = "identical synthetic content", "speaker"
	}
	if _, err := st.InsertObservations(obs, nil); err != nil {
		t.Fatal(err)
	}
	r := leaves.Recipe{Size: 2, Split: leaves.SplitFixed, WithPrefix: true}
	if _, err := st.SyncLeaves(obs[0].SourceRoom, r, at, nil); err != nil {
		t.Fatal(err)
	}
	firstID := LeafID(r.Version(), obs[0].SourceRoom, obs[0].ObservationID)
	if err := st.SaveLeafVector(firstID, "synthetic-space", []byte{1, 2}); err != nil {
		t.Fatal(err)
	}
	late := mk(3, at.Add(30*time.Second))
	late.Content, late.ActorLabel = obs[0].Content, obs[0].ActorLabel
	if _, err := st.InsertObservations([]corpus.Observation{late}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncLeaves(obs[0].SourceRoom, r, at, nil); err != nil {
		t.Fatal(err)
	}
	var ids string
	if err := st.DB.Reader().QueryRow(`SELECT observation_ids FROM memory_leaves WHERE first_observation_id=? AND recipe=?`, obs[0].ObservationID, r.Version()).Scan(&ids); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(ids, `"`+obs[1].ObservationID+`"`) || !strings.Contains(ids, `"`+late.ObservationID+`"`) {
		t.Fatal("equal-body recut kept obsolete observation IDs")
	}
	var vec []byte
	if err := st.DB.Reader().QueryRow(`SELECT leaf_vec FROM memory_leaves WHERE leaf_id=?`, firstID).Scan(&vec); err != nil || len(vec) != 2 || vec[0] != 1 || vec[1] != 2 {
		t.Fatalf("equal embedding text lost its cached vector: %v", err)
	}
}

// This standalone benchmark is copied unchanged to the baseline checkout, so
// before/after include the exact production SyncLeaves rather than an estimate.
func BenchmarkSyncLeaves100KAppend10Production(b *testing.B) {
	db, err := storage.Open(filepath.Join(b.TempDir(), "sync.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	st, err := Open(db)
	if err != nil {
		b.Fatal(err)
	}
	room := "discord:1:2"
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	makeObservation := func(i int) corpus.Observation {
		o := mk(0, at.Add(time.Duration(i)*time.Second))
		o.ObservationID, o.NativeMessageID = fmt.Sprintf("bench-o%09d", i), fmt.Sprintf("bench-m%09d", i)
		o.Content = fmt.Sprintf("synthetic message %d for leaf sync", i)
		return o
	}
	var obs []corpus.Observation
	for i := 0; i < 100000; i++ {
		obs = append(obs, makeObservation(i))
	}
	if _, err := st.InsertObservations(obs, nil); err != nil {
		b.Fatal(err)
	}
	r := leaves.DefaultRecipe()
	if _, err := st.SyncLeaves(room, r, at, nil); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for n := 0; n < b.N; n++ {
		b.StopTimer()
		var extra []corpus.Observation
		for i := 100000 + n*10; i < 100010+n*10; i++ {
			extra = append(extra, makeObservation(i))
		}
		if _, err := st.InsertObservations(extra, nil); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := st.SyncLeaves(room, r, at, nil); err != nil {
			b.Fatal(err)
		}
	}
}
