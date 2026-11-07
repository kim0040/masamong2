package application

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/clock"
	"masamong/internal/memory/store"
)

// Retention against a reply committing meanwhile. A withdrawal must reach every reply that was kept, and a reply that
// read only a removed reply must be kept as long as it can be reached.

func refsOf(t *testing.T, s *Service, link string) int {
	t.Helper()
	var n int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM evidence_refs WHERE holder_kind='chat' AND holder_id=?`, link).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// removedReader leaves a reply that read a message, removed by its deletion; extra is how many more things it holds, as
// a row of an earlier release does.
func removedReader(t *testing.T, s *Service, extra int) (link string, sources []string) {
	t.Helper()
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	held := []string{"secret", personRef("alice")}
	for i := range extra {
		held = append(held, fmt.Sprintf("legacy-flat-%d", i))
	}
	if err := e.Store.AppendChatTurns("discord:g:c", []store.ChatTurn{{Role: "assistant", Content: "removed reader", NativeID: "a", SpeakerID: "bob", SourceIDs: held}}, 40); err != nil {
		t.Fatal(err)
	}
	turns, err := e.Store.RecentChatTurns("discord:g:c", 1)
	if err != nil || len(turns) != 1 {
		t.Fatalf("%d turns, %v", len(turns), err)
	}
	if err := s.MessagesDeleted("g", "c", []string{"secret"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := repliesKept(t, s, "c"); n != 0 {
		t.Fatalf("the reader is still there: %d", n)
	}
	return turns[0].SourceIDs[0], turns[0].SourceIDs
}

func lateReply(t *testing.T, s *Service, read []string) {
	t.Helper()
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Store.AppendChatTurns("discord:g:c", []store.ChatTurn{{Role: "assistant", Content: "late reader of the removed reply", NativeID: "b", SpeakerID: "carol", SourceIDs: read}}, 40); err != nil {
		t.Fatal(err)
	}
}

func withdrawAlice(t *testing.T, s *Service) {
	t.Helper()
	r := s.Execute(context.Background(), action.Request{Action: "privacy.consent", Input: []byte(`{"user_id":"alice","scope":"memory","decision":"withdraw"}`)})
	if !r.OK {
		t.Fatal(r.Error)
	}
	if err := s.DrainRedactions(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// the pass, as long after the deletion as it takes for nothing to be held out
func pruneLater(ctx context.Context, s *Service, batch int) error {
	e, err := s.MemoryEngine()
	if err != nil {
		return err
	}
	_, err = e.Store.PruneChatHolders(ctx, batch, time.Now().Add(time.Hour))
	return err
}

// A reply committing between two batches links to a turn half taken. It refuses itself: nothing could follow the link,
// and once stored the withdrawal would miss it.
func TestAReplyCommittingDuringAPassRefusesItself(t *testing.T) {
	s := privacyService(t)
	link, read := removedReader(t, s, 2100)
	initial := refsOf(t, s, link)
	done := make(chan error, 1)
	go func() { done <- pruneLater(context.Background(), s, 2000) }()
	deadline := time.Now().Add(10 * time.Second)
	for {
		n := refsOf(t, s, link)
		if n > 0 && n < initial {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the pass did not stop between batches: %d of %d", n, initial)
		}
		time.Sleep(time.Millisecond)
	}
	lateReply(t, s, read)
	kept := repliesKept(t, s, "c")
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if kept != 0 {
		t.Errorf("a reply linking to a turn half taken was stored: %d", kept)
	}
	withdrawAlice(t, s)
	if n := repliesKept(t, s, "c"); n != 0 {
		t.Errorf("a withdrawal missed %d replies", n)
	}
}

// One that commits before the pass is seen by it: the links stay, and a withdrawal goes through them.
func TestAReplyCommittedBeforeAPassIsReached(t *testing.T) {
	s := privacyService(t)
	link, read := removedReader(t, s, 2100)
	initial := refsOf(t, s, link)
	lateReply(t, s, read)
	if n := repliesKept(t, s, "c"); n != 1 {
		t.Fatalf("the reply was refused: %d", n)
	}
	if err := pruneLater(context.Background(), s, 2000); err != nil {
		t.Fatal(err)
	}
	if n := refsOf(t, s, link); n != initial {
		t.Fatalf("the pass took %d of the entries a reply links to", initial-n)
	}
	withdrawAlice(t, s)
	if n := repliesKept(t, s, "c"); n != 0 {
		t.Errorf("a withdrawal missed %d replies", n)
	}
}

// A running reply that read only the removed reply is not refused by a pass that happens to run right after the
// deletion, and is reached like any other. Past the longest a turn can run the entries go.
func TestAPassLeavesARunningReplyOnARemovedReply(t *testing.T) {
	s := privacyService(t)
	link, read := removedReader(t, s, 0)
	if _, err := s.runRetention(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := refsOf(t, s, link); n == 0 {
		t.Fatal("the pass took the entries of a turn just removed")
	}
	lateReply(t, s, read)
	if n := repliesKept(t, s, "c"); n != 1 {
		t.Fatalf("a reply that read only a removed reply was refused by the pass: %d", n)
	}
	withdrawAlice(t, s)
	if n := repliesKept(t, s, "c"); n != 0 {
		t.Errorf("a withdrawal missed %d replies", n)
	}
}

func TestTheHoldIsTheTurnClock(t *testing.T) {
	if chatLinkHold <= ChatTurnCeiling {
		t.Fatalf("a turn can run %v, and its links are held for %v", ChatTurnCeiling, chatLinkHold)
	}
	for _, tc := range []struct {
		after time.Duration
		held  bool
	}{{ChatTurnCeiling, true}, {chatLinkHold - time.Minute, true}, {chatLinkHold + time.Minute, false}} {
		t.Run(tc.after.String(), func(t *testing.T) {
			s := privacyService(t)
			start := time.Now().UTC()
			s.Clock = clock.Fixed{T: start}
			link, read := removedReader(t, s, 0)
			s.Clock = clock.Fixed{T: start.Add(tc.after)}
			if _, err := s.runRetention(context.Background()); err != nil {
				t.Fatal(err)
			}
			if held := refsOf(t, s, link) > 0; held != tc.held {
				t.Fatalf("%v after the deletion the entries are there: %v", tc.after, held)
			}
			lateReply(t, s, read)
			if kept := repliesKept(t, s, "c") == 1; kept != tc.held {
				t.Fatalf("%v after the deletion the reply is stored: %v", tc.after, kept)
			}
		})
	}
}

// What a pass left must be there for the next one: expiry takes the old tombstones and not a mark, however old.
func TestExpiryLeavesAMark(t *testing.T) {
	s := privacyService(t)
	if _, err := s.MemoryEngine(); err != nil { // its tables
		t.Fatal(err)
	}
	if err := s.Store.WithWriter(func(tx *sql.Tx) error {
		// a holder with no entries keeps its mark through the pass: nothing else leaves one for expiry to see
		if _, err := tx.Exec(`INSERT INTO evidence_holders(holder_kind,holder_id) VALUES('chat','chat:77')`); err != nil {
			return err
		}
		if _, err := tx.Exec(`INSERT INTO privacy_tombstones(kind,source_id,at) VALUES('pruning','chat:77','2000-01-01T00:00:00.000000000Z')`); err != nil {
			return err
		}
		if err := store.MarkChatTurnsGoing(tx, []string{"chat:79"}, "2000-01-01T00:00:00Z"); err != nil {
			return err
		}
		return store.TombstoneSources(tx, []string{"old source"}, "2000-01-01T00:00:00Z")
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.runRetention(context.Background()); err != nil {
		t.Fatal(err)
	}
	has := func(kind, id string) bool {
		var n int
		if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM privacy_tombstones WHERE kind=? AND source_id=?`, kind, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	if !has("pruning", "chat:77") {
		t.Error("expiry took a mark")
	}
	if has("going", "chat:79") || has("source", "old source") {
		t.Error("expiry left old tombstones")
	}
}
