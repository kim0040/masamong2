package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
)

// seedEvidence fills a store the way a room that has been talking for months looks: many tombstones
// and many indexed sources.
func seedEvidence(b *testing.B, st *Store, tombstones, sources int) {
	b.Helper()
	if err := MigratePrivacy(st.DB); err != nil {
		b.Fatal(err)
	}
	const batch = 2000
	for lo := 0; lo < max(tombstones, sources); lo += batch {
		err := st.DB.WithWriter(func(tx *sql.Tx) error {
			for i := lo; i < lo+batch; i++ {
				if i < tombstones {
					if err := Tombstone(tx, "source", fmt.Sprintf("gone-%d", i), now()); err != nil {
						return err
					}
				}
				if i < sources {
					if _, err := tx.Exec(`INSERT OR IGNORE INTO evidence_refs(source_id,holder_kind,holder_id) VALUES(?,'chat',?)`, fmt.Sprintf("old-%d", i), fmt.Sprintf("chat:seed%d", i/40)); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkAppendChatTurns is the time a reply holds the one write connection, for the source lists a
// long-lived room reaches: "ids" entries with each unique ID repeated "repeat" times (operations saw
// 23,919 entries in one room; a window of 16 turns repeats shared IDs about eight times).
func BenchmarkAppendChatTurns(b *testing.B) {
	for _, c := range []struct{ ids, repeat int }{{24000, 8}, {24000, 1}, {3000, 1}, {100, 1}} {
		b.Run(fmt.Sprintf("ids=%d/repeat=%d", c.ids, c.repeat), func(b *testing.B) {
			st := openStore(b)
			seedEvidence(b, st, 50000, 50000)
			sources := make([]string, 0, c.ids)
			for i := range c.ids {
				sources = append(sources, fmt.Sprintf("old-%d", (i/c.repeat)%50000))
			}
			b.ResetTimer()
			for i := range b.N {
				native := fmt.Sprintf("m%d", i)
				if err := st.AppendChatTurns("c", []ChatTurn{
					{Role: "user", Content: "q", NativeID: native, SpeakerID: "u"},
					{Role: "assistant", Content: "a", NativeID: native, SpeakerID: "u", SourceIDs: sources},
				}, 40); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRecentChatTurns is the read every turn does before anything else, on a window of 16 from the 40 kept
// turns of a store with many tombstones, with and without the turns themselves tombstoned.
func BenchmarkRecentChatTurns(b *testing.B) {
	for _, c := range []struct{ tombstones, inWindow int }{{0, 0}, {50000, 0}, {500000, 0}, {50000, 16}} {
		b.Run(fmt.Sprintf("tombstones=%d/inWindow=%d", c.tombstones, c.inWindow), func(b *testing.B) {
			st := openStore(b)
			seedEvidence(b, st, c.tombstones, 0)
			for i := range 40 {
				if err := st.AppendChatTurns("c", []ChatTurn{{Role: "user", Content: "words of a turn", NativeID: fmt.Sprintf("m%d", i), SpeakerID: "u"}}, 40); err != nil {
					b.Fatal(err)
				}
			}
			if c.inWindow > 0 {
				turns, err := st.RecentChatTurns("c", c.inWindow)
				if err != nil {
					b.Fatal(err)
				}
				err = st.DB.WithWriter(func(tx *sql.Tx) error {
					for _, t := range turns {
						if err := Tombstone(tx, "source", t.SourceIDs[0], now()); err != nil {
							return err
						}
					}
					return nil
				})
				if err != nil {
					b.Fatal(err)
				}
			}
			b.ResetTimer()
			for range b.N {
				turns, err := st.RecentChatTurns("c", 16)
				if err != nil || len(turns) != 16 {
					b.Fatalf("%d turns, %v", len(turns), err)
				}
			}
		})
	}
}

// seedChain writes a chain of depth replies as the link structure does: each reply links to the 16 turns
// before it, and keeps 25 IDs of what it newly read.
func seedChain(b *testing.B, st *Store, depth int) {
	b.Helper()
	if err := MigratePrivacy(st.DB); err != nil {
		b.Fatal(err)
	}
	const batch = 500
	for lo := 0; lo < depth; lo += batch {
		err := st.DB.WithWriter(func(tx *sql.Tx) error {
			for i := lo; i < min(lo+batch, depth); i++ {
				keys := make([]string, 0, 41)
				for back := 1; back <= 16 && i-back >= 0; back++ {
					keys = append(keys, ChatLink(int64(i-back)))
				}
				for read := range 25 {
					keys = append(keys, fmt.Sprintf("read-%d-%d", i, read))
				}
				if err := IndexSourceRefs(tx, "chat", ChatLink(int64(i)), keys); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkChatLineage is the deletion's walk with the write connection held, for the first message of a
// room's whole history.
func BenchmarkChatLineage(b *testing.B) {
	for _, depth := range []int{2000, 20000} {
		b.Run(fmt.Sprintf("depth=%d", depth), func(b *testing.B) {
			st := openStore(b)
			seedChain(b, st, depth)
			b.ResetTimer()
			for range b.N {
				err := st.DB.WithWriter(func(tx *sql.Tx) error {
					got, err := ChatLineage(tx, []string{"read-0-0"})
					if err == nil && len(got) != depth {
						err = fmt.Errorf("walked %d of %d", len(got), depth)
					}
					return err
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkPruneChatHolders is the retention read over a live chain (nothing goes): it runs on the read
// pool, and this is what it costs each pass.
func BenchmarkPruneChatHolders(b *testing.B) {
	st := openStore(b)
	seedChain(b, st, 20000)
	if _, err := st.DB.DB.Exec(`INSERT INTO chat_turns(id,channel_key,role,speaker,content,created_at,native_id,speaker_id,evidence_state) VALUES(19999,'c','assistant','','a','',  '','',1)`); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for range b.N {
		n, err := st.PruneChatHolders(context.Background(), 2000)
		if err != nil || n != 0 {
			b.Fatalf("pruned %d, %v", n, err)
		}
	}
}
