package store

import (
	"database/sql"
	"fmt"
	"testing"
	"time"
)

func blockedAsOf(t *testing.T, st *Store, asOf int64, keys ...string) bool {
	t.Helper()
	var blocked bool
	if err := st.DB.WithWriter(func(tx *sql.Tx) (err error) {
		blocked, err = SourcesBlockedAsOf(tx, keys, asOf)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	return blocked
}

func tombstone(t *testing.T, st *Store, kind, id string) {
	t.Helper()
	if err := st.DB.WithWriter(func(tx *sql.Tx) error { return Tombstone(tx, kind, id, time.Now().Format(time.RFC3339Nano)) }); err != nil {
		t.Fatal(err)
	}
}

func settle(t *testing.T, st *Store, ids ...string) int64 {
	t.Helper()
	if err := st.DB.WithWriter(func(tx *sql.Tx) error { return SettleSources(tx, ids) }); err != nil {
		t.Fatal(err)
	}
	return SettledMark(st.DB)
}

// A turn is excused from a source's tombstone only when the edit settled before the turn took its mark.
func TestASettledSourceExcusesOnlyLaterTurns(t *testing.T) {
	st := openStore(t)
	tombstone(t, st, "source", "m")
	before := SettledMark(st.DB)
	if !blockedAsOf(t, st, before, "m") {
		t.Fatal("a tombstone with no settled edit does not block")
	}
	after := settle(t, st, "m")
	if after <= before {
		t.Fatalf("the mark did not move: %d then %d", before, after)
	}
	if !blockedAsOf(t, st, before, "m") {
		t.Fatal("a turn that took its mark before the edit settled is excused")
	}
	if blockedAsOf(t, st, after, "m") {
		t.Fatal("a turn that took its mark after the edit settled is refused")
	}
	if !blockedAsOf(t, st, 0, "m") {
		t.Fatal("a turn with no mark is excused")
	}
	var plain bool
	if err := st.DB.WithWriter(func(tx *sql.Tx) (err error) {
		plain, err = SourcesBlocked(tx, []string{"m"})
		return err
	}); err != nil || !plain {
		t.Fatalf("SourcesBlocked excuses a settled source: %v %v", plain, err)
	}
	tombstone(t, st, "source", "other")
	if !blockedAsOf(t, st, after, "m", "other") {
		t.Fatal("one settled source excused another in the same list")
	}
}

// Every later tombstone of the source, by any writer, takes the excuse back.
func TestALaterTombstoneUndoesASettledSource(t *testing.T) {
	writers := map[string]func(tx *sql.Tx, id string) error{
		"source":  func(tx *sql.Tx, id string) error { return Tombstone(tx, "source", id, "2020-01-01T00:00:00Z") },
		"message": func(tx *sql.Tx, id string) error { return Tombstone(tx, "message", id, "2020-01-01T00:00:00Z") },
		"erased":  func(tx *sql.Tx, id string) error { return Tombstone(tx, "erased", id, "2020-01-01T00:00:00Z") },
		"lineage": func(tx *sql.Tx, id string) error { return TombstoneSources(tx, []string{id}, "2020-01-01T00:00:00Z") },
		"leaves":  func(tx *sql.Tx, id string) error { return invalidateLeaves(tx, id) },
	}
	for name, write := range writers {
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			tombstone(t, st, "source", "m")
			tombstone(t, st, "message", "m")
			mark := settle(t, st, "m")
			if blockedAsOf(t, st, mark, "m") {
				t.Fatal("the settled source blocks")
			}
			// older than what is there: the row's time does not move, the source has changed all the same
			if err := st.DB.WithWriter(func(tx *sql.Tx) error { return write(tx, "m") }); err != nil {
				t.Fatal(err)
			}
			if !blockedAsOf(t, st, mark, "m") {
				t.Fatal("a later tombstone left the source excused")
			}
			if !blockedAsOf(t, st, SettledMark(st.DB), "m") {
				t.Fatal("a later mark is excused with nothing settled")
			}
		})
	}
}

// A number is never used twice: a turn holding the old mark does not take a newer settling for its own.
func TestASettlingAfterARemovalHasALaterNumber(t *testing.T) {
	st := openStore(t)
	tombstone(t, st, "source", "m")
	mark := settle(t, st, "m")
	tombstone(t, st, "source", "m") // removes the row, and with it the highest number
	settle(t, st, "m")
	if !blockedAsOf(t, st, mark, "m") {
		t.Fatal("the newer settling reused the number a turn already holds")
	}
}

func TestAnErasedSourceIsNeverSettled(t *testing.T) {
	st := openStore(t)
	tombstone(t, st, "source", "m")
	tombstone(t, st, "erased", "m")
	mark := settle(t, st, "m", "n")
	if !blockedAsOf(t, st, mark+1000, "m") {
		t.Fatal("a deleted source was settled")
	}
}

// A settled turn link does not stand in for a turn that is gone.
func TestASettledLinkToAGoneTurnStillBlocks(t *testing.T) {
	st := openStore(t)
	link := ChatLink(99)
	tombstone(t, st, "source", link)
	mark := settle(t, st, link)
	if !blockedAsOf(t, st, mark, link) {
		t.Fatal("a link to a turn that is gone was excused")
	}
}

// Writing the intent's revision again (the foreground apply reaching the store after the worker finished)
// is no change, and an edit that keeps the words still counts as applied.
func TestApplyingTheSameRevisionAgainKeepsItSettled(t *testing.T) {
	st := openStore(t)
	const at = "2026-01-02T03:04:05.123456789Z"
	if err := st.AppendChatTurns("discord:g:c", []ChatTurn{{Role: "user", Speaker: "a", Content: "old", NativeID: "m", SpeakerID: "a"}}, 40); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.WithWriter(func(tx *sql.Tx) error {
		if err := Tombstone(tx, "message", "m", at); err != nil {
			return err
		}
		return Tombstone(tx, "source", "m", at)
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.EditMessage("discord", "m", "new", at); err != nil {
		t.Fatal(err)
	}
	mark := settle(t, st, "m")
	if _, _, err := st.EditMessage("discord", "m", "new", at); err != nil {
		t.Fatal(err)
	}
	if blockedAsOf(t, st, mark, "m") {
		t.Fatal("applying the same revision again undid the settled edit")
	}
	if _, _, err := st.EditMessage("discord", "m", "newer", "2026-01-02T03:04:06Z"); err != nil {
		t.Fatal(err)
	}
	if !blockedAsOf(t, st, mark, "m") {
		t.Fatal("a newer revision left the source excused")
	}
}

// Copies of turns keep what their words were made from.
func TestChatTurnsToCopyCarriesDerivedFrom(t *testing.T) {
	st := openStore(t)
	if err := st.AppendChatTurns("discord:g:c", []ChatTurn{
		{Role: "user", Speaker: "a", Content: "described", NativeID: "q1", SpeakerID: "a", DerivedFrom: []string{"picture"}},
		{Role: "user", Speaker: "a", Content: "plain", NativeID: "q2", SpeakerID: "a"},
	}, 40); err != nil {
		t.Fatal(err)
	}
	turns, err := st.ChatTurnsToCopy("discord:g:c", 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(turns) != 2 {
		t.Fatalf("turns: %d", len(turns))
	}
	if got := fmt.Sprint(turns[0].DerivedFrom); got != "[picture]" {
		t.Fatalf("the described turn: %s", got)
	}
	if len(turns[1].DerivedFrom) != 0 {
		t.Fatalf("the plain turn: %v", turns[1].DerivedFrom)
	}
}

// A reply whose walk has tombstoned it but whose row is still there (the job failed after the walk and waits for its
// retry) is not read again: nothing built on it would be kept, and the words it holds are on their way out. A user
// turn whose edit has settled is read again, with its new words.
func TestRecentChatTurnsLeavesOutWhatIsOnItsWayOut(t *testing.T) {
	st := openStore(t)
	const key = "discord:g:c"
	if err := st.AppendChatTurns(key, []ChatTurn{
		{Role: "user", Speaker: "alice", Content: "edited words", NativeID: "m1", SpeakerID: "alice"},
		{Role: "assistant", Content: "reply on the old words", NativeID: "m1", SpeakerID: "alice"},
		{Role: "user", Speaker: "bob", Content: "unrelated", NativeID: "m2", SpeakerID: "bob"},
		{Role: "assistant", Content: "unrelated reply", NativeID: "m2", SpeakerID: "bob"},
	}, 40); err != nil {
		t.Fatal(err)
	}
	read := func() []string {
		t.Helper()
		turns, err := st.RecentChatTurns(key, 16)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, turn := range turns {
			out = append(out, turn.Content)
		}
		return out
	}
	turns, err := st.RecentChatTurns(key, 16)
	if err != nil || len(turns) != 4 {
		t.Fatalf("before the change: %d turns, %v", len(turns), err)
	}
	userLink, replyLink := turns[0].SourceIDs[0], turns[1].SourceIDs[0]
	tombstone(t, st, "source", userLink)
	tombstone(t, st, "source", replyLink)
	if got := read(); len(got) != 2 || got[0] != "unrelated" || got[1] != "unrelated reply" {
		t.Fatalf("while the job waits: %q", got)
	}
	settle(t, st, userLink)
	if got := read(); len(got) != 3 || got[0] != "edited words" {
		t.Fatalf("after the edit settled: %q", got)
	}
	// the limit counts what is shown, so a tombstoned row does not push a kept turn out of the window
	turns, err = st.RecentChatTurns(key, 3)
	if err != nil || len(turns) != 3 || turns[0].Content != "edited words" {
		t.Fatalf("the window with a limit: %+v, %v", turns, err)
	}
	// and a later change hides the settled turn again
	tombstone(t, st, "source", userLink)
	if got := read(); len(got) != 2 {
		t.Fatalf("after a later change: %q", got)
	}
}

// A job is finished only for the revision that settled, and only until the message changes again.
func TestEditFinishedIsAboutOneRevision(t *testing.T) {
	st := openStore(t)
	at := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	finished := func(when string) bool {
		t.Helper()
		got, err := EditFinished(st.DB, "m", when)
		if err != nil {
			t.Fatal(err)
		}
		return got
	}
	if finished(at.Format(time.RFC3339Nano)) {
		t.Fatal("a message with no tombstone is not finished")
	}
	if err := st.DB.WithWriter(func(tx *sql.Tx) error { return Tombstone(tx, "message", "m", at.Format(time.RFC3339Nano)) }); err != nil {
		t.Fatal(err)
	}
	tombstone(t, st, "source", "unrelated")
	if finished(at.Format(time.RFC3339Nano)) {
		t.Fatal("a revision not settled is not finished")
	}
	settle(t, st, "m")
	if !finished(at.Format(time.RFC3339Nano)) {
		t.Fatal("the settled revision is finished")
	}
	if finished(at.Add(time.Second).Format(time.RFC3339Nano)) || finished(at.Add(-time.Second).Format(time.RFC3339Nano)) {
		t.Fatal("another revision was taken as finished")
	}
	if finished("not a time") {
		t.Fatal("a time that cannot be read was taken as finished")
	}
	if err := st.DB.WithWriter(func(tx *sql.Tx) error {
		return Tombstone(tx, "message", "m", at.Add(time.Minute).Format(time.RFC3339Nano))
	}); err != nil {
		t.Fatal(err)
	}
	if finished(at.Add(time.Minute).Format(time.RFC3339Nano)) {
		t.Fatal("a change after the settling left it finished")
	}
}

// A row with the words of an edit made while its turn ran is not refused by that edit, and is by anything later.
func TestARevisedTurnFollowsItsMessage(t *testing.T) {
	at := time.Now().UTC()
	rev := at.Format(time.RFC3339Nano)
	stamp := func(st *Store, kind string, when time.Time) {
		t.Helper()
		if err := st.DB.WithWriter(func(tx *sql.Tx) error { return Tombstone(tx, kind, "m", when.Format(time.RFC3339Nano)) }); err != nil {
			t.Fatal(err)
		}
	}
	for name, c := range map[string]struct {
		before func(*Store)
		kept   bool
	}{
		"no tombstone yet":   {func(*Store) {}, true},
		"its own tombstone":  {func(st *Store) { stamp(st, "message", at) }, true},
		"an older tombstone": {func(st *Store) { stamp(st, "message", at.Add(-time.Minute)) }, true},
		"a newer tombstone":  {func(st *Store) { stamp(st, "message", at.Add(time.Minute)) }, false},
		"erased":             {func(st *Store) { stamp(st, "message", at); stamp(st, "erased", at) }, false},
		"a newer edit that kept the words": {func(st *Store) {
			if err := st.DB.WithWriter(func(tx *sql.Tx) error {
				return recordKeptRevision(tx, "m", at.Add(time.Minute).Format(time.RFC3339Nano))
			}); err != nil {
				t.Fatal(err)
			}
		}, false},
		"its speaker withdrew": {func(st *Store) {
			if err := st.DB.WithWriter(func(tx *sql.Tx) error { return Tombstone(tx, "account", "alice", rev) }); err != nil {
				t.Fatal(err)
			}
		}, false},
	} {
		t.Run(name, func(t *testing.T) {
			st := openStore(t)
			c.before(st)
			if err := st.AppendChatTurns("k", []ChatTurn{{Role: "user", Speaker: "alice", Content: "new", NativeID: "m", SpeakerID: "alice", Revision: rev}}, 40); err != nil {
				t.Fatal(err)
			}
			var n int
			if err := st.DB.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE native_id='m'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if (n == 1) != c.kept {
				t.Fatalf("%d rows, kept should be %v", n, c.kept)
			}
		})
	}
	// the same tombstone without a revision refuses the row: that is the turn with the old words
	st := openStore(t)
	stamp(st, "message", at)
	if err := st.AppendChatTurns("k", []ChatTurn{{Role: "user", Content: "old", NativeID: "m", SpeakerID: "alice"}}, 40); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a row with no revision past the tombstone: %d, %v", n, err)
	}
}
