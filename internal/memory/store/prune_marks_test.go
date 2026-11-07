package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

// A pass of PruneChatHolders against a reply committing meanwhile. goingFrom in the future means no 'going' tombstone is
// young enough to hold anything out.
var longAfter = time.Now().Add(time.Hour)

func wantBlocked(t *testing.T, st *Store, want bool, keys ...string) {
	t.Helper()
	err := st.DB.WithWriter(func(tx *sql.Tx) error {
		blocked, err := SourcesBlocked(tx, keys)
		if blocked != want {
			t.Errorf("%v: blocked %v, want %v", keys, blocked, want)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

func countOf(t *testing.T, st *Store, query string, args ...any) int {
	t.Helper()
	var n int
	if err := st.DB.DB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// goneTurn indexes a chat turn with no row, as one a deletion removed leaves it.
func goneTurn(t *testing.T, st *Store, id int64, sources ...string) string {
	t.Helper()
	link := ChatLink(id)
	err := st.DB.WithWriter(func(tx *sql.Tx) error { return IndexSourceRefs(tx, "chat", link, sources) })
	if err != nil {
		t.Fatal(err)
	}
	return link
}

func going(t *testing.T, st *Store, at time.Time, links ...string) {
	t.Helper()
	err := st.DB.WithWriter(func(tx *sql.Tx) error { return MarkChatTurnsGoing(tx, links, at.Format(time.RFC3339Nano)) })
	if err != nil {
		t.Fatal(err)
	}
}

func marks(t *testing.T, st *Store) int {
	t.Helper()
	return countOf(t, st, `SELECT COUNT(*) FROM privacy_tombstones WHERE kind='pruning'`)
}

func TestAMarkedLinkIsGoneAtOnce(t *testing.T) {
	st := openStore(t)
	link := goneTurn(t, st, 500, "the message", "the person")
	wantBlocked(t, st, false, link) // gone, and indexed: a withdrawal can follow it
	if err := st.markChatHolders(context.Background(), longAfter); err != nil {
		t.Fatal(err)
	}
	if marks(t, st) != 1 || countOf(t, st, `SELECT COUNT(*) FROM evidence_refs WHERE holder_id=?`, link) != 2 {
		t.Fatalf("after the mark: %d marks, entries %d", marks(t, st), countOf(t, st, `SELECT COUNT(*) FROM evidence_refs WHERE holder_id=?`, link))
	}
	wantBlocked(t, st, true, link) // every entry still there, and the next batch would take them
	wantBlocked(t, st, false, "the message", "another turn's source")
	if _, err := st.takeMarkedChatHolders(context.Background(), 2000); err != nil {
		t.Fatal(err)
	}
	if marks(t, st) != 0 || countOf(t, st, `SELECT COUNT(*) FROM evidence_refs WHERE holder_id=?`, link) != 0 {
		t.Fatal("the pass left marks or entries")
	}
	wantBlocked(t, st, true, link)
}

// A mark says nothing about the words: it refuses nothing else, and hides nothing.
func TestAMarkIsNoSourceTombstone(t *testing.T) {
	st := openStore(t)
	link := goneTurn(t, st, 500, "the message")
	if err := st.markChatHolders(context.Background(), longAfter); err != nil {
		t.Fatal(err)
	}
	err := st.DB.WithWriter(func(tx *sql.Tx) error {
		blocked, err := SourceBlocked(tx, link)
		if blocked {
			t.Error("a mark counts as the source's tombstone")
		}
		if err != nil {
			return err
		}
		blocked, err = MessageBlocked(tx, link, link)
		if blocked {
			t.Error("a mark counts as the message's tombstone")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A reply that committed before the mark is seen by the computation: what it links to stays, however far back.
func TestAReplyBeforeTheMarkKeepsItsChain(t *testing.T) {
	st := openStore(t)
	older := goneTurn(t, st, 500, "the message", "the person")
	removed := goneTurn(t, st, 501, older, "another message")
	if err := st.AppendChatTurns("c", []ChatTurn{{Role: "assistant", Content: "a", NativeID: "n", SourceIDs: []string{removed}}}, 40); err != nil {
		t.Fatal(err)
	}
	n, err := st.PruneChatHolders(context.Background(), 2000, longAfter)
	if err != nil || n != 0 || marks(t, st) != 0 {
		t.Fatalf("took %d, %d marks, %v", n, marks(t, st), err)
	}
	wantBlocked(t, st, false, removed, older)
	err = st.DB.WithWriter(func(tx *sql.Tx) error {
		found, err := ChatLineage(tx, []string{"the person"})
		if len(found) != 3 {
			t.Errorf("the walk found %v", found)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// A pass that stops anywhere is finished by the next one, and its turns stay gone in between, part of their entries
// taken or not.
func TestAPassThatStopsIsFinishedByTheNext(t *testing.T) {
	st := openStore(t)
	sources := make([]string, 120)
	for i := range sources {
		sources[i] = fmt.Sprintf("source-%d", i)
	}
	link := goneTurn(t, st, 500, sources...)
	ctx, stop := context.WithCancel(context.Background())
	defer stop()
	done := make(chan error, 1)
	go func() {
		_, err := st.PruneChatHolders(ctx, 1, longAfter)
		done <- err
	}()
	deadline := time.Now().Add(10 * time.Second)
	for countOf(t, st, `SELECT COUNT(*) FROM evidence_refs WHERE holder_id=?`, link) == len(sources) {
		if time.Now().After(deadline) {
			t.Fatal("the pass took nothing")
		}
		time.Sleep(time.Millisecond)
	}
	stop()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("stopped with %v", err)
	}
	left := countOf(t, st, `SELECT COUNT(*) FROM evidence_refs WHERE holder_id=?`, link)
	if left == 0 || left == len(sources) || marks(t, st) != 1 {
		t.Fatalf("not stopped half way: %d entries, %d marks", left, marks(t, st))
	}
	wantBlocked(t, st, true, link)
	// a later pass needs no computation to find it: something that now builds on it does not bring it back
	if _, err := st.DB.DB.Exec(`INSERT INTO evidence_refs(source_id,holder_kind,holder_id) VALUES(?,'turn_ev','turn_x')`, link); err != nil {
		t.Fatal(err)
	}
	if _, err := st.PruneChatHolders(context.Background(), 2000, longAfter); err != nil {
		t.Fatal(err)
	}
	if n := countOf(t, st, `SELECT COUNT(*) FROM evidence_refs WHERE holder_id=?`, link); n != 0 || marks(t, st) != 0 {
		t.Fatalf("%d entries and %d marks after the next pass", n, marks(t, st))
	}
}

// A turn removed by a deletion may have a reply running on it: it stays out of the set for as long as one can run, and
// so does what it links to, or a withdrawal could not reach that reply.
func TestATurnJustRemovedIsHeldOut(t *testing.T) {
	st := openStore(t)
	now := time.Now()
	older := goneTurn(t, st, 500, "the person")
	removed := goneTurn(t, st, 501, older, "the message")
	going(t, st, now, removed)
	if _, err := st.PruneChatHolders(context.Background(), 2000, now.Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	if marks(t, st) != 0 {
		t.Fatalf("%d marks", marks(t, st))
	}
	wantBlocked(t, st, false, removed)
	wantBlocked(t, st, false, older)
	if n := countOf(t, st, `SELECT COUNT(*) FROM evidence_refs WHERE holder_id IN (?,?)`, removed, older); n != 3 {
		t.Fatalf("%d entries left of 3", n)
	}
	// long enough after, with nothing built on it, it goes
	if _, err := st.PruneChatHolders(context.Background(), 2000, now.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	wantBlocked(t, st, true, removed)
	wantBlocked(t, st, true, older)
	if n := countOf(t, st, `SELECT COUNT(*) FROM evidence_refs WHERE holder_id IN (?,?)`, removed, older); n != 0 {
		t.Fatalf("%d entries left", n)
	}
}

func TestAPassCancelledBeforeTheMarkLeavesNothing(t *testing.T) {
	st := openStore(t)
	link := goneTurn(t, st, 500, "the message")
	ctx, stop := context.WithCancel(context.Background())
	stop()
	if err := st.markChatHolders(ctx, longAfter); err == nil {
		t.Fatal("a cancelled pass reported nothing")
	}
	if marks(t, st) != 0 {
		t.Fatal("a cancelled pass left marks")
	}
	wantBlocked(t, st, false, link)
}

// An edit drops what ties the message's own row to other messages, and nothing more: the row's own entries, a thread's
// copy of it, and any other message's ties stay.
func TestAnEditDropsOnlyItsRowsTies(t *testing.T) {
	st := openStore(t)
	const key = "discord:g:c"
	if err := st.AppendChatTurns(key, []ChatTurn{
		{Role: "user", Speaker: "alice", Content: "first\nlast", NativeID: "last", SpeakerID: "alice", Merged: []string{"first"}, DerivedFrom: []string{"pictured"}},
		{Role: "user", Speaker: "bob", Content: "other", NativeID: "other", SpeakerID: "bob", Merged: []string{"first"}},
	}, 40); err != nil {
		t.Fatal(err)
	}
	var row, other int64
	if err := st.DB.DB.QueryRow(`SELECT id FROM chat_turns WHERE native_id='last'`).Scan(&row); err != nil {
		t.Fatal(err)
	}
	if err := st.DB.DB.QueryRow(`SELECT id FROM chat_turns WHERE native_id='other'`).Scan(&other); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendChatTurns("discord:g:thread", []ChatTurn{
		{Role: "user", Speaker: "alice", Content: "first\nlast", NativeID: "last", SpeakerID: "alice", SourceIDs: []string{ChatLink(row)}, Copied: true},
	}, 40); err != nil {
		t.Fatal(err)
	}
	var copied int64
	if err := st.DB.DB.QueryRow(`SELECT id FROM chat_turns WHERE native_id='last' AND channel_key='discord:g:thread'`).Scan(&copied); err != nil {
		t.Fatal(err)
	}
	refs := func(kind string, id int64) []string {
		t.Helper()
		rows, err := st.DB.DB.Query(`SELECT source_id FROM evidence_refs WHERE holder_kind=? AND holder_id=? ORDER BY 1`, kind, ChatLink(id))
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []string
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		return out
	}
	own := refs("chat", row)
	if len(own) == 0 || len(refs("chat_from", row)) != 2 {
		t.Fatalf("setup: chat %v, chat_from %v", own, refs("chat_from", row))
	}
	if _, _, err := st.EditMessageContext(context.Background(), "discord", "last", "new words", time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if got := refs("chat_from", row); len(got) != 0 {
		t.Errorf("the edited row is still tied to %v", got)
	}
	if got := refs("chat", row); !slices.Equal(got, own) {
		t.Errorf("the row's own entries: %v, were %v", got, own)
	}
	if got := refs("chat_from", copied); !slices.Equal(got, []string{ChatLink(row)}) {
		t.Errorf("the thread's copy: %v", got)
	}
	if got := refs("chat_from", other); !slices.Equal(got, []string{"first"}) {
		t.Errorf("another message's ties: %v", got)
	}
}
