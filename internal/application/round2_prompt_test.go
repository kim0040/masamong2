package application_test

import (
	"strings"
	"testing"
	"time"
)

// A thread hidden by a deletion whose tombstone write is still pending must not
// feed the parent's person block (the same boundary as threadRooms/ThreadFor).
func TestRound2PendingThreadDeletion(t *testing.T) {
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	ev := f.source(t, "g", "c", "public-source", "101", "민수", f.at)
	f.fact(t, "g", "c", "public-control", "101", "101", ev)
	s.NoteThread("g", "thread", "c", false)
	hearsay := f.source(t, "g", "thread", "hearsay-source", "202", "영희", f.at)
	f.fact(t, "g", "thread", "thread-hearsay-marker", "101", "202", hearsay)
	ask := func() string {
		return block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose)
	}
	if got := ask(); !strings.Contains(got, "thread-hearsay-marker") {
		t.Fatalf("live thread control failed: %q", got)
	}
	if _, err := s.Store.DB.Exec(`CREATE TRIGGER postpone_thread_delete BEFORE INSERT ON thread_tombstones BEGIN SELECT RAISE(FAIL,'synthetic retry'); END`); err != nil {
		t.Fatal(err)
	}
	s.ThreadDeleted("thread")
	if _, known := s.ThreadFor("thread", "101"); known {
		t.Fatal("thread registry did not hide pending deletion")
	}
	if got := ask(); strings.Contains(got, "thread-hearsay-marker") || !strings.Contains(got, "public-control") {
		t.Errorf("person block with a pending thread deletion: %q", got)
	}
	if _, err := s.Store.DB.Exec(`DROP TRIGGER postpone_thread_delete`); err != nil {
		t.Fatal(err)
	}
	s.ThreadDeleted("thread")
	if got := ask(); strings.Contains(got, "thread-hearsay-marker") || !strings.Contains(got, "public-control") {
		t.Fatalf("durable deletion control failed: %q", got)
	}
}
