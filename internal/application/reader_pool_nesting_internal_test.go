package application

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/hostprofile"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// smallHostSvc is a service on the small host profile: two reader connections. A pass that keeps a reader
// cursor open while it asks the same pool again holds one connection and waits for another; two such
// passes at once wait for good.
func smallHostSvc(t *testing.T) (*Service, *storage.Store) {
	t.Helper()
	prev := hostprofile.Current()
	hostprofile.Apply(hostprofile.DetectFrom(1, 1<<30, "small"))
	t.Cleanup(func() { hostprofile.Apply(prev) })
	s := jobTestSvc(t)
	if hostprofile.Current().ReaderConns > 2 {
		t.Fatalf("precondition: the small profile has %d reader connections", hostprofile.Current().ReaderConns)
	}
	if _, err := store.Open(s.Store); err != nil {
		t.Fatal(err)
	}
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	return s, db
}

// bothFinish runs pass twice at once and fails when either is still waiting after 10s. A failure closes
// the reader pool first, which ends the stuck waits (the service's cleanup would otherwise wait for them).
func bothFinish(t *testing.T, db *storage.Store, what string, pass func() int) []int {
	t.Helper()
	done := make(chan int, 2)
	for range 2 {
		go func() { done <- pass() }()
	}
	timeout := time.After(10 * time.Second)
	var got []int
	for range 2 {
		select {
		case n := <-done:
			got = append(got, n)
		case <-timeout:
			_ = db.Reader().Close() // unblocks the stuck passes; the test fails anyway
			t.Fatalf("two %s passes at once still wait after 10s: a cursor holds the reader pool", what)
		}
	}
	return got
}

// The episode pass lists the capturing rooms; a room with no entry of its own asks the reader pool for its
// stored thread parent (roomCaptures). Two passes at once on a small host must both finish.
func TestCapturingRoomsTwoPassesFinishOnASmallHost(t *testing.T) {
	s, db := smallHostSvc(t)
	for i := range 200 {
		if _, err := s.Store.DB.Exec(`INSERT INTO memory_rooms(room, collection, platform, updated_at) VALUES(?,?,?,?)`,
			fmt.Sprintf("discord:g1:c%03d", i), "c", "discord", "2026-11-22T00:00:00Z"); err != nil {
			t.Fatal(err)
		}
	}
	for _, n := range bothFinish(t, db, "capturingRooms", func() int { return len(s.capturingRooms(db)) }) {
		if n != 0 {
			t.Fatalf("no channel stores messages here, got %d capturing rooms", n)
		}
	}
}

// The console's episode list names the people of each room (refLabels reads the reader pool) while the
// list itself is read from the same pool. Two lists at once on a small host must both finish, and the
// names still follow memory consent.
func TestEpisodeListTwoAtOnceFinishOnASmallHost(t *testing.T) {
	s, db := smallHostSvc(t)
	at := "2026-11-22T00:00:00Z"
	for _, room := range []string{"c1", "c2"} {
		key := "discord:g:" + room
		for _, p := range []struct{ account, label string }{{"501", "Minsu"}, {"502", "Withdrawn Name"}} {
			if _, err := s.Store.DB.Exec(`INSERT INTO memory_observations(observation_id,platform,collection,source_room,room,native_message_id,actor_label,actor_account,content,parsed_time,record_kind,imported_at)
				VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, key+":"+p.account, "discord", "c", key, key, room+"-"+p.account, p.label, p.account, "synthetic", at, "human_text", at); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := s.Store.DB.Exec(`INSERT INTO channel_episode(id,guild_id,channel_id,room,kind,title,status,source,first_seen,last_seen,updated_at)
			VALUES(?,'g',?,?,'event',?,'open','discord',?,?,?)`, "ep-"+room, room, key, "{@"+store.PersonRef("501")+"} and {@"+store.PersonRef("502")+"} plan", at, at, at); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.setConsent("502", "memory", "withdrawn", "console"); err != nil {
		t.Fatal(err)
	}
	var titles []string
	var mu sync.Mutex
	bothFinish(t, db, "episode list", func() int {
		r := s.Execute(context.Background(), action.Request{Action: "memory.episodes.list", Input: []byte(`{}`)})
		if !r.OK {
			return -1
		}
		list, _ := r.Output["episodes"].([]map[string]any)
		mu.Lock()
		defer mu.Unlock()
		for _, e := range list {
			titles = append(titles, fmt.Sprint(e["title"]))
		}
		return len(list)
	})
	if len(titles) != 4 {
		t.Fatalf("want two episodes from each list, got %q", titles)
	}
	for _, title := range titles {
		if !strings.Contains(title, "Minsu") || strings.Contains(title, "Withdrawn Name") {
			t.Fatalf("labels must follow memory consent: %q", title)
		}
	}
}
