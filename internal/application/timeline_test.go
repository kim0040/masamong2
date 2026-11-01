package application_test

import (
	"strconv"
	"testing"
	"time"

	"masamong/internal/memory/engine"
)

func TestTimelineListsClosedPiecesByDay(t *testing.T) {
	svc := setupSvc(t)
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	day1 := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 16; i++ {
		at := day1.Add(time.Duration(i) * time.Minute)
		text := "캠핑 계획 " + strconv.Itoa(i)
		if i >= 8 {
			at = day2.Add(time.Duration(i) * time.Minute)
			text = "마라탕 약속 " + strconv.Itoa(i)
		}
		_, _ = eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: strconv.Itoa(500 + i), AuthorID: "u", AuthorName: "철수", Content: text, At: at})
	}
	if err := eng.Resync([]string{engine.RoomKey("g", "c")}); err != nil {
		t.Fatal(err)
	}
	r := exec(svc, "events.timeline", map[string]any{"guild_id": "g", "channel_id": "c"})
	if !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	days := asMap(t, r.Output)["days"].([]any)
	if len(days) != 2 || days[0].(map[string]any)["date"] != "2026-09-03" {
		t.Fatalf("days newest first: %v", days)
	}
	r = exec(svc, "events.timeline", map[string]any{"guild_id": "g", "channel_id": "c", "from": "2026-09-01", "to": "2026-09-01"})
	if days := asMap(t, r.Output)["days"].([]any); len(days) != 1 {
		t.Fatalf("date filter: %v", days)
	}
	// a question without an embedding model falls back to exact message search
	r = exec(svc, "events.timeline", map[string]any{"guild_id": "g", "channel_id": "c", "query": "마라탕"})
	out := asMap(t, r.Output)
	if !r.OK || out["mode"] != "search" || out["count"].(float64) == 0 {
		t.Fatalf("search: %+v", r)
	}
	if r := exec(svc, "events.timeline", nil); r.OK {
		t.Fatal("a scope is required")
	}
}
