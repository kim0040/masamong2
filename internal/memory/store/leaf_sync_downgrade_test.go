package store

import (
	"database/sql"
	"fmt"
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
)

// rawOldBinaryInsert mimics an older binary: inserts the row + bumps the room revision
// but never touches memory_leaf_sync. Use the old SQL, not the new revision hook,
// so this remains a real downgrade even after instrumentation is added.
func rawOldBinaryInsert(t *testing.T, st *Store, o corpus.Observation) {
	t.Helper()
	m := o.Message(nil)
	err := st.DB.WithWriter(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`INSERT INTO memory_observations(observation_id, platform, collection, source_room, room, native_message_id, actor_label,
			actor_account, content, parsed_time, record_kind, imported_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, o.ObservationID, o.Platform, o.Collection, o.SourceRoom, m.Room,
			o.NativeMessageID, m.ActorLabel, m.ActorAccount, o.Content, o.ParsedTime, o.RecordKind, now()); err != nil {
			return err
		}
		if _, err := tx.Exec(`UPDATE memory_revision_sequence SET revision=revision+1 WHERE id=1`); err != nil {
			return err
		}
		_, err := tx.Exec(`UPDATE memory_rooms SET revision=(SELECT revision FROM memory_revision_sequence WHERE id=1),updated_at=?,pending_sync=1 WHERE room=?`, now(), m.Room)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
}

// Preserve every checkpoint column, including future guards: an old binary's
// leaf sync does not know this table and must leave the entire checkpoint alone.
type savedLeafCheckpoint struct {
	columns []string
	values  []any
}

func saveCP(t *testing.T, st *Store, room string) savedLeafCheckpoint {
	t.Helper()
	rows, err := st.DB.Reader().Query(`SELECT * FROM memory_leaf_sync WHERE room=?`, room)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	columns, err := rows.Columns()
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("missing checkpoint")
	}
	c := savedLeafCheckpoint{columns: columns, values: make([]any, len(columns))}
	dest := make([]any, len(columns))
	for i := range dest {
		dest[i] = &c.values[i]
	}
	if err := rows.Scan(dest...); err != nil {
		t.Fatal(err)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return c
}

func restoreCP(t *testing.T, st *Store, room string, c savedLeafCheckpoint) {
	t.Helper()
	sets := make([]string, len(c.columns))
	for i, column := range c.columns {
		sets[i] = column + "=?"
	}
	args := append(append([]any{}, c.values...), room)
	if _, err := st.DB.DB.Exec(`UPDATE memory_leaf_sync SET `+strings.Join(sets, ",")+` WHERE room=?`, args...); err != nil {
		t.Fatal(err)
	}
}

// Old binary scenarios. kind: "prefix" = back-filled row before the restart point; "append" = strict append;
// "tailmid" = late row inside the saved tail.
func TestLeafSyncDowngrade(t *testing.T) {
	recipes := []leaves.Recipe{leaves.DefaultRecipe(), {Size: 8, Split: leaves.SplitFixed, WithPrefix: true}, {Size: 8, Overlap: 7, Split: leaves.SplitFixed, WithPrefix: true}}
	for _, kind := range []string{"prefix", "append", "tailmid"} {
		for _, withOldSync := range []bool{false, true} {
			for ri, r := range recipes {
				t.Run(fmt.Sprintf("%s/oldsync=%v/recipe%d", kind, withOldSync, ri), func(t *testing.T) {
					st := openStore(t)
					room := "discord:1:dg"
					at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
					var all []corpus.Observation
					for i := 0; i < 64; i++ {
						all = append(all, leafSyncObservation(i, at.Add(time.Duration(i)*time.Minute), room, false))
					}
					if _, err := st.InsertObservations(all, nil); err != nil {
						t.Fatal(err)
					}
					nowT := at.Add(24 * time.Hour)
					if _, err := st.SyncLeaves(room, r, nowT, nil); err != nil {
						t.Fatal(err)
					}
					for i := 64; i < 69; i++ {
						o := leafSyncObservation(i, at.Add(time.Duration(i)*time.Minute), room, false)
						if _, err := st.InsertObservations([]corpus.Observation{o}, nil); err != nil {
							t.Fatal(err)
						}
					}
					if _, err := st.SyncLeaves(room, r, nowT, nil); err != nil {
						t.Fatal(err)
					}
					cp := saveCP(t, st, room)
					var o corpus.Observation
					switch kind {
					case "prefix":
						o = leafSyncObservation(1000, at.Add(10*time.Minute+30*time.Second), room, false)
					case "append":
						o = leafSyncObservation(1000, at.Add(80*time.Minute), room, false)
					case "tailmid":
						o = leafSyncObservation(1000, at.Add(66*time.Minute+30*time.Second), room, false)
					}
					rawOldBinaryInsert(t, st, o)
					if withOldSync { // old binary's own full leaf sync
						if _, err := st.DB.DB.Exec(`UPDATE memory_leaf_sync SET valid=0 WHERE room=?`, room); err != nil {
							t.Fatal(err)
						}
						if _, err := st.SyncLeaves(room, r, nowT, nil); err != nil {
							t.Fatal(err)
						}
						restoreCP(t, st, room, cp)
					}
					stats, err := st.SyncLeaves(room, r, nowT, nil)
					if err != nil {
						t.Fatal(err)
					}
					assertFullLeafCut(t, st, room, r, nowT, nil, stats)
				})
			}
		}
	}
}

// New API: late arrival with equal parsed_time but smaller observation_id than the current last message.
func TestLeafSyncEqualTimeSmallerID(t *testing.T) {
	recipes := []leaves.Recipe{leaves.DefaultRecipe(), {Size: 8, Split: leaves.SplitFixed, WithPrefix: true}, {Size: 8, Overlap: 7, Split: leaves.SplitFixed, WithPrefix: true}}
	for ri, r := range recipes {
		t.Run(fmt.Sprintf("recipe%d", ri), func(t *testing.T) {
			st := openStore(t)
			room := "discord:1:dg"
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			var all []corpus.Observation
			for i := 10; i < 74; i++ {
				all = append(all, leafSyncObservation(i, at.Add(time.Duration(i)*time.Minute), room, false))
			}
			if _, err := st.InsertObservations(all, nil); err != nil {
				t.Fatal(err)
			}
			nowT := at.Add(24 * time.Hour)
			if _, err := st.SyncLeaves(room, r, nowT, nil); err != nil {
				t.Fatal(err)
			}
			// same time as last message, smaller ID
			o := leafSyncObservation(5, at.Add(73*time.Minute), room, false)
			if _, err := st.InsertObservations([]corpus.Observation{o}, nil); err != nil {
				t.Fatal(err)
			}
			stats, err := st.SyncLeaves(room, r, nowT, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertFullLeafCut(t, st, room, r, nowT, nil, stats)
		})
	}
}

// WHY a normal capture follows the rollback: advancing observed_revision from
// the checkpoint instead of the previous room revision would hide the gap.
func TestLeafSyncDowngradeThenInstrumentedAppend(t *testing.T) {
	st := openStore(t)
	room := "discord:1:rollback-chain"
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := leaves.DefaultRecipe()
	var obs []corpus.Observation
	for i := 0; i < 64; i++ {
		obs = append(obs, leafSyncObservation(i, at.Add(time.Duration(i)*time.Minute), room, false))
	}
	if _, err := st.InsertObservations(obs, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncLeaves(room, r, at, nil); err != nil {
		t.Fatal(err)
	}
	rawOldBinaryInsert(t, st, leafSyncObservation(1000, at.Add(30*time.Second), room, false))
	if _, err := st.InsertObservations([]corpus.Observation{leafSyncObservation(1001, at.Add(80*time.Minute), room, false)}, nil); err != nil {
		t.Fatal(err)
	}
	_, plan, err := st.leafSyncMessages(room, r, nil, leafSyncSnapshot(t, st, room))
	if err != nil || plan.incremental {
		t.Fatalf("rollback revision gap was bridged: %+v %v", plan, err)
	}
	stats, err := st.SyncLeaves(room, r, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertFullLeafCut(t, st, room, r, at, nil, stats)
}

// WHY do not use SyncLeaves to simulate these writes: the old binary never
// invalidated a checkpoint, and its leaf sync did not advance room.revision.
// A round trip must remain visible even when values return to their old state.
func TestLeafSyncOldLeafMutationRoundTrip(t *testing.T) {
	for _, kind := range []string{"WithPrefix", "recipe", "replace", "delete"} {
		t.Run(kind, func(t *testing.T) {
			st := openStore(t)
			room := "discord:1:old-leaves"
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			r := leaves.DefaultRecipe()
			var obs []corpus.Observation
			for i := 0; i < 64; i++ {
				obs = append(obs, leafSyncObservation(i, at.Add(time.Duration(i)*time.Minute), room, false))
			}
			if _, err := st.InsertObservations(obs, nil); err != nil {
				t.Fatal(err)
			}
			if _, err := st.SyncLeaves(room, r, at, nil); err != nil {
				t.Fatal(err)
			}
			id := LeafID(r.Version(), room, obs[0].ObservationID)
			var queries []string
			switch kind {
			case "WithPrefix":
				queries = []string{`UPDATE memory_leaves SET body=body||'synthetic suffix' WHERE leaf_id=?`, `UPDATE memory_leaves SET body=substr(body,1,length(body)-length('synthetic suffix')) WHERE leaf_id=?`}
			case "recipe":
				queries = []string{`UPDATE memory_rooms SET active_recipe='old-recipe' WHERE room=?`, `UPDATE memory_rooms SET active_recipe=? WHERE room=?`}
			case "replace":
				queries = []string{`UPDATE memory_leaves SET observation_ids='[]' WHERE leaf_id=?`}
			case "delete":
				queries = []string{`DELETE FROM memory_leaves WHERE leaf_id=?`}
			}
			for i, q := range queries {
				args := []any{id}
				if kind == "recipe" {
					args = []any{room}
					if i == 1 {
						args = []any{r.Version(), room}
					}
				}
				if _, err := st.DB.DB.Exec(q, args...); err != nil {
					t.Fatal(err)
				}
			}
			_, plan, err := st.leafSyncMessages(room, r, nil, leafSyncSnapshot(t, st, room))
			if err != nil || plan.incremental {
				t.Fatalf("old leaf mutation reused its prefix: %+v %v", plan, err)
			}
			stats, err := st.SyncLeaves(room, r, at, nil)
			if err != nil {
				t.Fatal(err)
			}
			assertFullLeafCut(t, st, room, r, at, nil, stats)
		})
	}
}

// WHY merged Kakao exports: source_room precedes observation_id in corpus
// order. Smaller IDs in a later source still append, while an earlier source
// with larger IDs must take the full path at the same parsed_time.
func TestLeafSyncKakaoMergeSourceOrder(t *testing.T) {
	for _, r := range []leaves.Recipe{leaves.DefaultRecipe(), {Size: 8, Split: leaves.SplitFixed, WithPrefix: true}, {Size: 8, Overlap: 7, Split: leaves.SplitFixed, WithPrefix: true}} {
		t.Run(r.Version(), func(t *testing.T) {
			st := openStore(t)
			room := "kakao:merged"
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			var obs []corpus.Observation
			for i := 10; i < 74; i++ {
				obs = append(obs, leafSyncObservation(i, at, "kakao:source-m", true))
			}
			merge := map[string]string{"kakao:source-a": room, "kakao:source-m": room, "kakao:source-z": room}
			if _, err := st.InsertObservations(obs, merge); err != nil {
				t.Fatal(err)
			}
			if _, err := st.SyncLeaves(room, r, at, nil); err != nil {
				t.Fatal(err)
			}
			for _, tc := range []struct {
				source      string
				id          int
				incremental bool
			}{{"kakao:source-z", 5, true}, {"kakao:source-a", 1000, false}} {
				if _, err := st.InsertObservations([]corpus.Observation{leafSyncObservation(tc.id, at, tc.source, true)}, merge); err != nil {
					t.Fatal(err)
				}
				_, plan, err := st.leafSyncMessages(room, r, nil, leafSyncSnapshot(t, st, room))
				if err != nil || plan.incremental != tc.incremental {
					t.Fatalf("source ordering changed: %+v %v", plan, err)
				}
				stats, err := st.SyncLeaves(room, r, at, nil)
				if err != nil {
					t.Fatal(err)
				}
				assertFullLeafCut(t, st, room, r, at, nil, stats)
			}
		})
	}
}
