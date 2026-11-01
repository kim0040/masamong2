package application_test

import (
	"testing"

	"masamong/internal/action"
	"masamong/internal/application"
)

func TestAdminCommandNeedsOperatorOrServerManager(t *testing.T) {
	svc := setupSvc(t)
	serveAnyServer(t, svc)
	member := &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "111111"}
	manager := &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "222222", Manager: true}
	if r := as(svc, "access.policy", map[string]any{"op": "pause"}, member); r.OK || r.Error.Code != "PERMISSION_DENIED" {
		t.Fatalf("a member cannot pause: %+v", r)
	}
	if r := as(svc, "access.policy", map[string]any{"op": "pause"}, manager); !r.OK || r.Output["paused"] != true {
		t.Fatalf("a server manager can: %+v", r)
	}
	if !svc.Paused("g", "c") {
		t.Fatal("the channel is paused")
	}
	// feature commands are refused in a paused channel; consent and settings are not
	if r := as(svc, "summaries.channel", nil, member); r.OK || r.Error.MessageKey != "runtime.paused" {
		t.Fatalf("paused summary: %+v", r)
	}
	if r := as(svc, "preferences.set", map[string]any{"weather_region": "전주"}, member); !r.OK {
		t.Fatalf("settings in a paused channel: %+v", r)
	}
	if r := as(svc, "access.policy", map[string]any{"op": "respond", "mode": "always"}, manager); !r.OK || r.Output["respond"] != "always" {
		t.Fatalf("respond: %+v", r)
	}
	// server managers can be switched off; operators work everywhere
	if r := exec(svc, "access.policy", map[string]any{"guild_admins": false, "operators": []string{"111111"}, "blocked_users": []string{"333333"}}); !r.OK {
		t.Fatalf("policy: %+v", r)
	}
	if r := as(svc, "access.policy", map[string]any{"op": "resume"}, manager); r.OK {
		t.Fatal("server managers were switched off")
	}
	if r := as(svc, "access.policy", map[string]any{"op": "resume"}, member); !r.OK || svc.Paused("g", "c") {
		t.Fatalf("an operator can resume: %+v", r)
	}
	blocked := &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "333333"}
	if r := as(svc, "weather.query", map[string]any{"fixture": "clear"}, blocked); r.OK || r.Error.MessageKey != "access.blocked" {
		t.Fatalf("blocked users are refused: %+v", r)
	}
	if r := exec(svc, "access.policy", map[string]any{"operators": []string{"not-an-id"}}); r.OK {
		t.Fatal("ids are validated")
	}
}

func TestUserSettingsFillTheWeatherPlaceAndQuietHours(t *testing.T) {
	svc := setupSvc(t)
	serveAnyServer(t, svc)
	u := &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "u1"}
	if r := as(svc, "preferences.set", map[string]any{"weather_region": "부산", "quiet_start": "23:00", "quiet_end": "07:00", "timezone": "Asia/Seoul"}, u); !r.OK || r.Output["changed"] != true {
		t.Fatalf("set: %+v", r)
	}
	if r := as(svc, "preferences.set", map[string]any{"quiet_start": "25:00", "quiet_end": "07:00"}, u); r.OK {
		t.Fatal("bad time accepted")
	}
	if r := as(svc, "preferences.set", map[string]any{"timezone": "Mars/Base"}, u); r.OK {
		t.Fatal("bad zone accepted")
	}
	r := as(svc, "weather.query", map[string]any{"fixture": "clear"}, u)
	if !r.OK || r.Output["region"] != "부산" {
		t.Fatalf("the weather place comes from the settings: %+v", r)
	}
	r = as(svc, "weather.query", map[string]any{"fixture": "clear", "region": "서울"}, u)
	if r.Output["region"] != "서울" {
		t.Fatalf("an explicit place wins: %+v", r.Output)
	}
	list := exec(svc, "preferences.set", nil)
	if n := len(list.Output["users"].([]application.UserPrefs)); n != 1 {
		t.Fatalf("console list: %d", n)
	}
	if r := as(svc, "preferences.set", map[string]any{"reset": true}, u); !r.OK {
		t.Fatal(r)
	}
	if n := len(exec(svc, "preferences.set", nil).Output["users"].([]application.UserPrefs)); n != 0 {
		t.Fatalf("reset removes the row: %d", n)
	}
}
