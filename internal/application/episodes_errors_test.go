package application_test

import (
	"context"
	"testing"
	"time"
)

// An episode whose stored segment cannot be read is kept as it is. Reading it as "no messages" looked
// like a conversation with no people and deleted the episode (error-drop rule, fix-plan R04).
func TestRegenerationKeepsAnEpisodeWithAnUnreadableSegment(t *testing.T) {
	t.Parallel()
	svc, m, eng := roomSvc(t)
	m.extract = func(string) string { return hikeAnswer }
	sayIn(eng, time.Now().Add(-2*time.Hour), hike...)
	svc.RunRoomMemory(context.Background())
	id := episodeList(t, svc)[0]["id"].(string)

	if err := svc.MessagesDeleted("g", "c", []string{"3003"}); err != nil {
		t.Fatal(err)
	}
	drain(t, svc)
	if l := episodeList(t, svc); l[0]["regen"] != true {
		t.Fatalf("a hidden decision queues a regeneration: %+v", l)
	}
	if _, err := svc.Store.DB.Exec(`UPDATE channel_episode SET segment='{not json' WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	calls := m.calls("episode")
	svc.RunRoomMemory(context.Background())
	l := episodeList(t, svc)
	if len(l) != 1 || l[0]["id"] != id {
		t.Fatalf("the episode survives a segment that cannot be read: %+v", l)
	}
	if l[0]["regen"] != false {
		t.Fatalf("the regeneration is not retried every pass: %+v", l)
	}
	if m.calls("episode") != calls {
		t.Fatal("no model call is spent on a segment that cannot be read")
	}
}

// A room cursor that is not valid JSON must not stall the pass: the room starts over.
func TestRoomCursorThatIsNotJSONStartsTheRoomOver(t *testing.T) {
	t.Parallel()
	svc, m, eng := roomSvc(t)
	m.extract = func(string) string { return hikeAnswer }
	sayIn(eng, time.Now().Add(-2*time.Hour), hike...)
	var room string
	if err := svc.Store.DB.QueryRow(`SELECT room FROM memory_rooms LIMIT 1`).Scan(&room); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Store.DB.Exec(`INSERT INTO admin_state(k, v, updated_at) VALUES(?, '{not json', '')`, "episodes:cursor:"+room); err != nil {
		t.Fatal(err)
	}
	if rep := svc.RunRoomMemory(context.Background()); rep.StoppedBy != "" || rep.Episodes != 1 {
		t.Fatalf("the pass goes on past a bad cursor: %+v", rep)
	}
}
