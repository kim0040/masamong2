package store

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
)

func TestReviewAliasWriteRollback(t *testing.T) {
	s := openStore(t)
	o := mk(0, time.Now())
	o.ActorAccount = "101"
	if _, err := s.DB.DB.Exec(`CREATE TRIGGER fixture_failure BEFORE INSERT ON person_alias BEGIN SELECT RAISE(ROLLBACK,'fixture'); END`); err != nil {
		t.Fatal(err)
	}
	res, err := s.InsertObservations([]corpus.Observation{o}, nil)
	if err != nil || res.Inserted != 1 {
		t.Fatalf("capture %+v: %v", res, err)
	}
	var count int
	if err := s.DB.Reader().QueryRow(`SELECT count(*) FROM memory_observations WHERE observation_id=? AND actor_account=?`, o.ObservationID, o.ActorAccount).Scan(&count); err != nil || count != 1 {
		t.Fatalf("sources %d: %v", count, err)
	}
	if _, err := s.DB.DB.Exec(`DROP TRIGGER fixture_failure; CREATE TABLE IF NOT EXISTS consent_state(user_id TEXT,scope TEXT,status TEXT); INSERT INTO consent_state(user_id,scope,status) VALUES('101','memory','withdrawn')`); err != nil {
		t.Fatal(err)
	}
	check := func() {
		t.Helper()
		if err := indexCapturedAliases(t.Context(), s.DB, []string{o.ObservationID}); err != nil {
			t.Fatal(err)
		}
		if err := s.DB.Reader().QueryRow(`SELECT count(*) FROM person_alias WHERE person_ref=?`, PersonRef(o.ActorAccount)).Scan(&count); err != nil || count != 0 {
			t.Fatalf("alias resurrected %d: %v", count, err)
		}
	}
	check()
	if _, err := s.DB.DB.Exec(`DELETE FROM consent_state; DELETE FROM memory_observations`); err != nil {
		t.Fatal(err)
	}
	check()
}

// TestReviewAliasMigration200k runs the migration over 200,000 stored messages, which takes over a minute,
// so only MASAMONG_PERF_TESTS=1 does that (one CI job sets it). Without it the same path runs at a tenth of the
// size, as on Windows and under the race detector (a Windows CI runner needs minutes per 10,000 rows, and
// the race detector slows the fill several times over).
func TestReviewAliasMigration200k(t *testing.T) {
	size := 20000
	if os.Getenv("MASAMONG_PERF_TESTS") != "" && runtime.GOOS != "windows" && !raceDetector {
		size = 200000
	}
	reviewAliasMigration(t, size, 90*time.Second)
}

// Run the reviewer's full-sized reproduction with -benchtime=1x.
func BenchmarkReviewAliasMigration550k(b *testing.B) {
	for range b.N {
		reviewAliasMigration(b, 550000, 5*time.Minute)
	}
}

func reviewAliasMigration(t testing.TB, size int, bound time.Duration) {
	db := openStore(t).DB
	rows, err := db.Reader().Query(`EXPLAIN QUERY PLAN `+aliasRangeSQL+eligibleAliasSQL, 512, 1024, corpus.HumanText)
	if err != nil {
		t.Fatal(err)
	}
	var bounded bool
	for rows.Next() {
		var a, b, c int
		var text string
		if err := rows.Scan(&a, &b, &c, &text); err != nil {
			t.Fatal(err)
		}
		bounded = bounded || strings.Contains(text, "INTEGER PRIMARY KEY (rowid>? AND rowid<?)")
	}
	if err := rows.Close(); err != nil || !bounded {
		t.Fatalf("not a rowid range: %t %v", bounded, err)
	}
	if _, err := db.DB.Exec(`WITH RECURSIVE ids(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM ids WHERE i<?)
		INSERT INTO memory_observations(observation_id,platform,collection,source_room,room,native_message_id,actor_label,actor_account,content,parsed_time,record_kind,imported_at)
		SELECT 'o'||i,'discord','discord','discord:1:'||(i%20),'discord:1:'||(i%20),'m'||i,'Person'||(i%1000),'u'||(i%1000),'fixture','2026-10-01T00:00:00Z','human_text','2026-10-01T00:00:00Z' FROM ids`, size); err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`DELETE FROM person_alias_fill; DROP INDEX idx_memory_obs_person_name`); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if err := migratePersonAliases(db); err != nil {
		t.Fatal(err)
	}
	if personNameIndexed(t, db) {
		t.Fatal("startup built the name index over a stored history")
	}
	var cursor int
	checkCursor := func(want int) {
		t.Helper()
		if err := db.Reader().QueryRow(`SELECT cursor FROM person_alias_fill`).Scan(&cursor); err != nil || cursor != want {
			t.Fatalf("cursor=%d want %d: %v", cursor, want, err)
		}
	}
	checkCursor(0) // opening the schema did not fill any historical rows
	if done, err := fillAliasBatch(t.Context(), db); err != nil || done {
		t.Fatalf("batch: %v %v", done, err)
	}
	checkCursor(512)
	if err := migratePersonAliases(db); err != nil {
		t.Fatal(err)
	}
	checkCursor(512)
	if err := FillPersonAliases(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	t.Logf("fill of %d rows: %v", size, elapsed)
	// The bound is checked where disk syncs are cheap and nothing instruments the code. Windows CI
	// runners took about 3 s a 512-row batch (19 minutes at 200,000 rows) and -race takes over two
	// minutes at 200,000; the fill runs in the background there too, so only its correctness is checked.
	if elapsed > bound && runtime.GOOS != "windows" && !raceDetector {
		t.Fatalf("fill exceeded %v: %v", bound, elapsed)
	}
	checkCursor(-1)
	if !personNameIndexed(t, db) {
		t.Fatal("background fill did not build the name index")
	}
	var n int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM person_alias WHERE guild_id='1'`).Scan(&n); err != nil || n != 1000 {
		t.Fatalf("aliases=%d: %v", n, err)
	}
	if err := db.Reader().QueryRow(`SELECT count(*) FROM person_alias_source WHERE person_ref=?`, PersonRef("u1")).Scan(&n); err != nil || n != size/1000 {
		t.Fatalf("sources=%d: %v", n, err)
	}
	start = time.Now()
	if err := FillPersonAliases(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	t.Logf("%d rows: %v; completed replay: %v", size, elapsed, time.Since(start))
}
