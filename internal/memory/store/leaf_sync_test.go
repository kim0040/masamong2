package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"math/rand"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/storage"
)

func leafSyncSnapshot(t testing.TB, st *Store, room string) leafRoomState {
	t.Helper()
	var snapshot leafRoomState
	if err := st.DB.Reader().QueryRow(leafRoomSnapshotQuery, room).Scan(&snapshot.revision, &snapshot.recipe, &snapshot.platform, &snapshot.evidenceRevision, &snapshot.naiveTimezone); err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func leafSyncObservation(i int, at time.Time, room string, naive bool) corpus.Observation {
	o := corpus.Observation{ObservationID: fmt.Sprintf("sync-o%09d", i), NativeMessageID: fmt.Sprintf("sync-m%09d", i),
		Platform: corpus.PlatformOfRoom(room), Collection: "synthetic", SourceRoom: room,
		ActorLabel: fmt.Sprintf("speaker-%d", i%3), Content: fmt.Sprintf("synthetic message %d for leaf sync", i),
		ParsedTime: at.UTC().Format(time.RFC3339Nano), RecordKind: corpus.HumanText}
	if naive {
		o.ParsedTime = at.Format("2006-01-02T15:04:05.999999999")
	}
	return o
}

type syncLeafRow struct {
	First, IDs, Body, Hash, Speakers, End string
	N                                     int
	Closed                                bool
}

// The oracle cuts the entire live corpus after every mutation, without using
// checkpoint metadata. Compare provenance and closure as well as rendered text:
// a matching body alone would miss overlap, duplicate text and idle-tail errors.
func assertFullLeafCut(t testing.TB, st *Store, room string, r leaves.Recipe, nowT time.Time, tz *time.Location, stats SyncStats) {
	t.Helper()
	msgs, err := st.Messages([]string{room})
	if err != nil {
		t.Fatal(err)
	}
	cut := leaves.SlicePerRoom(msgs, r, 1<<31-1, "leaf")
	want := map[string]syncLeafRow{}
	closedCount := 0
	for i, l := range cut {
		closed := i+1 < len(cut) || l.N >= r.Size || corpus.PlatformOfRoom(room) == "kakao"
		last := msgs[l.End-1]
		if !closed && r.Split == leaves.SplitTimeGap {
			closed = idleFor(last.ParsedTime, nowT, tz) >= time.Duration(r.GapMinutes*float64(time.Minute))
		}
		if closed {
			closedCount++
		}
		ids, _ := json.Marshal(l.ObservationIDs)
		sp, _ := json.Marshal(l.Speakers)
		want[LeafID(r.Version(), room, l.ObservationIDs[0])] = syncLeafRow{l.ObservationIDs[0], string(ids), l.Body, hashText(l.Body), string(sp), last.ParsedTime, l.N, closed}
	}
	rows, err := st.DB.Reader().Query(`SELECT leaf_id,first_observation_id,observation_ids,body,body_hash,speakers,end_time,n,closed
		FROM memory_leaves WHERE room=? AND recipe=?`, room, r.Version())
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]syncLeafRow{}
	for rows.Next() {
		var id string
		var row syncLeafRow
		if err := rows.Scan(&id, &row.First, &row.IDs, &row.Body, &row.Hash, &row.Speakers, &row.End, &row.N, &row.Closed); err != nil {
			t.Fatal(err)
		}
		got[id] = row
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("stored leaves differ from full cut: stored=%d full=%d", len(got), len(want))
	}
	if stats.Messages != len(msgs) || stats.Leaves != len(cut) || stats.Closed != closedCount {
		t.Fatalf("room totals changed: %+v; want messages=%d leaves=%d closed=%d", stats, len(msgs), len(cut), closedCount)
	}
}

func TestIncrementalLeafSyncEquivalent(t *testing.T) {
	recipes := []leaves.Recipe{leaves.DefaultRecipe(),
		{Size: 8, Split: leaves.SplitFixed, WithPrefix: true},
		{Size: 8, Overlap: 7, Split: leaves.SplitFixed, WithPrefix: true}}
	for _, platform := range []string{"discord", "kakao"} {
		for ri, initial := range recipes {
			t.Run(fmt.Sprintf("%s/recipe%d", platform, ri), func(t *testing.T) {
				st := openStore(t)
				rng := rand.New(rand.NewSource(int64(91823 + ri)))
				room := platform + ":sync-room"
				at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
				var all []corpus.Observation
				for i := 0; i < 64; i++ {
					all = append(all, leafSyncObservation(i, at.Add(time.Duration(i)*time.Minute), room, true))
				}
				if _, err := st.InsertObservations(all, nil); err != nil {
					t.Fatal(err)
				}
				latest := at.Add(63 * time.Minute)
				r := initial
				var tz *time.Location
				for step := 0; step < 120; step++ {
					nowT := latest.Add(time.Duration(rng.Intn(90)) * time.Minute)
					switch rng.Intn(8) {
					case 0, 1, 2: // appends, including min-size and large idle gaps
						for j, n := 0, 1+rng.Intn(10); j < n; j++ {
							latest = latest.Add(time.Duration(1+rng.Intn(60)) * time.Minute)
							o := leafSyncObservation(len(all), latest, room, true)
							all = append(all, o)
							if _, err := st.InsertObservations([]corpus.Observation{o}, nil); err != nil {
								t.Fatal(err)
							}
						}
					case 3: // late insertion, sometimes exactly on an existing time
						o := leafSyncObservation(len(all), at, room, true)
						o.ParsedTime = all[rng.Intn(len(all))].ParsedTime
						all = append(all, o)
						if _, err := st.InsertObservations([]corpus.Observation{o}, nil); err != nil {
							t.Fatal(err)
						}
					case 4:
						o := all[rng.Intn(len(all))]
						if _, _, err := st.EditMessage(platform, o.NativeMessageID, fmt.Sprintf("changed synthetic text %d", step), nowT.Format(time.RFC3339)); err != nil {
							t.Fatal(err)
						}
					case 5:
						o := all[rng.Intn(len(all))]
						if _, err := st.DeleteMessages(platform, []string{o.NativeMessageID}, nowT.Format(time.RFC3339)); err != nil {
							t.Fatal(err)
						}
					case 6:
						r.WithPrefix = !r.WithPrefix // Version() stays the same
					case 7:
						if tz == nil {
							tz = time.FixedZone("synthetic-zone", 9*3600)
						} else {
							tz = nil
						}
					}
					stats, err := st.SyncLeaves(room, r, nowT, tz)
					if err != nil {
						t.Fatalf("step %d: %v", step, err)
					}
					assertFullLeafCut(t, st, room, r, nowT, tz, stats)
				}
			})
		}
	}
}

func TestIncrementalLeafSyncReadsOnlyTail(t *testing.T) {
	st := openStore(t)
	room := "discord:sync-big"
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const count = 10000
	obs := make([]corpus.Observation, count)
	for i := range obs {
		obs[i] = leafSyncObservation(i, at.Add(time.Duration(i)*time.Second), room, false)
	}
	if _, err := st.InsertObservations(obs, nil); err != nil {
		t.Fatal(err)
	}
	r := leaves.DefaultRecipe()
	nowT := at.Add(time.Duration(count+10) * time.Second)
	if _, err := st.SyncLeaves(room, r, nowT, nil); err != nil {
		t.Fatal(err)
	}
	// Durable restart metadata must survive opening a fresh Store too.
	var err error
	st, err = Open(st.DB)
	if err != nil {
		t.Fatal(err)
	}
	var extra []corpus.Observation
	for i := count; i < count+10; i++ {
		extra = append(extra, leafSyncObservation(i, at.Add(time.Duration(i)*time.Second), room, false))
	}
	if _, err := st.InsertObservations(extra, nil); err != nil {
		t.Fatal(err)
	}
	msgs, plan, err := st.leafSyncMessages(room, r, nil, leafSyncSnapshot(t, st, room))
	if err != nil || !plan.incremental || len(msgs) != 26 || len(plan.tailIDs) != 2 || plan.prefixMessages != count-16 {
		t.Fatalf("tail read: rows=%d plan=%+v err=%v", len(msgs), plan, err)
	}
	// Prove the bounded tuple query seeks an index instead of scanning/offsetting
	// the room. This matters on an SD card even when few rows reach Go.
	key := messageKey(msgs[0])
	rows, err := st.DB.Reader().Query("EXPLAIN QUERY PLAN "+leafTailQuery, room, key.at, key.at, key.source, key.id)
	if err != nil {
		t.Fatal(err)
	}
	var details string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details += detail
	}
	rows.Close()
	if !strings.Contains(details, "SEARCH memory_observations USING INDEX idx_memory_obs_room_live") || !strings.Contains(details, "parsed_time>?") {
		t.Fatalf("tail query does not seek the live-room time range: %s", details)
	}
	q, args := leafSyncExistingQuery(room, r.Version(), plan)
	rows, err = st.DB.Reader().Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	details = ""
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details += detail
	}
	rows.Close()
	if !strings.Contains(details, "SEARCH memory_leaves USING INDEX sqlite_autoindex_memory_leaves_1 (leaf_id=?)") {
		t.Fatalf("tail hashes do not seek primary keys: %s", details)
	}
	firstID := LeafID(r.Version(), room, obs[0].ObservationID)
	if _, err := st.DB.DB.Exec(`CREATE TRIGGER protect_leaf_prefix BEFORE UPDATE ON memory_leaves WHEN OLD.leaf_id='` + firstID + `' BEGIN SELECT RAISE(ABORT,'prefix rewritten'); END`); err != nil {
		t.Fatal(err)
	}
	stats, err := st.SyncLeaves(room, r, nowT, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertFullLeafCut(t, st, room, r, nowT, nil, stats)
}

func TestIncrementalLeafSyncFallbacks(t *testing.T) {
	st := openStore(t)
	room := "discord:sync-fallback"
	r := leaves.DefaultRecipe()
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var obs []corpus.Observation
	for i := 0; i < 40; i++ {
		obs = append(obs, leafSyncObservation(i, at.Add(time.Duration(i)*time.Minute), room, true))
	}
	if _, err := st.InsertObservations(obs, nil); err != nil {
		t.Fatal(err)
	}
	tz := time.FixedZone("same-name", 9*3600)
	nowT := at.Add(time.Hour)
	if _, err := st.SyncLeaves(room, r, nowT, tz); err != nil {
		t.Fatal(err)
	}
	check := func(wantIncremental bool) {
		t.Helper()
		_, plan, err := st.leafSyncMessages(room, r, tz, leafSyncSnapshot(t, st, room))
		if err != nil || plan.incremental != wantIncremental {
			t.Fatalf("incremental=%v want %v (%v)", plan.incremental, wantIncremental, err)
		}
		stats, err := st.SyncLeaves(room, r, nowT, tz)
		if err != nil {
			t.Fatal(err)
		}
		assertFullLeafCut(t, st, room, r, nowT, tz, stats)
	}
	check(true)
	// Tie ordering is source then ID, not just time; merge two synthetic sources.
	late := leafSyncObservation(41, at.Add(20*time.Minute), "discord:source-z", true)
	if _, err := st.InsertObservations([]corpus.Observation{late}, map[string]string{late.SourceRoom: room}); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, _, err := st.EditMessage("discord", obs[4].NativeMessageID, "new text in the middle", ""); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, err := st.DeleteMessages("discord", []string{obs[0].NativeMessageID}, nowT.Format(time.RFC3339)); err != nil {
		t.Fatal(err)
	}
	check(false)
	r.WithPrefix = false
	check(false)
	// WHY caller zones are safe: the entire closure-sensitive suffix is recut.
	tz = time.FixedZone("same-name", -4*3600)
	check(true)
	r.Size = 3
	check(false)
	if err := st.DB.WithWriter(func(tx *sql.Tx) error {
		return st.UpsertRoom(tx, Room{Room: room, Platform: "discord", Collection: "synthetic", NaiveTimezone: "changed-declaration"})
	}); err != nil {
		t.Fatal(err)
	}
	check(false)
	if _, err := st.DeleteRoom(room); err != nil {
		t.Fatal(err)
	}
	if _, err := st.InsertObservations(obs[10:], nil); err != nil {
		t.Fatal(err)
	}
	check(false)
}

func TestIncrementalLeafSyncInterruptedBatch(t *testing.T) {
	st := openStore(t)
	room := "discord:sync-interrupted"
	r := leaves.Recipe{Size: 1, Split: leaves.SplitFixed, WithPrefix: true}
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	insert := func(lo, hi int) {
		t.Helper()
		var obs []corpus.Observation
		for i := lo; i < hi; i++ {
			obs = append(obs, leafSyncObservation(i, at.Add(time.Duration(i)*time.Second), room, false))
		}
		if _, err := st.InsertObservations(obs, nil); err != nil {
			t.Fatal(err)
		}
	}
	insert(0, 32)
	if _, err := st.SyncLeaves(room, r, at, nil); err != nil {
		t.Fatal(err)
	}
	insert(32, 32+syncBatch+10)
	failID := fmt.Sprintf("sync-o%09d", 32+syncBatch-2)
	if _, err := st.DB.DB.Exec(`CREATE TRIGGER fail_sync_suffix BEFORE INSERT ON memory_leaves WHEN NEW.first_observation_id='` + failID + `' BEGIN SELECT RAISE(ABORT,'injected suffix failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncLeaves(room, r, at, nil); err == nil {
		t.Fatal("second batch must fail")
	}
	var n int
	if err := st.DB.Reader().QueryRow(`SELECT COUNT(*) FROM memory_leaves WHERE room=?`, room).Scan(&n); err != nil || n <= 32 {
		t.Fatalf("first suffix batch was not durable: %d %v", n, err)
	}
	if _, err := st.DB.DB.Exec(`DROP TRIGGER fail_sync_suffix`); err != nil {
		t.Fatal(err)
	}
	_, plan, err := st.leafSyncMessages(room, r, nil, leafSyncSnapshot(t, st, room))
	if err != nil || plan.incremental {
		t.Fatalf("interrupted cut trusted a prefix: %+v %v", plan, err)
	}
	stats, err := st.SyncLeaves(room, r, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertFullLeafCut(t, st, room, r, at, nil, stats)
}

func TestIncrementalLeafSyncRejectsChangedSnapshot(t *testing.T) {
	st := openStore(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	room := "discord:sync-race"
	r := leaves.DefaultRecipe()
	var obs []corpus.Observation
	for i := 0; i < 32; i++ {
		obs = append(obs, leafSyncObservation(i, at.Add(time.Duration(i)*time.Second), room, false))
	}
	if _, err := st.InsertObservations(obs, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncLeaves(room, r, at, nil); err != nil {
		t.Fatal(err)
	}
	snapshot := leafSyncSnapshot(t, st, room)
	msgs, plan, err := st.leafSyncMessages(room, r, nil, snapshot)
	if err != nil || !plan.incremental {
		t.Fatalf("plan: %+v %v", plan, err)
	}
	if _, _, err := st.EditMessage("discord", obs[0].NativeMessageID, "changed prefix", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.syncLeavesFrom(room, r, at, nil, msgs, plan, snapshot); err == nil {
		t.Fatal("source change between planning and commit must be rejected")
	}
	stats, err := st.SyncLeaves(room, r, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertFullLeafCut(t, st, room, r, at, nil, stats)
}

// Seed once outside the timer, then measure a real pass after ten new rows. Full
// uses the same reconciliation code with checkpoint reuse disabled, isolating
// the cost of room reads/cutting rather than APIs, imports or vector generation.
func BenchmarkSyncLeaves100KAppend10(b *testing.B) {
	for _, mode := range []string{"full", "incremental"} {
		b.Run(mode, func(b *testing.B) {
			db, err := storage.Open(filepath.Join(b.TempDir(), "sync.db"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = db.Close() })
			st, err := Open(db)
			if err != nil {
				b.Fatal(err)
			}
			room := "discord:sync-benchmark"
			at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			r := leaves.DefaultRecipe()
			var obs []corpus.Observation
			for i := 0; i < 100000; i++ {
				obs = append(obs, leafSyncObservation(i, at.Add(time.Duration(i)*time.Second), room, false))
			}
			if _, err := st.InsertObservations(obs, nil); err != nil {
				b.Fatal(err)
			}
			if _, err := st.SyncLeaves(room, r, at, nil); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for n := 0; n < b.N; n++ {
				b.StopTimer()
				var extra []corpus.Observation
				for i := 100000 + n*10; i < 100010+n*10; i++ {
					extra = append(extra, leafSyncObservation(i, at.Add(time.Duration(i)*time.Second), room, false))
				}
				if _, err := st.InsertObservations(extra, nil); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if mode == "full" {
					snapshot := leafSyncSnapshot(b, st, room)
					msgs, err := st.Messages([]string{room})
					if err != nil {
						b.Fatal(err)
					}
					if _, err := st.syncLeaves(room, r, at, nil, msgs, snapshot); err != nil {
						b.Fatal(err)
					}
				} else if _, err := st.SyncLeaves(room, r, at, nil); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
