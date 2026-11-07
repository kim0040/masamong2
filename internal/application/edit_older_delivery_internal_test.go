package application

import (
	"context"
	"testing"
	"time"

	"masamong/internal/memory/store"
)

// A to B at t1, to C at t2, a reply reads C, and then B comes again (a gateway that resends, an intent replayed after a
// restart). Nothing moves: not the stored words, not the live line, not the reply, not the queue. The stored row always
// refused it; the job did not, and took the reply on the newest words.
func TestAnEditOlderThanTheNewestChangesNothing(t *testing.T) {
	const key = "discord:g:c"
	t1 := time.Now().Add(-2 * time.Minute)
	t2 := t1.Add(time.Minute)
	setup := func(t *testing.T) (*Service, int64) {
		t.Helper()
		s := privacyService(t)
		e, err := s.MemoryEngine()
		if err != nil {
			t.Fatal(err)
		}
		seedEditedTurn(t, s, key, "secret", "alice")
		s.noteLive(ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: "secret", AuthorID: "alice", AuthorName: "alice", Content: "words A", At: t1})
		for _, edit := range []struct {
			words string
			at    time.Time
		}{{"words B", t1}, {"words C", t2}} {
			if err := s.MessageEdited("g", "c", "secret", edit.words, edit.at); err != nil {
				t.Fatal(err)
			}
			if err := s.DrainRedactions(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		mark := store.SettledMark(s.Store)
		if mark == 0 {
			t.Fatal("nothing settled")
		}
		if err := e.Store.AppendChatTurns(key, []store.ChatTurn{
			{Role: "user", Speaker: "bob", Content: "q", NativeID: "n1", SpeakerID: "bob"},
			{Role: "assistant", Content: "reply on C", NativeID: "n1", SpeakerID: "bob", SourceIDs: windowSources(t, s, key), ReadAsOf: mark},
		}, 40); err != nil {
			t.Fatal(err)
		}
		return s, mark
	}
	unchanged := func(t *testing.T, s *Service, mark int64) {
		t.Helper()
		var words string
		if err := s.Store.DB.QueryRow(`SELECT content FROM chat_turns WHERE native_id='secret' AND role='user'`).Scan(&words); err != nil || words != "words C" {
			t.Errorf("the stored words: %q %v", words, err)
		}
		if !s.liveHasWords("g", "c", "secret", "words C") {
			t.Error("the live line went back to older words")
		}
		if n := repliesKept(t, s, "c"); n != 1 {
			t.Errorf("the reply on the newest words went: %d kept", n)
		}
		if got := store.SettledMark(s.Store); got != mark {
			t.Errorf("the settling got a new number: %d was %d", got, mark)
		}
		var jobs int
		if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM redaction_jobs`).Scan(&jobs); err != nil || jobs != 0 {
			t.Errorf("%d jobs queued (%v)", jobs, err)
		}
	}
	t.Run("delivered again", func(t *testing.T) {
		s, mark := setup(t)
		if err := s.MessageEdited("g", "c", "secret", "words B", t1); err != nil {
			t.Fatal(err)
		}
		unchanged(t, s, mark)
		if err := s.DrainRedactions(context.Background()); err != nil {
			t.Fatal(err)
		}
		unchanged(t, s, mark)
	})
	t.Run("a turn under way", func(t *testing.T) {
		s, _ := setup(t)
		// the turn began after C and holds C: an older delivery would have it keep B
		flight, end := s.flyTurn(ChatInput{MessageID: "secret"})
		defer end()
		if err := s.MessageEdited("g", "c", "secret", "words B", t1); err != nil {
			t.Fatal(err)
		}
		s.flights.mu.Lock()
		defer s.flights.mu.Unlock()
		if flight.edited {
			t.Errorf("the turn took older words: %q", flight.words)
		}
	})
	// the check in the transaction: MessageEdited looked before C was written, or the intent is replayed afterwards
	t.Run("a replayed intent", func(t *testing.T) {
		s, mark := setup(t)
		if err := s.commitSourceChange(context.Background(), "g", "c", []string{"secret"}, "edit", "words B", t1); err != nil {
			t.Fatal(err)
		}
		unchanged(t, s, mark)
		if err := s.DrainRedactions(context.Background()); err != nil {
			t.Fatal(err)
		}
		unchanged(t, s, mark)
	})
	// a newer edit is another event, and an older one after a deletion brings nothing back
	t.Run("the next edit still counts", func(t *testing.T) {
		s, _ := setup(t)
		if err := s.MessageEdited("g", "c", "secret", "words D", t2.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := s.DrainRedactions(context.Background()); err != nil {
			t.Fatal(err)
		}
		if n := repliesKept(t, s, "c"); n != 0 {
			t.Fatalf("a newer edit left %d replies on the words it replaced", n)
		}
	})
	t.Run("after a deletion", func(t *testing.T) {
		s, _ := setup(t)
		if err := s.MessagesDeleted("g", "c", []string{"secret"}); err != nil {
			t.Fatal(err)
		}
		if err := s.DrainRedactions(context.Background()); err != nil {
			t.Fatal(err)
		}
		if err := s.MessageEdited("g", "c", "secret", "words B", t1); err != nil {
			t.Fatal(err)
		}
		if err := s.DrainRedactions(context.Background()); err != nil {
			t.Fatal(err)
		}
		if s.liveHasWords("g", "c", "secret", "words B") {
			t.Error("the live line came back")
		}
		var n int
		if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE native_id='secret'`).Scan(&n); err != nil || n != 0 {
			t.Errorf("%d rows of a deleted message (%v)", n, err)
		}
	})
}

// An edit that kept the words overtakes an older one after MessageEdited looked: only the writer can tell. The stored
// words refuse the older edit, so a job for it would take the reply on words that never changed.
func TestAnEditOlderThanOneThatKeptTheWordsTakesNoReply(t *testing.T) {
	s := privacyService(t)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	const key = "discord:g:c"
	seedEditedTurn(t, s, key, "secret", "alice")
	var words string
	if err := s.Store.DB.QueryRow(`SELECT content FROM chat_turns WHERE native_id='secret' AND role='user'`).Scan(&words); err != nil {
		t.Fatal(err)
	}
	t1 := time.Now().Add(-2 * time.Minute)
	if ok, err := s.MessageKeptItsWords("g", "c", "secret", words, t1.Add(time.Minute)); err != nil || !ok {
		t.Fatalf("kept revision: %v %v", ok, err)
	}
	if err := e.Store.AppendChatTurns(key, []store.ChatTurn{{Role: "assistant", Content: "reply on the kept words", NativeID: "n1", SpeakerID: "bob", SourceIDs: []string{"secret"}, ReadAsOf: store.SettledMark(s.Store)}}, 40); err != nil {
		t.Fatal(err)
	}
	if err := s.commitSourceChange(context.Background(), "g", "c", []string{"secret"}, "edit", "older words", t1); err != nil {
		t.Fatal(err)
	}
	if changed, err := e.EditContext(context.Background(), "secret", "older words", t1); err != nil || changed {
		t.Fatalf("the stored words took an older edit: %v %v", changed, err)
	}
	if err := s.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := repliesKept(t, s, "c"); n != 2 { // the seeded reply on those words too
		t.Errorf("%d replies kept, want 2", n)
	}
}
