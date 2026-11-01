package store

import (
	"encoding/json"
	"fmt"
	"testing"
)

// Leaves that held a changed message lose their text at once, whether one message changed (a substring
// scan of the ID lists) or thousands did (one JSON pass per chunk); other leaves are left alone.
func TestInvalidateLeavesFindsExactlyTheLeavesHoldingTheMessages(t *testing.T) {
	st := openStore(t)
	const leaves = 1200
	setup, err := st.DB.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < leaves; i++ {
		ids, _ := json.Marshal([]string{fmt.Sprintf("o%d", i*2), fmt.Sprintf("o%d", i*2+1)})
		if _, err := setup.Exec(`INSERT INTO memory_leaves(leaf_id, room, recipe, first_observation_id, observation_ids, n, body, body_hash, speakers, updated_at)
			VALUES(?,?,?,?,?,2,'text','h','["a"]','now')`, fmt.Sprintf("l%d", i), "r", "v", fmt.Sprintf("o%d", i*2), string(ids)); err != nil {
			t.Fatal(err)
		}
	}
	if err := setup.Commit(); err != nil {
		t.Fatal(err)
	}
	cleared := func() map[string]bool {
		rows, err := st.DB.DB.Query(`SELECT leaf_id FROM memory_leaves WHERE body=''`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		out := map[string]bool{}
		for rows.Next() {
			var id string
			_ = rows.Scan(&id)
			out[id] = true
		}
		return out
	}
	run := func(ids ...string) {
		t.Helper()
		tx, err := st.DB.DB.Begin()
		if err != nil {
			t.Fatal(err)
		}
		if err := invalidateLeaves(tx, ids...); err != nil {
			t.Fatal(err)
		}
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
	run("o3", "o10") // few: leaves l1 and l5; "o1" would not match "o10"'s leaf by prefix
	if got := cleared(); len(got) != 2 || !got["l1"] || !got["l5"] {
		t.Fatalf("few: %v", got)
	}
	var many []string
	for i := 0; i < 1000; i += 2 { // 500 ids -> leaves l0..l249 (two ids each leaf, one named)
		many = append(many, fmt.Sprintf("o%d", i))
	}
	many = append(many, many...) // duplicates are harmless
	run(many...)
	if got := cleared(); len(got) != 500 && len(got) != 501 && len(got) != 502 {
		t.Fatalf("many: %d leaves cleared", len(got))
	}
	for i := 600; i < leaves; i++ {
		var body string
		_ = st.DB.DB.QueryRow(`SELECT body FROM memory_leaves WHERE leaf_id=?`, fmt.Sprintf("l%d", i)).Scan(&body)
		if body == "" {
			t.Fatalf("leaf l%d holds none of the messages but lost its text", i)
		}
	}
}
