package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/storage"
)

// WHY a driver barrier: release the real SQLite transaction, then pause exactly
// after checkpoint invalidation. A second Store can capture during SyncLeaves
// without sleeps, scheduling assumptions or hooks in production code.
type checkpointBarrierDriver struct {
	driver.Driver
	committed, resume chan struct{}
	once              sync.Once
}

func (d *checkpointBarrierDriver) Open(name string) (driver.Conn, error) {
	c, err := d.Driver.Open(name)
	if err != nil {
		return nil, err
	}
	return &checkpointBarrierConn{Conn: c, barrier: d}, nil
}

type checkpointBarrierConn struct {
	driver.Conn
	barrier     *checkpointBarrierDriver
	invalidated bool
}

func (c *checkpointBarrierConn) ExecContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Result, error) {
	r, err := c.Conn.(driver.ExecerContext).ExecContext(ctx, q, args)
	if err == nil && strings.HasPrefix(q, "UPDATE memory_leaf_sync SET valid=0") {
		c.invalidated = true
	}
	return r, err
}

func (c *checkpointBarrierConn) QueryContext(ctx context.Context, q string, args []driver.NamedValue) (driver.Rows, error) {
	return c.Conn.(driver.QueryerContext).QueryContext(ctx, q, args)
}

func (c *checkpointBarrierConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	c.invalidated = false
	tx, err := c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
	if err != nil {
		return nil, err
	}
	return &checkpointBarrierTx{Tx: tx, conn: c}, nil
}

type checkpointBarrierTx struct {
	driver.Tx
	conn *checkpointBarrierConn
}

func (tx *checkpointBarrierTx) Commit() error {
	if err := tx.Tx.Commit(); err != nil {
		return err
	}
	if tx.conn.invalidated {
		tx.conn.barrier.once.Do(func() {
			close(tx.conn.barrier.committed)
			<-tx.conn.barrier.resume
		})
	}
	return nil
}

func TestLeafSyncConcurrentInsertObservations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.db")
	db, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	otherDB, err := storage.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = otherDB.Close() })
	other, err := Open(otherDB)
	if err != nil {
		t.Fatal(err)
	}
	room := "discord:sync-concurrent"
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
	barrier := &checkpointBarrierDriver{Driver: db.DB.Driver(), committed: make(chan struct{}), resume: make(chan struct{})}
	name := fmt.Sprintf("checkpoint-barrier-%d", checkpointBarrierSequence.Add(1))
	sql.Register(name, barrier)
	pool, err := sql.Open(name, path+"?_txlock=immediate&_pragma=busy_timeout(100)")
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	if err := db.DB.Close(); err != nil {
		t.Fatal(err)
	}
	db.DB = pool
	// Always release/join the sync goroutine, including a failed assertion.
	done := make(chan error, 1)
	go func() { _, err := st.SyncLeaves(room, r, at, nil); done <- err }()
	released, joined := false, false
	defer func() {
		if !released {
			close(barrier.resume)
		}
		if !joined {
			<-done
		}
	}()
	select {
	case <-barrier.committed:
	case <-time.After(10 * time.Second):
		t.Fatal("sync never invalidated the checkpoint")
	}
	if _, err := other.InsertObservations([]corpus.Observation{leafSyncObservation(64, at.Add(64*time.Minute), room, false)}, nil); err != nil {
		t.Fatal(err)
	}
	// Unblock/join explicitly before attempting the recovery pass.
	close(barrier.resume)
	released = true
	err = <-done
	joined = true
	if err == nil || !strings.Contains(err.Error(), "source changed") {
		t.Fatalf("sync published concurrent capture: %v", err)
	}
	var valid bool
	if err := st.DB.Reader().QueryRow(`SELECT valid FROM memory_leaf_sync WHERE room=?`, room).Scan(&valid); err != nil || valid {
		t.Fatalf("concurrent sync left a trusted checkpoint: %v %v", valid, err)
	}
	_, plan, err := st.leafSyncMessages(room, r, nil, leafSyncSnapshot(t, st, room))
	if err != nil || plan.incremental {
		t.Fatalf("recovery must use a full cut: %+v %v", plan, err)
	}
	stats, err := st.SyncLeaves(room, r, at, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertFullLeafCut(t, st, room, r, at, nil, stats)
}

// WHY preserve fallback availability: optional schema failure must neither stop
// Open nor cause later inserts, edits, cuts or room deletion to query that table.
func TestLeafSyncMigrationFailureKeepsFullPath(t *testing.T) {
	st := openStore(t)
	for _, name := range []string{"memory_leaf_sync_insert", "memory_leaf_sync_delete", "memory_leaf_sync_update", "memory_leaf_sync_recipe"} {
		if _, err := st.DB.DB.Exec(`DROP TRIGGER IF EXISTS ` + name); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := st.DB.DB.Exec(`DROP TABLE memory_leaf_sync`); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DB.DB.Exec(`CREATE VIEW memory_leaf_sync AS SELECT 1 AS unavailable`); err != nil {
		t.Fatal(err)
	}
	st, err := Open(st.DB)
	if err != nil {
		t.Fatalf("optional checkpoint migration stopped Open: %v", err)
	}
	room := "discord:sync-fallback-migration"
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := leaves.DefaultRecipe()
	o := leafSyncObservation(0, at, room, false)
	if _, err := st.InsertObservations([]corpus.Observation{o}, nil); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		stats, err := st.SyncLeaves(room, r, at, nil)
		if err != nil {
			t.Fatal(err)
		}
		assertFullLeafCut(t, st, room, r, at, nil, stats)
	}
	if _, _, err := st.EditMessage("discord", o.NativeMessageID, "synthetic edit", ""); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteMessages("discord", []string{o.NativeMessageID}, now()); err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteRoom(room); err != nil {
		t.Fatal(err)
	}
}

func TestLeafSyncLegacyCheckpointRequiresFullCut(t *testing.T) {
	st := openStore(t)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	room := "discord:legacy-checkpoint"
	r := leaves.DefaultRecipe()
	if _, err := st.InsertObservations([]corpus.Observation{leafSyncObservation(0, at, room, false)}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncLeaves(room, r, at, nil); err != nil {
		t.Fatal(err)
	}
	// WHY rebuild the old schema: existing dev checkpoints lack the new chain,
	// even though their valid flag and recipe already match this binary.
	if _, err := st.DB.DB.Exec(`ALTER TABLE memory_leaf_sync DROP COLUMN observed_revision`); err != nil {
		t.Fatal(err)
	}
	st, err := Open(st.DB)
	if err != nil {
		t.Fatal(err)
	}
	_, plan, err := st.leafSyncMessages(room, r, nil, leafSyncSnapshot(t, st, room))
	if err != nil || plan.incremental {
		t.Fatalf("legacy checkpoint reused: %+v %v", plan, err)
	}
	if _, err := st.SyncLeaves(room, r, at, nil); err != nil {
		t.Fatal(err)
	}
	_, plan, err = st.leafSyncMessages(room, r, nil, leafSyncSnapshot(t, st, room))
	if err != nil || !plan.incremental {
		t.Fatalf("rebuilt checkpoint not reusable: %+v %v", plan, err)
	}
}

func explainLeafSync(t *testing.T, st *Store, q string, args ...any) string {
	t.Helper()
	rows, err := st.DB.Reader().Query("EXPLAIN QUERY PLAN "+q, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "; ")
}

// WHY test the old dev index as well as a fresh database: upgrades must reclaim
// its disk space and restore console covering-index selection automatically.
func TestLeafSyncUsesExistingRoomLiveIndex(t *testing.T) {
	st := openStore(t)
	if _, err := st.DB.DB.Exec(`CREATE INDEX IF NOT EXISTS idx_memory_obs_leaf_tail ON memory_observations(room,deleted_at,parsed_time,source_room,observation_id)`); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(st.DB); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := st.DB.Reader().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='idx_memory_obs_leaf_tail'`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("dev index retained: %d %v", n, err)
	}
	for _, tc := range []struct {
		name, q string
		args    []any
		search  bool
	}{
		{"tail", leafTailQuery, []any{"synthetic-room", "2026-01-01", "2026-01-01", "synthetic-source", "synthetic-id"}, true},
		{"Rooms", `SELECT r.room,(SELECT COUNT(*) FROM memory_observations o WHERE o.room=r.room AND o.deleted_at='') FROM memory_rooms r ORDER BY r.room`, nil, true},
		{"RoomSpans", `SELECT room,MIN(parsed_time),MAX(parsed_time),COUNT(DISTINCT actor_label) FROM memory_observations WHERE deleted_at='' GROUP BY room`, nil, false},
		{"Counts", `SELECT COUNT(*) FROM memory_observations WHERE deleted_at=''`, nil, false},
	} {
		detail := explainLeafSync(t, st, tc.q, tc.args...)
		t.Logf("%s: %s", tc.name, detail)
		if !strings.Contains(detail, "idx_memory_obs_room_live") || (tc.search && !strings.Contains(detail, "SEARCH")) {
			t.Fatalf("%s lost room_live: %s", tc.name, detail)
		}
		if tc.name == "tail" && !strings.Contains(detail, "parsed_time>?") {
			t.Fatalf("tail lost its time seek: %s", detail)
		}
	}
}

// WHY test closure, not just reuse: different offsets (even with the same zone
// name) can reopen the idle tail. Prefix position and body stay timezone-free.
func TestLeafSyncAlternatingTimezoneReusesPrefix(t *testing.T) {
	st := openStore(t)
	room := "discord:timezone-tail"
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	r := leaves.DefaultRecipe()
	var obs []corpus.Observation
	for i := 0; i < 65; i++ {
		obs = append(obs, leafSyncObservation(i, at.Add(time.Duration(i)*time.Minute), room, true))
	}
	if _, err := st.InsertObservations(obs, nil); err != nil {
		t.Fatal(err)
	}
	nowT := at.Add(90 * time.Minute)
	for i, tz := range []*time.Location{time.FixedZone("room", 9*3600), time.FixedZone("instance", -4*3600), time.FixedZone("room", -4*3600), nil, time.FixedZone("room", 9*3600)} {
		if i > 0 {
			_, plan, err := st.leafSyncMessages(room, r, tz, leafSyncSnapshot(t, st, room))
			if err != nil || !plan.incremental {
				t.Fatalf("zone %d forced a full cut: %+v %v", i, plan, err)
			}
		}
		stats, err := st.SyncLeaves(room, r, nowT, tz)
		if err != nil {
			t.Fatal(err)
		}
		assertFullLeafCut(t, st, room, r, nowT, tz, stats)
	}
}

// Keep driver names unique across repeated in-process test runs.
var checkpointBarrierSequence atomic.Uint64
