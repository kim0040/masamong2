package store

import (
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
)

func TestReviewAliasMigration550k(t *testing.T) {
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
	for _, q := range []string{
		`WITH RECURSIVE ids(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM ids WHERE i<550000)
		INSERT INTO memory_observations(observation_id,platform,collection,source_room,room,native_message_id,actor_label,actor_account,content,parsed_time,record_kind,imported_at)
		SELECT 'o'||i,'discord','discord','discord:1:'||(i%20),'discord:1:'||(i%20),'m'||i,'Person'||(i%1000),'u'||(i%1000),'fixture','2026-10-01T00:00:00Z','human_text','2026-10-01T00:00:00Z' FROM ids`,
		`DELETE FROM person_alias_fill`,
	} {
		if _, err := db.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	start := time.Now()
	if err := migratePersonAliases(db); err != nil {
		t.Fatal(err)
	}
	var cursor int
	checkCursor := func(want int) {
		t.Helper()
		if err := db.Reader().QueryRow(`SELECT cursor FROM person_alias_fill`).Scan(&cursor); err != nil || cursor != want {
			t.Fatalf("cursor=%d want %d: %v", cursor, want, err)
		}
	}
	checkCursor(0) // opening the schema did not read the 550,000 historical rows
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
	if elapsed > 150*time.Second {
		t.Fatalf("fill exceeded 150s: %v", elapsed)
	}
	checkCursor(-1)
	var n int
	if err := db.Reader().QueryRow(`SELECT count(*) FROM person_alias WHERE guild_id='1'`).Scan(&n); err != nil || n != 1000 {
		t.Fatalf("aliases=%d: %v", n, err)
	}
	if err := db.Reader().QueryRow(`SELECT count(*) FROM person_alias_source WHERE person_ref=?`, PersonRef("u1")).Scan(&n); err != nil || n != 550 {
		t.Fatalf("sources=%d: %v", n, err)
	}
	start = time.Now()
	if err := FillPersonAliases(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	t.Logf("550,000 rows: %v; completed replay: %v", elapsed, time.Since(start))
}
