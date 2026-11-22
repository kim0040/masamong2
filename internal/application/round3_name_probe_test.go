package application

import (
	"strings"
	"testing"
	"time"

	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

// A plain name fails closed while the name index is missing, a captured
// message of another account is not yet indexed, or the historical fill failed.
func TestRound3NameAvailability(t *testing.T) {
	s := resolverService(t)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", AuthorID: "101",
		AuthorName: "민수", MessageID: "visible", Content: "fixture", At: time.Now()}); !ok || err != nil {
		t.Fatal(ok, err)
	}
	in := ChatInput{GuildID: "g", ChannelID: "c", SpeakerID: "303"}
	check := func(msg string, want int) {
		t.Helper()
		in.Message = msg
		if got := s.targetPeople(s.Store, in, nil); len(got) != want || want > 0 && got[0].ref != personRef("101") {
			t.Fatalf("%q: %v, want %d results for 101", msg, got, want)
		}
	}
	exec := func(q string) {
		t.Helper()
		if _, err := s.Store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	check("민수는?", 1)
	exec(`DROP INDEX idx_memory_obs_person_name`)
	check("민수는?", 0)
	check("<@101>?", 1)
	if err := store.FillPersonAliases(t.Context(), s.Store); err != nil {
		t.Fatal(err)
	}
	check("민수는?", 1)
	exec(`INSERT INTO memory_observations(observation_id,platform,collection,source_room,room,native_message_id,actor_label,actor_account,content,parsed_time,record_kind,imported_at)
        VALUES('pending','discord','discord','discord:g:c','discord:g:c','pending','민수','202','fixture','2026-10-01T00:00:00Z','human_text','2026-10-01T00:00:00Z')`)
	check("민수는?", 0)
	check("<@101>?", 1)
	exec(`UPDATE person_alias_fill SET cursor=0;
        CREATE TRIGGER r3_fill_failure BEFORE INSERT ON person_alias_source
        WHEN new.observation='pending' BEGIN SELECT RAISE(ROLLBACK,'round3 fill failure'); END`)
	check("민수는?", 0)
	check("<@101>?", 1)
	if err := store.FillPersonAliases(t.Context(), s.Store); err == nil || !strings.Contains(err.Error(), "round3 fill failure") {
		t.Fatalf("fill fault did not fire: %v", err)
	}
	check("민수는?", 0)
	check("<@101>?", 1)
}

// 200,000 deleted rows under the same name, account and room cost the name
// check nothing: the index holds live rows only, and building it rewrites no row.
func TestRound3LargeNameIndex(t *testing.T) {
	s := resolverService(t)
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", AuthorID: "101",
		AuthorName: "민수", MessageID: "visible", Content: "fixture", At: time.Now()}); !ok || err != nil {
		t.Fatal(ok, err)
	}
	in := ChatInput{GuildID: "g", ChannelID: "c", SpeakerID: "303", Message: "민수는?"}
	measure := func(n int) time.Duration {
		t.Helper()
		start := time.Now()
		for range n {
			if got := s.targetPeople(s.Store, in, nil); len(got) != 1 || got[0].ref != personRef("101") {
				t.Fatalf("non-live rows should not create ambiguity: %v", got)
			}
		}
		return time.Since(start) / time.Duration(n)
	}
	before := measure(100)
	exec := func(q string) {
		t.Helper()
		if _, err := s.Store.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	exec(`DROP INDEX idx_memory_obs_person_name;
        WITH RECURSIVE ids(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM ids WHERE i<200000)
        INSERT INTO memory_observations(observation_id,platform,collection,source_room,room,native_message_id,actor_label,actor_account,content,parsed_time,record_kind,imported_at,deleted_at)
        SELECT printf('discord:1425814867168345118:1425814867168345119:%018d',i),'discord','discord','discord:g:c','discord:g:c',
          printf('%018d',i),'민수','1425814867168345120','','2026-10-01T00:00:00Z','human_text','2026-10-01T00:00:00Z','2026-10-02T00:00:00Z' FROM ids;
        CREATE TRIGGER r3_no_rewrite_insert BEFORE INSERT ON memory_observations BEGIN SELECT RAISE(ABORT,'observation insert'); END;
        CREATE TRIGGER r3_no_rewrite_update BEFORE UPDATE ON memory_observations BEGIN SELECT RAISE(ABORT,'observation update'); END;
        CREATE TRIGGER r3_no_rewrite_delete BEFORE DELETE ON memory_observations BEGIN SELECT RAISE(ABORT,'observation delete'); END;`)
	pages := func(q string) int64 {
		t.Helper()
		var result int64
		if err := s.Store.Reader().QueryRow(q).Scan(&result); err != nil {
			t.Fatal(err)
		}
		return result
	}
	pageSize := pages(`PRAGMA page_size`)
	allocated := func() int64 {
		return (pages(`PRAGMA page_count`) - pages(`PRAGMA freelist_count`)) * pageSize
	}
	was := allocated()
	start := time.Now()
	if err := store.FillPersonAliases(t.Context(), s.Store); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)
	grew := allocated() - was
	if got := pages(`SELECT count(*) FROM memory_observations`); got != 200001 {
		t.Fatal(got)
	}
	if got := pages(`SELECT cursor FROM person_alias_fill`); got != -1 {
		t.Fatal(got)
	}
	after := measure(20)
	t.Logf("index build %s, +%d B; resolve %s before and %s after 200k deleted rows", elapsed, grew, before, after)
	if grew > 1<<20 {
		t.Errorf("the name index holds deleted rows: +%d B", grew)
	}
	if after > max(5*time.Millisecond, 4*before) {
		t.Errorf("deleted history slows the name check: %s per resolve (was %s)", after, before)
	}
}
