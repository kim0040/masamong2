package application

import (
	"context"
	"testing"
	"time"
)

// Implicit rows go with the turn records' retention, like the ratings they sit beside.
func TestRetentionDropsOldImplicitFeedback(t *testing.T) {
	s, clk := deliveryService(t)
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	for i, age := range []time.Duration{3650 * 24 * time.Hour, time.Hour} {
		if _, err := db.DB.Exec(`INSERT INTO turn_feedback(turn_id,user_hash,source,rating,reason,at) VALUES(?,?,?,0,'',?)`,
			"turn_"+string(rune('a'+i)), "hash", implicitSource+"recall", clk.t.Add(-age).UTC().Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.runRetention(context.Background()); err != nil {
		t.Fatal(err)
	}
	var kept []string
	rows, err := db.DB.Query(`SELECT turn_id FROM turn_feedback`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		_ = rows.Scan(&id)
		kept = append(kept, id)
	}
	if len(kept) != 1 || kept[0] != "turn_b" {
		t.Fatalf("kept %v, want only the recent row", kept)
	}
}
