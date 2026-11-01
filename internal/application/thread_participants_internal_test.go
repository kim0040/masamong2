package application

import (
	"fmt"
	"strings"
	"testing"
)

func TestSingleThreadParticipantReadIsBounded(t *testing.T) {
	s, _ := deliveryService(t)
	s.NoteThread("g", "target", "parent", false)
	s.NoteThreadParticipant("target", "member")
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	for i := 0; i < 512; i++ {
		id := fmt.Sprintf("other-%d", i)
		if _, err := tx.Exec(`INSERT INTO channel_threads(thread_id,guild_id,parent_id,state,created_at,last_at) VALUES(?,'g','parent','open','','')`, id); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO thread_participants(thread_id,user_id,added_at) VALUES(?,'other-member','')`, id); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	info := &threadInfo{humans: map[string]bool{}, participants: map[string]bool{}}
	if visited := s.loadSingleThreadParticipants(db, "target", info); visited != 1 || !info.participants["member"] {
		t.Fatalf("single-thread recovery visited %d rows, want only its one participant", visited)
	}
	rows, err := db.Reader().Query(`EXPLAIN QUERY PLAN `+singleThreadParticipantsSQL, "target")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var indexed bool
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(detail, "SCAN p") {
			t.Fatal("single-thread recovery scans unrelated participant rows")
		}
		indexed = indexed || strings.Contains(detail, "SEARCH p") && strings.Contains(detail, "thread_id=?")
	}
	if err := rows.Err(); err != nil || !indexed {
		t.Fatal("participant lookup did not use the thread_id PK range")
	}
}
