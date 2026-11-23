package application_test

import (
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
)

// A thread hidden by a deletion whose tombstone write is still pending must not
// feed the parent's person block (the same boundary as threadRooms/ThreadFor).
func TestRound2PendingThreadDeletion(t *testing.T) {
	t.Parallel()
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

// Only an explicit request about the caller adds their card; a first-person
// aside next to an unrelated question adds no person block.
func TestRound2IncidentalSelfRecall(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	ev := f.source(t, "g", "c", "caller-source", "303", "지수", f.at)
	f.fact(t, "g", "c", "caller-only-marker", "303", "303", ev)
	for _, q := range []string{"날 기억해?", "나에 대해 뭐 알아?", "나는 뭘 좋아해?"} {
		if got := block(f.ask(t, q), cardOpen, cardClose); !strings.Contains(got, "caller-only-marker") {
			t.Fatalf("%q: explicit self request lost the caller's card: %q", q, got)
		}
	}
	for _, q := range []string{"오늘 서울 날씨 알려줘.", "나는 기억력이 좋아. 오늘 서울 날씨 알려줘.", "제가 기억하기로는 오늘 비 온대요. 맞아?"} {
		if got := block(f.ask(t, q), cardOpen, cardClose); got != "" {
			t.Fatalf("%q: incidental first person recalled the caller's card: %q", q, got)
		}
	}
}

// A profile-withdrawn account is never indexed, yet its captured name still
// makes the plain name ambiguous: no fact of the other account is sent.
func TestRound2HiddenCollisionPrompt(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	const q = "동명이인은 어떤 사람이야?"
	ev := f.source(t, "g", "c", "first-account", "101", "동명이인", f.at)
	f.fact(t, "g", "c", "first-account-fact", "101", "101", ev)
	if got := block(f.ask(t, q), cardOpen, cardClose); !strings.Contains(got, "first-account-fact") {
		t.Fatalf("unique account control failed: %q", got)
	}
	if r := s.Execute(t.Context(), action.Request{Action: "privacy.consent", Input: []byte(`{"scope":"profile","user_id":"202","decision":"withdraw"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	f.source(t, "g", "c", "second-account", "202", "동명이인", f.at.Add(time.Minute))
	if got := block(f.ask(t, q), cardOpen, cardClose); got != "" {
		t.Fatalf("an ambiguous query supplied a different account's fact: %q", got)
	}
	if got := block(f.ask(t, "<@101> 어떤 사람이야?"), cardOpen, cardClose); !strings.Contains(got, "first-account-fact") {
		t.Fatalf("an explicit mention stopped working: %q", got)
	}
}
