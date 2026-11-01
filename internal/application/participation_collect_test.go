package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/config"
)

// noModelSvc is a service whose model host fails the test when it is called at all.
func noModelSvc(t *testing.T, instanceMode, channelMode string) (*application.Service, *atomic.Int32) {
	t.Helper()
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		calls.Add(1)
		t.Errorf("collect mode called the model: %s", r.URL.Path)
		http.Error(w, "no", 500)
	}))
	t.Cleanup(srv.Close)
	svc := routedSvc(t, srv.URL, srv.URL)
	applyDoc(t, svc, func(d *config.Document) {
		d.Discord.Enabled = true
		d.Participation = config.Participation{Enabled: true, Mode: instanceMode}
		d.Memory.Capture = true
		d.Guilds = map[string]config.GuildSettings{"g": {Locale: "ko", Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention", Participation: channelMode, Capture: true}}}}
	})
	application.SetParticipationWait(svc, 30*time.Millisecond)
	return svc, &calls
}

func TestParticipationCollectRecordsWithoutAModelCall(t *testing.T) {
	svc, calls := noModelSvc(t, "shadow", "collect")
	if r := exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"}); r.Output["effective"] != "collect" {
		t.Fatalf("effective mode: %+v", r.Output)
	}
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	log := partLog(t, svc, 1)
	if log[0]["state"] != "collected" || log[0]["mode"] != "collect" || log[0]["reason"] != "unanswered" || log[0]["kind"] == "" {
		t.Fatalf("collected row: %+v", log[0])
	}
	// a moment people handled meanwhile is kept with what they did
	application.SetParticipationWait(svc, 300*time.Millisecond)
	say(svc, "m2", "u1", "누가 보드게임 가져오기로 했지?")
	say(svc, "m3", "u2", "내가 가져갈게")
	log = partLog(t, svc, 2)
	if log[0]["state"] != "cancelled" || log[0]["reason"] != "people_answered" || log[0]["mode"] != "collect" {
		t.Fatalf("handled row: %+v", log[0])
	}
	time.Sleep(400 * time.Millisecond)
	if n := len(partLog(t, svc, 2)); n != 2 {
		t.Fatalf("a handled moment must not also be collected: %d rows", n)
	}
	// the operator can still label a collected row
	if r := exec(svc, "participation.mode", map[string]any{"review": log[1]["id"], "verdict": "good"}); !r.OK {
		t.Fatal(r.Error)
	}
	if calls.Load() != 0 {
		t.Fatalf("model calls: %d", calls.Load())
	}
}

// collect sits below shadow: a stricter side lowers the other, whichever it is.
func TestParticipationCollectSitsBelowShadow(t *testing.T) {
	svc, _ := noModelSvc(t, "collect", "conservative")
	if r := exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"}); r.Output["effective"] != "collect" {
		t.Fatalf("instance collect caps the channel: %+v", r.Output)
	}
	svc, _ = noModelSvc(t, "conservative", "collect")
	if r := exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"}); r.Output["effective"] != "collect" {
		t.Fatalf("channel collect: %+v", r.Output)
	}
}

// The bot's name said without calling it is a candidate moment in collect mode, and only there.
func TestParticipationCollectNotesBotMentioned(t *testing.T) {
	for _, mode := range []string{"collect", "shadow"} {
		svc, _, js := partSvc(t, mode)
		js.answer = `{"to":"about_bot","tone":"neutral"}`
		applyDoc(t, svc, func(d *config.Document) {
			p := d.Guilds["g"].Channels["c"]
			p.NameCall = "addressed"
			d.Guilds["g"].Channels["c"] = p
		})
		_, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m9", Message: "마사몽 요즘 똑똑하더라",
			Speaker: "철수", SpeakerID: "u1", NameOnly: true})
		if err == nil || err.Error() != application.ErrNotAddressed.Error() {
			t.Fatalf("%s: expected ErrNotAddressed, got %v", mode, err)
		}
		if mode == "collect" {
			log := partLog(t, svc, 1)
			if log[0]["state"] != "collected" || log[0]["kind"] != "bot_mentioned" || log[0]["mode"] != "collect" {
				t.Fatalf("mention row: %+v", log[0])
			}
		} else {
			time.Sleep(100 * time.Millisecond)
			r := exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"})
			if raw, _ := json.Marshal(r.Output["log"]); string(raw) != "[]" {
				t.Fatalf("shadow must not collect mentions: %s", raw)
			}
		}
		if js.judged.Load() != 0 {
			t.Fatalf("%s: the judge was called for a mention", mode)
		}
	}
}

// Shadow is unchanged by collect: the same question is judged and logged as before.
func TestParticipationShadowStillJudgesAfterCollect(t *testing.T) {
	svc, _, js := partSvc(t, "shadow")
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지?")
	log := partLog(t, svc, 1)
	if log[0]["state"] != "shadow" || js.judged.Load() != 1 {
		t.Fatalf("shadow: %+v judged=%d", log[0], js.judged.Load())
	}
}

func TestConversationLogActionCountsDecisions(t *testing.T) {
	svc, _, js := partSvc(t, "off")
	js.answer = `{"to":"about_bot","tone":"neutral"}`
	applyDoc(t, svc, func(d *config.Document) {
		p := d.Guilds["g"].Channels["c"]
		p.NameCall = "addressed"
		d.Guilds["g"].Channels["c"] = p
	})
	for i := 0; i < 2; i++ {
		_, _ = svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m", Message: "마사몽 얘기", Speaker: "철수", SpeakerID: "u1", NameOnly: true})
	}
	now := time.Now().UTC()
	ins := func(id, guild, ch, kind, decision, outcome string, age time.Duration) {
		t.Helper()
		if _, err := svc.Store.DB.Exec(`INSERT INTO conversation_log(id,guild_id,channel_id,created_at,kind,decision,mode,outcome) VALUES(?,?,?,?,?,?,?,?)`,
			id, guild, ch, now.Add(-age).Format(time.RFC3339Nano), kind, decision, "off", outcome); err != nil {
			t.Fatal(err)
		}
	}
	ins("a", "g", "c", "follow", "would_answer", "retagged", time.Hour)
	ins("b", "g", "c", "follow", "would_answer", "", time.Hour)
	ins("c", "g", "c", "follow", "would_skip", "", time.Hour)
	ins("d", "g", "c", "thread", "offer", "", time.Hour)
	ins("e", "g", "c", "thread", "declined", "", time.Hour)
	ins("old", "g", "c", "follow", "would_answer", "", 10*24*time.Hour) // outside the 7 days
	ins("other", "g", "other", "follow", "would_answer", "", time.Hour) // another channel
	r := exec(svc, "conversation.log", map[string]any{"guild_id": "g", "channel_id": "c", "limit": 3})
	if !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	raw, _ := json.Marshal(r.Output)
	var out struct {
		Days     int                       `json:"days"`
		Counts   map[string]map[string]int `json:"counts"`
		Outcomes map[string]map[string]int `json:"outcomes"`
		Rows     []map[string]any          `json:"rows"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatal(err)
	}
	if out.Days != 7 || out.Counts["follow"]["would_answer"] != 2 || out.Counts["follow"]["would_skip"] != 1 || out.Counts["thread"]["offer"] != 1 ||
		out.Counts["thread"]["declined"] != 1 || out.Counts["name_call"]["about_bot"] != 2 || out.Outcomes["follow"]["retagged"] != 1 {
		t.Fatalf("counts: %s", raw)
	}
	if len(out.Rows) != 3 {
		t.Fatalf("rows: %d", len(out.Rows))
	}
	if bad := exec(svc, "conversation.log", map[string]any{"guild_id": "g"}); bad.OK {
		t.Fatal("a channel is required")
	}
	// chat users never read it
	if chat := svc.Execute(context.Background(), action.Request{Action: "conversation.log", Input: json.RawMessage(`{"guild_id":"g","channel_id":"c"}`),
		Invoker: &action.Invoker{Via: "chat", UserID: "u1"}}); chat.OK {
		t.Fatal("conversation.log must be refused to a chat user")
	}
}
