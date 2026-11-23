package peoplebackfill

import (
	"sort"
	"testing"
)

// roomWindows builds w windows of n alternating-speaker lines each for room, one segment apart.
func roomWindows(room string, w, n int) []Window {
	var all []Window
	for i := 0; i < w; i++ {
		ws := BuildWindows(room, talk(room, n, i*24*60), Options{MaxLines: n + 1})
		if len(ws) != 1 {
			panic("fixture")
		}
		all = append(all, ws[0])
	}
	return all
}

func TestSelectPilotIsDeterministicAndSpread(t *testing.T) {
	var windows []Window
	// three rooms of very different sizes
	windows = append(windows, roomWindows("room-a", 200, 40)...)
	windows = append(windows, roomWindows("room-b", 100, 40)...)
	windows = append(windows, roomWindows("room-c", 10, 40)...)
	shuffled := append([]Window(nil), windows...)
	sort.SliceStable(shuffled, func(i, j int) bool { return shuffled[i].ID < shuffled[j].ID })
	a := SelectPilot(shuffled, PilotSize)
	b := SelectPilot(windows, PilotSize)
	if len(a) != PilotSize || len(b) != PilotSize {
		t.Fatalf("pilot size: %d %d", len(a), len(b))
	}
	for i := range a {
		if a[i].ID != b[i].ID {
			t.Fatalf("input order changes nothing: pick %d", i)
		}
	}
	rooms := map[string]int{}
	for _, w := range a {
		rooms[w.Room]++
	}
	for _, r := range []string{"room-a", "room-b", "room-c"} {
		if rooms[r] == 0 {
			t.Fatalf("every room is in the pilot: %v", rooms)
		}
	}
	// room-c has 10 of 310 windows: proportional shares give it a few, never none, and the
	// picks spread across its timeline rather than clustering at the start
	if rooms["room-c"] > 10 {
		t.Fatalf("a room gives no more than it has: %v", rooms)
	}
	picks := map[string]bool{}
	for _, w := range a {
		picks[w.ID] = true
	}
	var cPicks []int
	for i, w := range roomWindows("room-c", 10, 40) {
		if picks[w.ID] {
			cPicks = append(cPicks, i)
		}
	}
	for i := 1; i < len(cPicks); i++ {
		if cPicks[i] == cPicks[i-1] || cPicks[i]-cPicks[i-1] < 1 {
			t.Fatalf("picks spread over the room's timeline: %v", cPicks)
		}
	}
	if len(cPicks) > 1 && cPicks[0] == 0 && cPicks[1] == 1 && len(cPicks) > 2 {
		t.Fatalf("not a prefix of the room: %v", cPicks)
	}
}

func TestSelectPilotEdges(t *testing.T) {
	// fewer windows than asked: everything comes back
	ws := roomWindows("r", 5, 40)
	if got := SelectPilot(ws, 60); len(got) != 5 {
		t.Fatalf("all of a small set: %d", len(got))
	}
	// skipped windows are never picked
	ws[0].Skipped = true
	got := SelectPilot(ws, 5)
	if len(got) != 4 {
		t.Fatalf("a skipped window is not sent: %d", len(got))
	}
	for _, w := range got {
		if w.ID == ws[0].ID {
			t.Fatal("the skipped window was picked")
		}
	}
	// fewer picks than rooms: a deterministic spread of rooms
	var many []Window
	for i := 0; i < 8; i++ {
		many = append(many, roomWindows("room-"+itoa(i), 20, 40)...)
	}
	got = SelectPilot(many, 3)
	if len(got) != 3 {
		t.Fatalf("three picks: %d", len(got))
	}
	rooms := map[string]bool{}
	for _, w := range got {
		if rooms[w.Room] {
			t.Fatalf("a room gives one window each when picks < rooms: %v", got)
		}
		rooms[w.Room] = true
	}
	again := SelectPilot(many, 3)
	for i := range again {
		if again[i].ID != got[i].ID {
			t.Fatal("deterministic")
		}
	}
}
