package application

import (
	"sync"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
)

// A thread closed more than a month ago is no longer in the live thread table, but its stored parent still
// decides: under a parent with capture off the episode pass leaves it alone, under one that stores it is a
// capturing room. A thread's own entry never turns capture back on under a parent that turned it off
// (turning memory off is explicit and wins), and its own off still counts under a parent that stores.
// Two passes at once on a small host must both finish (the parent is read from the reader pool).
func TestAgedThreadFollowsItsParentsCaptureOff(t *testing.T) {
	s, db := smallHostSvc(t)
	if r := s.mutateConfig(action.Request{}, func(d *config.Document) error {
		d.Memory.Capture = true
		d.Guilds = map[string]config.GuildSettings{"g1": {Channels: map[string]config.ChannelPolicy{
			"off":    {Respond: "command", Capture: false},
			"on":     {Respond: "command", Capture: true},
			"kid":    {Respond: "command", Capture: true},  // its own on, under "off"
			"kidoff": {Respond: "command", Capture: false}, // its own off, under "on"
		}}}
		return nil
	}); !r.OK {
		t.Fatal(r.Error)
	}
	closed := s.Clock.Now().Add(-31 * 24 * time.Hour).UTC().Format(time.RFC3339)
	threads := map[string]string{"old": "off", "oldon": "on", "oldplain": "plain", "kid": "off", "kidoff": "on"}
	for id, parent := range threads {
		if _, err := s.Store.DB.Exec(`INSERT INTO channel_threads(thread_id,guild_id,parent_id,state,created_at,last_at,closed_at)
			VALUES(?,?,?,?,?,?,?)`, id, "g1", parent, "closed", closed, closed, closed); err != nil {
			t.Fatal(err)
		}
	}
	for _, room := range []string{"old", "oldon", "oldplain", "kid", "kidoff", "off", "on", "plain"} {
		if _, err := s.Store.DB.Exec(`INSERT INTO memory_rooms(room, collection, platform, updated_at) VALUES(?,?,?,?)`,
			"discord:g1:"+room, "c", "discord", closed); err != nil {
			t.Fatal(err)
		}
	}
	if p := s.threadParent("old"); p != "" {
		t.Fatalf("precondition: a thread closed a month ago is out of the live table, got parent %q", p)
	}
	if p := storedThreadParent(db, "g1", "old"); p != "off" {
		t.Fatalf("stored parent of the old thread = %q, want off", p)
	}
	want := map[string]bool{"oldon": true, "oldplain": true, "on": true, "plain": true}
	var mu sync.Mutex
	var passes [][]memRoom
	bothFinish(t, db, "capturingRooms", func() int {
		rooms := s.capturingRooms(db)
		mu.Lock()
		defer mu.Unlock()
		passes = append(passes, rooms)
		return len(rooms)
	})
	for _, rooms := range passes {
		got := map[string]bool{}
		for _, r := range rooms {
			got[r.channel] = true
		}
		for room := range threads {
			if got[room] != want[room] {
				t.Errorf("room %s capturing = %v, want %v", room, got[room], want[room])
			}
		}
		if !got["on"] || !got["plain"] || got["off"] {
			t.Errorf("channels: %v", got)
		}
	}
}
