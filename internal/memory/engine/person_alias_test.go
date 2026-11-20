package engine

import (
	"slices"
	"testing"
	"time"

	"masamong/internal/memory/store"
)

func TestAliasFillDoesNotReprocessHistory(t *testing.T) {
	e, fp := newEngine(t, true)
	seed(t, e)
	db := e.Store.DB
	if got := count(t, e, `SELECT COUNT(*) FROM person_alias`); got != 3 {
		t.Fatalf("captured aliases: %d", got)
	}
	want := tableRows(t, db, "person_alias", "")
	before := snapshotHistory(t, db)
	// An interrupted fill can replay the same history without moving the seen dates.
	for _, q := range []string{`DELETE FROM person_alias WHERE label='민수'`, `UPDATE person_alias_fill SET cursor=1`} {
		if _, err := db.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	for range 2 {
		if _, err := store.Open(db); err != nil {
			t.Fatal(err)
		}
		if got := tableRows(t, db, "person_alias", ""); !slices.Equal(want, got) {
			t.Fatalf("alias fill: %v != %v", got, want)
		}
	}
	wantSameHistory(t, "alias fill", before, snapshotHistory(t, db))
	if fp.chatCalls.Load() != 0 || fp.embedCalls.Load() != 0 {
		t.Fatal("alias fill called a model")
	}
	// A broken optional index must not feed capture into the retry queue or lose the message.
	if _, err := db.DB.Exec(`CREATE TRIGGER broken_alias BEFORE INSERT ON person_alias BEGIN SELECT RAISE(ABORT,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	if ok, err := e.Capture(LiveMessage{GuildID: "1", ChannelID: "10", MessageID: "new", AuthorID: "new", AuthorName: "New", Content: "hello", At: time.Now()}); err != nil || !ok {
		t.Fatalf("capture with broken alias index: %v %v", ok, err)
	}
}
