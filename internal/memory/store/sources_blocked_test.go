package store

import (
	"database/sql"
	"fmt"
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
