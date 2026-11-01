package application_test

import (
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/memory/engine"
)

func TestUsersListGathersOnePersonsData(t *testing.T) {
	var calls atomic.Int32
	svc := fortuneSvc(t, &calls)
	dm := person("100001", true)
	_ = as(svc, "privacy.consent", map[string]any{"scope": "fortune", "decision": "grant", "confirmed": true}, dm)
	if r := as(svc, "fortune.subscribe", map[string]any{"op": "register", "text": "1990-01-01 14:30 남 서울"}, dm); !r.OK {
		t.Fatalf("register: %+v", r)
	}
	if r := exec(svc, "preferences.set", map[string]any{"user_id": "200002", "timezone": "Asia/Seoul"}); !r.OK {
		t.Fatalf("prefs: %+v", r)
	}
	if r := exec(svc, "access.policy", map[string]any{"blocked_users": []any{"300003"}}); !r.OK {
		t.Fatalf("block: %+v", r)
	}
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "9001", AuthorID: "200002", AuthorName: "영희", Content: "안녕", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	r := exec(svc, "users.list", nil)
	if !r.OK {
		t.Fatalf("users.list: %+v", r)
	}
	raw, _ := json.Marshal(r.Output)
	var out struct {
		Users []struct {
			UserID   string                    `json:"user_id"`
			Name     string                    `json:"name"`
			Consents map[string]map[string]any `json:"consents"`
			Prefs    map[string]any            `json:"prefs"`
			Fortune  map[string]any            `json:"fortune"`
			Blocked  bool                      `json:"blocked"`
		} `json:"users"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	byID := map[string]int{}
	for i, u := range out.Users {
		byID[u.UserID] = i
	}
	if len(out.Users) != 3 {
		t.Fatalf("three people: %s", raw)
	}
	a := out.Users[byID["100001"]]
	if a.Consents["fortune"]["allows"] != true || a.Fortune == nil || a.Name != "철수" {
		t.Fatalf("fortune subscriber: %+v", a)
	}
	b := out.Users[byID["200002"]]
	if b.Prefs["timezone"] != "Asia/Seoul" || b.Name != "영희" {
		t.Fatalf("settings and a name from memory: %+v", b)
	}
	if !out.Users[byID["300003"]].Blocked {
		t.Fatal("blocked user listed")
	}
	// the console sees that someone subscribed, not what they told the bot
	if strings.Contains(string(raw), "1990") || strings.Contains(string(raw), "서울") {
		t.Fatalf("birth data leaked: %s", raw)
	}
}
