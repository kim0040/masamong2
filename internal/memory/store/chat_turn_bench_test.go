package store

import (
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
