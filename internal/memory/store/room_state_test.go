package store

import (
	"fmt"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
)

func TestRoomRevisionsAdvanceForEveryMutationAndSurviveReopen(t *testing.T) {
	st := openStore(t)
	at := time.Now().UTC().Truncate(time.Second)
	o := mk(0, at.Add(-time.Hour))
	if _, err := st.InsertObservations([]corpus.Observation{o}, nil); err != nil {
		t.Fatal(err)
	}
	last := int64(-1)
	check := func() {
		t.Helper()
		r, err := st.RoomRevisions([]string{o.SourceRoom})
		if err != nil || r[o.SourceRoom] <= last {
			t.Fatalf("revision did not advance: %d -> %d (%v)", last, r[o.SourceRoom], err)
		}
		last = r[o.SourceRoom]
	}
	check()
	for _, text := range []string{"replacement", "second replacement"} {
		rev, ok, err := st.EditMessage("discord", o.NativeMessageID, text, at.Format(time.RFC3339))
		if err != nil || !ok || rev.RoomRevision <= last {
			t.Fatalf("edit revision: %+v %v %v", rev, ok, err)
		}
		check()
	}
	if _, err := st.DeleteObservationsContext(t.Context(), []string{o.ObservationID}, at.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	check()
	if _, err := st.DeleteRoom(o.SourceRoom); err != nil {
		t.Fatal(err)
	}
	o.ObservationID += "-replacement"
	o.NativeMessageID += "-replacement"
	if _, err := st.InsertObservations([]corpus.Observation{o}, nil); err != nil {
		t.Fatal(err)
	}
	check()
	reopened, err := Open(st.DB)
	if err != nil {
		t.Fatal(err)
	}
	r, err := reopened.RoomRevisions([]string{o.SourceRoom})
	if err != nil || r[o.SourceRoom] != last {
		t.Fatalf("reopen changed revision: %v %v", r, err)
	}
	if _, err := reopened.DeleteMessages("discord", []string{o.NativeMessageID}, at.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	check()
}

func TestRecipeSwitchIsInvisibleUntilEveryBatchSucceeds(t *testing.T) {
	st := openStore(t)
	const room = "discord:1:2"
	at := time.Now().Add(-48 * time.Hour)
	var obs []corpus.Observation
	for i := 0; i < syncBatch+2; i++ {
		o := mk(0, at.Add(time.Duration(i)*time.Second))
		o.ObservationID, o.NativeMessageID = fmt.Sprintf("o%d", i), fmt.Sprintf("m%d", i)
		obs = append(obs, o)
	}
	if _, err := st.InsertObservations(obs, nil); err != nil {
		t.Fatal(err)
	}
	old := leaves.DefaultRecipe()
	oldStats, err := st.SyncLeaves(room, old, time.Now(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	newRecipe := leaves.Recipe{Size: 1, Split: leaves.SplitFixed, WithPrefix: true}
	// Fail the second batch: the first is durable but must remain invisible.
	if _, err := st.DB.DB.Exec(fmt.Sprintf(`CREATE TRIGGER fail_recipe_batch BEFORE INSERT ON memory_leaves WHEN NEW.recipe='%s' AND NEW.first_observation_id='o%d' BEGIN SELECT RAISE(ABORT,'injected recipe failure'); END`, newRecipe.Version(), syncBatch)); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncLeaves(room, newRecipe, time.Now(), time.UTC); err == nil {
		t.Fatal("expected injected second-batch failure")
	}
	assertActive := func(recipe leaves.Recipe, want int) {
		t.Helper()
		counts, err := st.Counts()
		if err != nil || counts["leaves"] != want {
			t.Fatalf("active counts: %+v %v, want %d", counts, err, want)
		}
		rooms, err := st.Rooms()
		if err != nil || len(rooms) != 1 || rooms[0].Leaves != want {
			t.Fatalf("room counts: %+v %v", rooms, err)
		}
		pending, err := st.Pending([]string{room}, "test:0:l2", false, 0)
		if err != nil || len(pending) != want {
			t.Fatalf("pending mixed recipes: %d, want %d (%v)", len(pending), want, err)
		}
		for _, p := range pending {
			var got string
			if err := st.DB.DB.QueryRow(`SELECT recipe FROM memory_leaves WHERE leaf_id=?`, p.LeafID).Scan(&got); err != nil || got != recipe.Version() {
				t.Fatalf("pending obsolete recipe: %q %v", got, err)
			}
		}
	}
	assertActive(old, oldStats.Leaves)
	// Reopen must not promote an interrupted recipe build.
	if _, err := Open(st.DB); err != nil {
		t.Fatal(err)
	}
	assertActive(old, oldStats.Leaves)
	if _, err := st.DB.DB.Exec(`DROP TRIGGER fail_recipe_batch`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncLeaves(room, newRecipe, time.Now(), time.UTC); err != nil {
		t.Fatal(err)
	}
	assertActive(newRecipe, len(obs))
	// An old engine's build started before the switch cannot switch back after
	// a successor activated its replacement, even through a different Store.
	msgs, err := st.Messages([]string{room})
	if err != nil {
		t.Fatal(err)
	}
	revisions, err := st.RoomRevisions([]string{room})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.syncLeaves(room, old, time.Now(), time.UTC, msgs, leafRoomState{revision: revisions[room], recipe: old.Version()}); err == nil {
		t.Fatal("stale recipe build reverted the activated recipe")
	}
	assertActive(newRecipe, len(obs))
}

func TestLegacyRecipeMigrationQuarantinesMixedRooms(t *testing.T) {
	st := openStore(t)
	old := leaves.DefaultRecipe()
	at := time.Now().Add(-time.Hour)
	a, b := mk(0, at), mk(1, at)
	b.SourceRoom = "discord:1:other"
	if _, err := st.InsertObservations([]corpus.Observation{a, b}, nil); err != nil {
		t.Fatal(err)
	}
	for _, room := range []string{a.SourceRoom, b.SourceRoom} {
		if _, err := st.SyncLeaves(room, old, time.Now(), time.UTC); err != nil {
			t.Fatal(err)
		}
	}
	// Simulate the prior schema with mixed stored representations in one room.
	if _, err := st.DB.DB.Exec(`INSERT INTO memory_leaves(leaf_id,room,recipe,first_observation_id,observation_ids,n,body,body_hash,speakers,updated_at)
		SELECT 'obsolete-id',room,'obsolete',first_observation_id,observation_ids,n,body,body_hash,speakers,updated_at FROM memory_leaves WHERE room=? LIMIT 1`, b.SourceRoom); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"active_recipe", "evidence_revision", "revision"} {
		// WHY remove the modern guard when recreating the legacy schema: that
		// release had neither active_recipe nor a trigger referencing it. Open
		// must reconstruct both after the historical migration is exercised.
		if column == "active_recipe" {
			if _, err := st.DB.DB.Exec(`DROP TRIGGER IF EXISTS memory_leaf_sync_recipe`); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := st.DB.DB.Exec(`ALTER TABLE memory_rooms DROP COLUMN ` + column); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := Open(st.DB); err != nil {
		t.Fatal(err)
	}
	var active string
	if err := st.DB.DB.QueryRow(`SELECT active_recipe FROM memory_rooms WHERE room=?`, a.SourceRoom).Scan(&active); err != nil || active != old.Version() {
		t.Fatalf("single legacy recipe not preserved: %q %v", active, err)
	}
	if err := st.DB.DB.QueryRow(`SELECT active_recipe FROM memory_rooms WHERE room=?`, b.SourceRoom).Scan(&active); err != nil || active != "" {
		t.Fatalf("mixed legacy room not quarantined: %q %v", active, err)
	}
	if _, err := st.SyncLeaves(b.SourceRoom, old, time.Now(), time.UTC); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(st.DB); err != nil {
		t.Fatal(err)
	}
	rooms, err := st.Rooms()
	if err != nil || len(rooms) != 2 || rooms[0].Leaves != 1 || rooms[1].Leaves != 1 {
		t.Fatalf("rebuilt active counts: %+v %v", rooms, err)
	}
}
