package store

import (
	"database/sql"
	"fmt"
	"slices"
	"testing"
)

// The batched lookup answers exactly what asking about each ID would: across chunk edges, for both kinds
// that block a source, and not for an account tombstone, which only MessageBlocked reads.
func TestSourcesBlockedAgreesWithAskingAboutEachID(t *testing.T) {
	st := openStore(t)
	const size = 3*sourceChunk + 7
	keys := make([]string, size)
	for i := range keys {
		keys[i] = fmt.Sprintf("id-%d", i)
	}
	cases := []struct {
		name, kind string
		at         int
		want       bool
	}{
		{"nothing tombstoned", "", 0, false},
		{"first", "source", 0, true},
		{"last of the first chunk", "source", sourceChunk - 1, true},
		{"first of the second chunk", "message", sourceChunk, true},
		{"last", "message", size - 1, true},
		{"an account tombstone does not block a source", "account", 5, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			err := st.DB.WithWriter(func(tx *sql.Tx) error {
				if _, err := tx.Exec(`DELETE FROM privacy_tombstones`); err != nil {
					return err
				}
				if c.kind != "" {
					if err := Tombstone(tx, c.kind, keys[c.at], now()); err != nil {
						return err
					}
				}
				one := false
				for _, key := range keys {
					blocked, err := SourceBlocked(tx, key)
					if err != nil {
						return err
					}
					one = one || blocked
				}
				all, err := SourcesBlocked(tx, keys)
				if err != nil {
					return err
				}
				if all != one || all != c.want {
					t.Errorf("one by one %v, batched %v, want %v", one, all, c.want)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
	err := st.DB.WithWriter(func(tx *sql.Tx) error {
		blocked, err := SourcesBlocked(tx, nil)
		if err == nil && blocked {
			t.Error("an empty list is not blocked")
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Keeping each source once changes nothing a reader of the index can see, and a tombstone on a repeated
// ID still refuses the turn.
func TestRepeatedSourcesIndexTheSameAsUniqueOnes(t *testing.T) {
	refsOf := func(st *Store, channel string) map[string]bool {
		t.Helper()
		rows, err := st.DB.DB.Query(`SELECT source_id FROM evidence_refs WHERE holder_kind='chat' AND holder_id=(SELECT 'chat:'||id FROM chat_turns WHERE channel_key=? AND role='assistant')`, channel)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			out[id] = true
		}
		return out
	}
	st := openStore(t)
	sources := []string{"a", "b", "c"}
	var repeated []string
	for range 8 {
		repeated = append(repeated, sources...)
		repeated = append(repeated, "")
	}
	for channel, list := range map[string][]string{"unique": sources, "repeated": repeated} {
		if err := st.AppendChatTurns(channel, []ChatTurn{{Role: "assistant", Content: "x", NativeID: "m-" + channel, SpeakerID: "u", SourceIDs: list}}, 40); err != nil {
			t.Fatal(err)
		}
	}
	unique, again := refsOf(st, "unique"), refsOf(st, "repeated")
	if len(unique) != 5 || len(again) != 5 {
		t.Fatalf("index entries: unique %v, repeated %v; want the 3 sources, the message and the person", unique, again)
	}
	for id := range unique {
		if id != "m-unique" && !again[id] {
			t.Fatalf("%q indexed for the unique list only: %v %v", id, unique, again)
		}
	}
	if err := st.DB.WithWriter(func(tx *sql.Tx) error { return Tombstone(tx, "source", "c", now()) }); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendChatTurns("late", []ChatTurn{{Role: "assistant", Content: "x", NativeID: "m-late", SpeakerID: "u", SourceIDs: repeated}}, 40); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB.DB.QueryRow(`SELECT COUNT(*) FROM chat_turns WHERE channel_key='late'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("a repeated list with a tombstoned ID was kept: %d %v", n, err)
	}
}

// A link to a chat turn that is gone blocks like a tombstone: its sources can no longer be promised to be
// found. A link to a kept turn does not.
func TestALinkToAGoneTurnBlocks(t *testing.T) {
	st := openStore(t)
	if err := st.AppendChatTurns("c", []ChatTurn{{Role: "user", Content: "q", NativeID: "m", SpeakerID: "u"}}, 40); err != nil {
		t.Fatal(err)
	}
	turns, err := st.RecentChatTurns("c", 1)
	if err != nil || len(turns) != 1 {
		t.Fatalf("%d %v", len(turns), err)
	}
	check := func(keys []string, want bool) {
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
	check(turns[0].SourceIDs, false)
	check([]string{ChatLink(999999)}, true)
	check([]string{"chat:not-a-number"}, true)
	if _, err := st.DB.DB.Exec(`DELETE FROM chat_turns`); err != nil {
		t.Fatal(err)
	}
	// gone, and still indexed: a withdrawal can follow the link, so a reply that read the turn is kept
	check(turns[0].SourceIDs, false)
	if _, err := st.DB.DB.Exec(`DELETE FROM evidence_links`); err != nil {
		t.Fatal(err)
	}
	check(turns[0].SourceIDs, true)
}

// The walk follows links and the "made from" kind, finds nothing for an unknown or empty list, and stops
// at a cycle (none is written, but a walk that could not end would hold the write connection).
func TestChatLineageClosesTheLinks(t *testing.T) {
	st := openStore(t)
	insert := func(kind, holder string, sources ...string) {
		t.Helper()
		err := st.DB.WithWriter(func(tx *sql.Tx) error {
			for _, source := range sources {
				if _, err := tx.Exec(`INSERT OR IGNORE INTO evidence_refs(source_id,holder_kind,holder_id) VALUES(?,?,?)`, source, kind, holder); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	insert("chat", "chat:1", "msg-a", "shared")
	insert("chat", "chat:2", "chat:1", "msg-b")
	insert("chat", "chat:3", "chat:2")
	insert("chat_from", "chat:4", "picture")
	insert("chat", "chat:5", "chat:4")
	insert("chat", "chat:6", "unrelated")
	insert("turn_ev", "turn-1", "chat:3") // a turn record is found by the keys, it is not a link to walk
	insert("chat", "chat:7", "chat:8")
	insert("chat", "chat:8", "chat:7", "cycle")
	cases := []struct {
		keys []string
		want []string
	}{
		{[]string{"msg-a"}, []string{"chat:1", "chat:2", "chat:3"}},
		{[]string{"msg-b"}, []string{"chat:2", "chat:3"}},
		{[]string{"picture"}, []string{"chat:4", "chat:5"}},
		{[]string{"cycle"}, []string{"chat:7", "chat:8"}},
		{[]string{"nobody-read-this"}, nil},
		{nil, nil},
	}
	for _, c := range cases {
		var got []string
		err := st.DB.WithWriter(func(tx *sql.Tx) error {
			var err error
			got, err = ChatLineage(tx, c.keys)
			if err != nil {
				return err
			}
			return TombstoneSources(tx, got, now())
		})
		if err != nil {
			t.Fatalf("%v: %v", c.keys, err)
		}
		slices.Sort(got)
		if !slices.Equal(got, c.want) {
			t.Errorf("%v: got %v, want %v", c.keys, got, c.want)
		}
	}
}

// Reading a reply is reading the reply, and its request only if the request's own turn was read too. A turn marked as
// going is not read, and refuses no reply that read it already.
func TestWhatReadingATurnMeans(t *testing.T) {
	st := openStore(t)
	if err := st.AppendChatTurns("c", []ChatTurn{
		{Role: "user", Content: "q", NativeID: "m", SpeakerID: "u"},
		{Role: "assistant", Content: "a", NativeID: "m", SpeakerID: "u"},
	}, 40); err != nil {
		t.Fatal(err)
	}
	turns, err := st.RecentChatTurns("c", 2)
	if err != nil || len(turns) != 2 {
		t.Fatalf("%d %v", len(turns), err)
	}
	if !slices.Contains(turns[0].SourceIDs, "m") {
		t.Fatalf("the request does not name its message: %v", turns[0].SourceIDs)
	}
	if slices.Contains(turns[1].SourceIDs, "m") {
		t.Fatalf("the reply names its request's message: %v", turns[1].SourceIDs)
	}
	reply := turns[1].SourceIDs[0]
	if err := st.DB.WithWriter(func(tx *sql.Tx) error { return MarkChatTurnsGoing(tx, []string{reply}, "2026-11-08T00:00:00Z") }); err != nil {
		t.Fatal(err)
	}
	turns, err = st.RecentChatTurns("c", 2)
	if err != nil || len(turns) != 1 || turns[0].Role != "user" {
		t.Fatalf("a turn marked as going was read: %+v %v", turns, err)
	}
	if err := st.DB.WithWriter(func(tx *sql.Tx) error {
		blocked, err := SourcesBlocked(tx, []string{reply})
		if blocked {
			t.Error("a turn marked as going refuses a reply that read it")
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
