package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/config"
)

// Room style (experimental): counted in code, a few model-picked traits with evidence, applied only after
// approval and only where the room turned it on; a deleted source message makes the version stale.
func TestRoomStyleApproveApplyAndStale(t *testing.T) {
	var plan, resp recordingServer
	planURL := plan.start(t, func(p string) string {
		if strings.Contains(p, "\"items\"") {
			// m1 and m2 are two people saying 개꿀; m3 alone says 존맛 (one person's habit is dropped)
			return `{"items":[{"kind":"slang","text":"개꿀","evidence":["m1","m2"]},{"kind":"slang","text":"존맛","evidence":["m3"]},{"kind":"avoid","text":"http://x","evidence":["m1"]}]}`
		}
		return `{"memory_requirement":"none","tool_plan":[]}`
	})
	respURL := resp.start(t, func(string) string { return "ㅇㅋ 개꿀" })
	svc := routedSvc(t, planURL, respURL)
	applyDoc(t, svc, func(d *config.Document) {
		d.Models["summary"] = d.Models["planner"]
		if d.Guilds == nil {
			d.Guilds = map[string]config.GuildSettings{}
		}
		d.Guilds["g"] = config.GuildSettings{Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention"}}}
	})
	if _, err := svc.MemoryEngine(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := 0; i < 60; i++ {
		text := []string{"오늘 점심 개꿀이었음 ㅋㅋㅋ", "ㄹㅇ 개꿀 ㅋㅋ", "존맛 존맛", "내일 뭐함?"}[i%4]
		if _, err := svc.Store.DB.Exec(`INSERT INTO memory_observations(observation_id, platform, collection, source_room, room, native_message_id, actor_account, actor_label, content, parsed_time, record_kind, imported_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, fmt.Sprintf("o%02d", i), "discord", "c", "discord:g:c", "discord:g:c", fmt.Sprintf("n%02d", i),
			fmt.Sprintf("u%d", i%4), fmt.Sprintf("P%d", i%4), text, now.Add(time.Duration(i-60)*time.Minute).Format(time.RFC3339), "human_text", now.Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	dry := exec(svc, "room.style.build", map[string]any{"guild_id": "g", "channel_id": "c", "dry_run": true})
	if !dry.OK {
		t.Fatalf("%+v", dry.Error)
	}
	raw, _ := json.Marshal(dry.Output)
	if !strings.Contains(string(raw), `"laughs":["ㅋㅋ"]`) || !strings.Contains(string(raw), `"enough":true`) {
		t.Fatalf("counted stats: %s", raw)
	}
	built := exec(svc, "room.style.build", map[string]any{"guild_id": "g", "channel_id": "c"})
	if !built.OK {
		t.Fatalf("%+v", built.Error)
	}
	raw, _ = json.Marshal(built.Output)
	if !strings.Contains(string(raw), "개꿀") || strings.Contains(string(raw), "존맛\",\"evidence") || strings.Contains(string(raw), "http") {
		t.Fatalf("only checked items stay: %s", raw)
	}
	turn := func(id string) string {
		t.Helper()
		if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id, Message: "@마사몽 점심 뭐 먹지",
			Speaker: "P0", SpeakerID: "u0", Deliver: func(_ context.Context, r chat.Result) string { return r.Answer }}); err != nil {
			t.Fatal(err)
		}
		all := resp.all()
		return all[len(all)-1]
	}
	// pending, or approved but the room has it off: nothing is added
	if strings.Contains(turn("x1"), "이 방 말투") {
		t.Fatal("a pending version is not used")
	}
	if r := exec(svc, "room.style.decide", map[string]any{"guild_id": "g", "channel_id": "c", "version": 1, "decision": "approve"}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	if strings.Contains(turn("x2"), "이 방 말투") {
		t.Fatal("the room has room_style off")
	}
	applyDoc(t, svc, func(d *config.Document) {
		d.Guilds["g"] = config.GuildSettings{Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention",
			Conversation: config.ChannelConversation{RoomStyle: "on"}}}}
	})
	if got := turn("x3"); !strings.Contains(got, "이 방 말투") || !strings.Contains(got, "개꿀") {
		t.Fatal("an approved note is added where the room turned it on")
	}
	// a source message of an item is deleted: the version goes stale and is no longer used
	if err := svc.MessagesDeleted("g", "c", []string{"n01"}); err != nil {
		t.Fatal(err)
	}
	drain(t, svc)
	show := exec(svc, "room.style.show", map[string]any{"guild_id": "g", "channel_id": "c"})
	raw, _ = json.Marshal(show.Output)
	if !strings.Contains(string(raw), `"state":"stale"`) || strings.Contains(string(raw), "개꿀") {
		t.Fatalf("not stale: %s", raw)
	}
	if strings.Contains(turn("x4"), "이 방 말투") {
		t.Fatal("a stale version is not used")
	}
	if r := exec(svc, "room.style.show", map[string]any{"guild_id": "g", "channel_id": "c"}); !r.OK {
		t.Fatal(r.Error)
	}
}

// styleRoom is a room of 60 lines by u0..u3 (u0 and u1 say 개꿀) and a model that picks 개꿀 from u0's and
// u1's lines and one topic to avoid; during is called with the item prompt before the model answers.
func styleRoom(t *testing.T, during func()) *application.Service {
	t.Helper()
	var model recordingServer
	url := model.start(t, func(p string) string {
		if strings.Contains(p, "\"items\"") {
			if during != nil {
				during()
			}
			return `{"items":[{"kind":"slang","text":"개꿀","evidence":["m1","m2"]},{"kind":"avoid","text":"시험 얘기","evidence":["m1"]}]}`
		}
		return `{"memory_requirement":"none","tool_plan":[]}`
	})
	svc := routedSvc(t, url, url)
	applyDoc(t, svc, func(d *config.Document) {
		d.Models["summary"] = d.Models["planner"]
		d.Guilds = map[string]config.GuildSettings{"g": {Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention"}}}}
	})
	if _, err := svc.MemoryEngine(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	for i := 0; i < 60; i++ {
		text := []string{"오늘 점심 개꿀이었음 ㅋㅋㅋ", "ㄹㅇ 개꿀 ㅋㅋ", "존맛 존맛", "내일 뭐함?"}[i%4]
		if _, err := svc.Store.DB.Exec(`INSERT INTO memory_observations(observation_id, platform, collection, source_room, room, native_message_id, actor_account, actor_label, content, parsed_time, record_kind, imported_at)
			VALUES(?,?,?,?,?,?,?,?,?,?,?,?)`, fmt.Sprintf("o%02d", i), "discord", "c", "discord:g:c", "discord:g:c", fmt.Sprintf("n%02d", i),
			fmt.Sprintf("u%d", i%4), fmt.Sprintf("P%d", i%4), text, now.Add(time.Duration(i-60)*time.Minute).Format(time.RFC3339), "human_text", now.Format(time.RFC3339)); err != nil {
			t.Fatal(err)
		}
	}
	return svc
}

func styleShown(t *testing.T, svc *application.Service) string {
	t.Helper()
	r := exec(svc, "room.style.show", map[string]any{"guild_id": "g", "channel_id": "c"})
	if !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	raw, _ := json.Marshal(r.Output)
	return string(raw)
}

// A line edited before the analysis read it is quoted as it is now: the edit left a tombstone, but that is
// no reason to refuse the version after its model call was paid. A line deleted while the model was
// thinking still never makes it into the note.
func TestRoomStyleEditedLineStillBuilds(t *testing.T) {
	var svc *application.Service
	deleteDuring := false
	svc = styleRoom(t, func() {
		if deleteDuring {
			deleteDuring = false
			if err := svc.MessagesDeleted("g", "c", []string{"n01"}); err != nil {
				t.Error(err)
			}
		}
	})
	if err := svc.MessageEdited("g", "c", "n00", "오늘 저녁도 개꿀이었음 ㅋㅋ", time.Now()); err != nil {
		t.Fatal(err)
	}
	drain(t, svc)
	built := exec(svc, "room.style.build", map[string]any{"guild_id": "g", "channel_id": "c"})
	if !built.OK {
		t.Fatalf("an edited line blocked the build: %+v", built.Error)
	}
	if raw, _ := json.Marshal(built.Output); !strings.Contains(string(raw), "개꿀") {
		t.Fatalf("the edited line is quoted as it is now: %s", raw)
	}
	deleteDuring = true
	built = exec(svc, "room.style.build", map[string]any{"guild_id": "g", "channel_id": "c"})
	if !built.OK {
		t.Fatalf("%+v", built.Error)
	}
	raw, _ := json.Marshal(built.Output)
	if strings.Contains(string(raw), "개꿀") || !strings.Contains(string(raw), "시험 얘기") {
		t.Fatalf("an item resting on a line deleted during the build stays (or the rest went): %s", raw)
	}
}

// "Don't analyze me" during the model call keeps the person's lines out of the version being saved, and
// withdrawing after an approval makes the version that quotes the person stale.
func TestRoomStyleProfileWithdrawal(t *testing.T) {
	var svc *application.Service
	svc = styleRoom(t, func() {
		if r := as(svc, "privacy.consent", map[string]any{"scope": "profile", "decision": "withdraw"}, inChannel("u1")); !r.OK {
			t.Errorf("withdraw: %+v", r.Error)
		}
	})
	built := exec(svc, "room.style.build", map[string]any{"guild_id": "g", "channel_id": "c"})
	if !built.OK {
		t.Fatalf("%+v", built.Error)
	}
	raw, _ := json.Marshal(built.Output)
	if strings.Contains(string(raw), "개꿀") || !strings.Contains(string(raw), `"people":3`) || !strings.Contains(string(raw), "시험 얘기") {
		t.Fatalf("u1, who withdrew during the build, is in the version: %s", raw)
	}
	if r := exec(svc, "room.style.decide", map[string]any{"guild_id": "g", "channel_id": "c", "version": 1, "decision": "approve"}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	if r := as(svc, "privacy.consent", map[string]any{"scope": "profile", "decision": "withdraw"}, inChannel("u0")); !r.OK {
		t.Fatalf("withdraw: %+v", r.Error)
	}
	if show := styleShown(t, svc); !strings.Contains(show, `"state":"stale"`) || strings.Contains(show, "시험 얘기") {
		t.Fatalf("the approved version quoting u0 is not stale: %s", show)
	}
}
