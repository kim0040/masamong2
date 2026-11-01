package store

import (
	"database/sql"
	"slices"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
)

// Deleting a room takes its episodes, their items, leaf links, evidence index and the room state along.
func TestDeleteRoomRemovesRoomMemory(t *testing.T) {
	st := openStore(t)
	base := time.Now().Add(-time.Hour)
	if _, err := st.InsertObservations([]corpus.Observation{mk(0, base), mk(1, base.Add(time.Minute))}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SyncLeaves("discord:1:2", leaves.DefaultRecipe(), time.Now(), time.UTC); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	err := st.DB.WithWriter(func(tx *sql.Tx) error {
		leafs, err := LeavesOf(tx, []string{"oa"})
		if err != nil || len(leafs) != 1 {
			t.Fatalf("leaves of oa: %v %v", leafs, err)
		}
		for _, q := range []string{
			`INSERT INTO channel_episode(id,guild_id,channel_id,room,kind,title,status,source,first_seen,last_seen,updated_at) VALUES('ep1','1','2','discord:1:2','plan','t','open','discord','` + now + `','` + now + `','` + now + `')`,
			`INSERT INTO episode_item(id,episode_id,kind,text,evidence,created_at) VALUES('it1','ep1','fact','x','["oa"]','` + now + `')`,
			`INSERT INTO episode_leaf(episode_id,leaf_id) VALUES('ep1','` + leafs[0] + `')`,
			`INSERT INTO evidence_refs(source_id,holder_kind,holder_id) VALUES('oa','episode_item','it1')`,
			`INSERT INTO channel_state(guild_id,channel_id,room,body,evidence,updated_at) VALUES('1','2','discord:1:2','{}','["ob"]','` + now + `')`,
			`INSERT INTO evidence_refs(source_id,holder_kind,holder_id) VALUES('ob','channel_state','state:1:2')`,
		} {
			if _, err := tx.Exec(q); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.DeleteRoom("discord:1:2"); err != nil {
		t.Fatal(err)
	}
	var left []string
	for _, table := range []string{"channel_episode", "episode_item", "episode_leaf", "channel_state", "evidence_refs"} {
		var n int
		if err := st.DB.DB.QueryRow(`SELECT COUNT(*) FROM ` + table).Scan(&n); err != nil {
			t.Fatal(err)
		}
		if n > 0 {
			left = append(left, table)
		}
	}
	if !slices.Equal(left, nil) {
		t.Fatalf("left after deleting the room: %v", left)
	}
}

// LeavesOf reads only the observations' rooms and the leaves that can still hold them, whatever form
// the stored times take, and finds the same leaves as reading every one.
func TestLeavesOfReadsOnlyTheLeavesThatCanHoldThem(t *testing.T) {
	st := openStore(t)
	for _, q := range []string{
		`INSERT INTO memory_observations(observation_id,platform,collection,source_room,room,native_message_id,actor_label,actor_account,content,parsed_time,record_kind,imported_at)
		 VALUES('o1','discord','c','r','room-a','n1','a','1','x','2026-10-20T10:00:00.123456+09:00','human_text','t'),
		       ('o2','kakao','c','r','room-b','n2','a','1','y','2026-10-20T10:00:00','human_text','t')`,
		`INSERT INTO memory_leaves(leaf_id,room,recipe,first_observation_id,observation_ids,n,body,body_hash,speakers,end_time,updated_at) VALUES
		 ('a-now','room-a','r','o1','["o0","o1"]',2,'b','h1','[]','2026-10-20T10:05:00+09:00','t'),
		 ('a-zone','room-a','r','o1','["o1"]',1,'b','h2','[]','2026-10-20T01:00:00','t'),
		 ('a-old','room-a','r','o0','["o0"]',1,'b','h3','[]','2026-09-01T00:00:00','t'),
		 ('a-open','room-a','r','o1','["o1","o9"]',2,'b','h4','[]','','t'),
		 ('b-now','room-b','r','o2','["o2"]',1,'b','h5','[]','2026-10-20T10:00:00','t'),
		 ('c-other','room-c','r','o1','["o1"]',1,'b','h6','[]','2026-10-21T00:00:00','t')`,
	} {
		if _, err := st.DB.DB.Exec(q); err != nil {
			t.Fatal(err)
		}
	}
	err := st.DB.WithWriter(func(tx *sql.Tx) error {
		got, err := LeavesOf(tx, []string{"o1", "o2", "gone"})
		if err != nil {
			return err
		}
		slices.Sort(got)
		// a-zone ended "before" o1 only by its zone; room-c is not o1's room
		if want := []string{"a-now", "a-open", "a-zone", "b-now"}; !slices.Equal(got, want) {
			t.Fatalf("leaves %v, want %v", got, want)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
