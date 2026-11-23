package application

import (
	"testing"
	"time"
)

// A thread recorded before thread_parents existed has only its channel_threads row. Starting the service
// copies its parent into thread_parents (keeping a parent already stored), so after retention removes the
// row the stored parent still answers, and nothing else is written.
func TestThreadParentsSeededFromThreadRows(t *testing.T) {
	t.Parallel()
	s, _ := deliveryService(t)
	at := s.Clock.Now().Add(-40 * 24 * time.Hour).UTC().Format(time.RFC3339)
	for _, row := range [][3]string{{"old", "g1", "off"}, {"known", "g1", "first"}, {"orphan", "g1", ""}} {
		if _, err := s.Store.DB.Exec(`INSERT INTO channel_threads(thread_id,guild_id,parent_id,state,created_at,last_at,closed_at)
			VALUES(?,?,?,'closed',?,?,?)`, row[0], row[1], row[2], at, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Store.DB.Exec(`DELETE FROM thread_parents`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec(`INSERT INTO thread_parents(thread_id,guild_id,parent_id) VALUES('known','g1','stored')`); err != nil {
		t.Fatal(err)
	}
	if err := migrateFeatures(s.Store); err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	rows, err := s.Store.DB.Query(`SELECT thread_id, guild_id || '/' || parent_id FROM thread_parents`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var id, parent string
		if err := rows.Scan(&id, &parent); err != nil {
			t.Fatal(err)
		}
		got[id] = parent
	}
	if err := rows.Close(); err != nil {
		t.Fatal(err)
	}
	if want := map[string]string{"old": "g1/off", "known": "g1/stored"}; len(got) != len(want) || got["old"] != want["old"] || got["known"] != want["known"] {
		t.Fatalf("thread_parents = %v, want %v", got, want)
	}
	// retention removes the closed thread's row; the seeded parent still answers
	if _, err := s.Store.DB.Exec(`DELETE FROM channel_threads WHERE thread_id='old'`); err != nil {
		t.Fatal(err)
	}
	if p := storedThreadParent(s.Store, "g1", "old"); p != "off" {
		t.Fatalf("stored parent after retention = %q, want off", p)
	}
	// a second start changes nothing
	if err := migrateFeatures(s.Store); err != nil {
		t.Fatal(err)
	}
	var n int
	if err := s.Store.DB.QueryRow(`SELECT count(*) FROM thread_parents`).Scan(&n); err != nil || n != 2 {
		t.Fatalf("rows after a second start: %d %v", n, err)
	}
}
