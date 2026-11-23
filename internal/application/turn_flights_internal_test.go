package application

import (
	"testing"
	"time"

	"masamong/internal/memory/store"
)

func TestFlyTurnRegistersOnlyTurnsThatStore(t *testing.T) {
	t.Parallel()
	s := &Service{}
	if fl, unreg := s.flyTurn(ChatInput{DryRun: true, MessageID: "m1"}); fl != nil {
		t.Fatal("a lab turn must not be noted")
	} else {
		unreg()
	}
	if fl, unreg := s.flyTurn(ChatInput{MessageID: ""}); fl != nil {
		t.Fatal("a turn without a message id has nothing an edit can follow")
	} else {
		unreg()
	}
	if len(s.flights.byMessage) != 0 {
		t.Fatal("no-op registrations must leave no state")
	}
}

func TestFlyTurnUnregisterRemovesOnlyItsOwnFlight(t *testing.T) {
	t.Parallel()
	s := &Service{}
	a, unregA := s.flyTurn(ChatInput{MessageID: "m1"})
	b, unregB := s.flyTurn(ChatInput{MessageID: "m1"})
	if a == nil || b == nil || a == b {
		t.Fatal("two turns on one message get two flights")
	}
	unregA()
	if got := len(s.flights.byMessage["m1"]); got != 1 {
		t.Fatalf("one flight left, got %d", got)
	}
	unregB()
	if _, ok := s.flights.byMessage["m1"]; ok {
		t.Fatal("empty message entry left behind")
	}
	unregB() // unregistering twice is safe
}

func TestFlightEditNewestRevisionWinsInEitherArrivalOrder(t *testing.T) {
	t.Parallel()
	s := &Service{}
	fl, unreg := s.flyTurn(ChatInput{MessageID: "m1"})
	defer unreg()
	t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Second)
	s.flights.edited("m1", "newer words", t2)
	s.flights.edited("m1", "older words", t1)
	if fl.words != "newer words" || !fl.at.Equal(t2) {
		t.Fatalf("late-arriving older revision overwrote the newer one: %q %v", fl.words, fl.at)
	}
	s.flights.edited("m1", "newest of all", t2.Add(time.Second))
	if fl.words != "newest of all" {
		t.Fatalf("newer revision ignored: %q", fl.words)
	}
}

func TestFlightEditBlankIsADeletionAndIsNotNoted(t *testing.T) {
	t.Parallel()
	s := &Service{}
	fl, unreg := s.flyTurn(ChatInput{MessageID: "m1"})
	defer unreg()
	s.flights.edited("m1", "   ", time.Now())
	if fl.edited {
		t.Fatal("a blanking edit must not be noted as new words")
	}
	s.flights.edited("unknown-message", "words", time.Now())
	if fl.edited {
		t.Fatal("an edit of another message reached this turn")
	}
}

func TestFlightKeptWordsOnlyAfterAnEdit(t *testing.T) {
	t.Parallel()
	s := &Service{}
	fl, unreg := s.flyTurn(ChatInput{MessageID: "m1"})
	defer unreg()
	t1 := time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC)
	s.flights.keptWords("m1", "same words", t1)
	if fl.edited {
		t.Fatal("keptWords alone must not mark the turn edited")
	}
	s.flights.edited("m1", "edited words", t1)
	s.flights.keptWords("m1", "kept words", t1.Add(time.Second))
	if fl.words != "kept words" {
		t.Fatalf("the newer revision was not kept: %q", fl.words)
	}
	s.flights.keptWords("m1", "stale kept words", t1.Add(-time.Second))
	if fl.words != "kept words" {
		t.Fatalf("an older kept revision overwrote: %q", fl.words)
	}
}

func userTurn(callAddress int) store.ChatTurn {
	return store.ChatTurn{
		Role:        "user",
		Speaker:     "synth-user",
		Content:     "original words",
		NativeID:    "m1",
		SpeakerID:   "acct-1",
		SpokenAt:    "2026-01-01T10:00:00Z",
		CallAddress: callAddress,
	}
}

func TestFlightToKeepPassesThroughUneditedTurns(t *testing.T) {
	t.Parallel()
	var f turnFlights
	turns := []store.ChatTurn{userTurn(store.CallReply), {Role: "assistant", Content: "answer"}}
	if got := f.toKeep(nil, turns); len(got) != 2 {
		t.Fatalf("nil flight changed the turns: %d", len(got))
	}
	fl := &turnFlight{}
	if got := f.toKeep(fl, turns); len(got) != 2 {
		t.Fatalf("unedited flight changed the turns: %d", len(got))
	}
}

func TestFlightToKeepRewritesTheUserRowWithNewWords(t *testing.T) {
	t.Parallel()
	at := time.Date(2026, 1, 1, 10, 0, 5, 0, time.UTC)
	for _, tc := range []struct {
		name        string
		callAddress int
		wantAddress int
	}{
		{"a reply keeps addressing the bot", store.CallReply, store.CallReply},
		{"an accepted request becomes an edited request", store.CallAccepted, store.CallEdited},
		{"unmarked becomes an edited request", 0, store.CallEdited},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var f turnFlights
			fl := &turnFlight{edited: true, words: "fixed words", at: at}
			turns := []store.ChatTurn{userTurn(tc.callAddress), {Role: "assistant", Content: "answer"}}
			got := f.toKeep(fl, turns)
			if len(got) != 1 {
				t.Fatalf("an edited message keeps one user row, got %d", len(got))
			}
			row := got[0]
			if row.Content != "fixed words" {
				t.Fatalf("new words not kept: %q", row.Content)
			}
			if row.CallAddress != tc.wantAddress {
				t.Fatalf("call_address %d, want %d", row.CallAddress, tc.wantAddress)
			}
			if row.Revision != at.UTC().Format(time.RFC3339Nano) {
				t.Fatalf("revision %q", row.Revision)
			}
			for _, field := range [][2]string{
				{row.Role, "user"}, {row.Speaker, "synth-user"}, {row.NativeID, "m1"}, {row.SpeakerID, "acct-1"}, {row.SpokenAt, "2026-01-01T10:00:00Z"},
			} {
				if field[0] != field[1] {
					t.Fatalf("user row field changed: %q want %q", field[0], field[1])
				}
			}
		})
	}
}
