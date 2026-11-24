package application_test

import (
	"strings"
	"testing"

	"masamong/internal/action"
)

func strList(t *testing.T, v any) []string {
	t.Helper()
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		var out []string
		for _, x := range l {
			out = append(out, x.(string))
		}
		return out
	case nil:
		return nil
	}
	t.Fatalf("not a list: %#v", v)
	return nil
}

// updates.notes reads the notes compiled into the binary: the running version by default, a named
// one on request, the one before it with previous. A Discord caller sees only what chat members can
// feel; the console and the CLI see the operator part too (unless they ask for one audience).
func TestUpdatesNotesFollowTheRunningVersionAndTheAudience(t *testing.T) {
	t.Parallel()
	svc := setupSvcVersion(t, "v1.5.1")
	serveAnyServer(t, svc)
	member := &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "u1"}

	console := out(t, svc, "updates.notes", nil)
	if console["version"] != "v1.5.1" || console["found"] != true || console["audience"] != "all" || console["is_running"] != true {
		t.Fatalf("console: %v", console)
	}
	if len(strList(t, console["user"])) == 0 || len(strList(t, console["operator"])) == 0 {
		t.Fatalf("the operator sees both parts: %v", console)
	}
	if !strings.Contains(console["markdown"].(string), "## v1.5.1 (2026-11-22)") || console["previous"] != "v1.4.0" {
		t.Fatalf("markdown or previous: %v", console)
	}

	r := as(svc, "updates.notes", nil, member)
	if !r.OK || r.Output["audience"] != "user" || len(strList(t, r.Output["operator"])) != 0 || len(strList(t, r.Output["user"])) == 0 {
		t.Fatalf("a Discord caller reads the user part only: %+v", r)
	}
	card := r.Output["card"].(action.Card)
	if !card.Ephemeral || !strings.Contains(card.Title, "v1.5.1") || strings.Contains(card.Text, "release check stopped") || !strings.Contains(card.Footer, "v1.4.0") {
		t.Fatalf("card: %+v", card)
	}
	// even an operator speaking in a chat room gets the chat-room view: the operator part is for the console
	svc2 := setupSvc(t)
	serveAnyServer(t, svc2)
	if r := exec(svc2, "access.policy", map[string]any{"operators": []string{"111111"}}); !r.OK {
		t.Fatal(r.Error)
	}
	op := &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "111111"}
	if r := as(svc2, "updates.notes", nil, op); !r.OK || len(strList(t, r.Output["operator"])) != 0 {
		t.Fatalf("operator in chat: %+v", r)
	}

	if o := out(t, svc, "updates.notes", map[string]any{"audience": "operator"}); len(strList(t, o["user"])) != 0 || len(strList(t, o["operator"])) == 0 {
		t.Fatalf("operator only: %v", o)
	}
	// a named version and the one before the running one
	if o := out(t, svc, "updates.notes", map[string]any{"version": "1.1.1"}); o["version"] != "v1.1.1" || o["is_running"] != false {
		t.Fatalf("named: %v", o)
	}
	if o := out(t, svc, "updates.notes", map[string]any{"previous": true, "audience": "user"}); o["version"] != "v1.4.0" || o["previous"] != "v1.3.0" {
		t.Fatalf("previous: %v", o)
	}
	if r := exec(svc, "updates.notes", map[string]any{"version": "v9.9.9"}); r.OK || r.Error.MessageKey != "updates.news.unknown" {
		t.Fatalf("an unknown version is refused: %+v", r)
	}
	// the oldest note has nothing before it
	if o := out(t, svc, "updates.notes", map[string]any{"version": "v1.0.0", "previous": true}); o["found"] != false {
		t.Fatalf("before the first: %v", o)
	}
}

// The language follows the caller's locale (their saved one first); a language the note lacks falls
// back to English.
func TestUpdatesNotesSpeakTheCallersLanguage(t *testing.T) {
	t.Parallel()
	svc := setupSvcVersion(t, "v1.4.0")
	serveAnyServer(t, svc)
	u := &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "u1"}
	ko := svc.Execute(t.Context(), action.Request{Action: "updates.notes", Locale: "ko-KR", Invoker: u})
	if !ko.OK || ko.Output["language"] != "ko" || !strings.Contains(ko.Output["markdown"].(string), "### 새 기능과 수정") {
		t.Fatalf("ko: %+v", ko)
	}
	if r := as(svc, "preferences.set", map[string]any{"locale": "ja"}, u); !r.OK {
		t.Fatal(r.Error)
	}
	// ja has no hand-written note for v1.4.0: English stands in under the Japanese heading
	ja := svc.Execute(t.Context(), action.Request{Action: "updates.notes", Locale: "ko", Invoker: u})
	if ja.Output["language"] != "ja" || !strings.Contains(ja.Output["markdown"].(string), "### 新機能と修正") {
		t.Fatalf("a saved language wins and falls back to English text: %+v", ja.Output)
	}
}

// A local build, a build after a tag and a pre-release: a build after a tag shows the last note up to
// its tag; a local build has none.
func TestUpdatesNotesForBuilds(t *testing.T) {
	t.Parallel()
	svc := setupSvc(t)
	for _, c := range []struct {
		version string
		found   bool
		shows   string
	}{
		{"1.0.0-dev", false, ""},
		{"v1.5.1-3-gabc1234", true, "v1.5.1"},
		{"v1.4.2", true, "v1.4.0"},
		{"v2.0.0", true, "v1.7.1"},
	} {
		restore := svc.SetVersionForTest(c.version)
		o := out(t, svc, "updates.notes", nil)
		restore()
		if o["found"] != c.found || (c.found && o["version"] != c.shows) {
			t.Errorf("%s: %v", c.version, o)
		}
	}
	defer svc.SetVersionForTest("v1.4.2")()
	if o := out(t, svc, "updates.notes", nil); o["is_running"] != false {
		t.Errorf("a version without notes of its own does not claim to be the running one: %v", o)
	}
}

// updates.subscribe is the person's own switch in their settings row: it needs no other setting, it
// goes away with the row when switched off, and resetting the other settings leaves it alone.
func TestUpdatesSubscribeIsThePersonsOwn(t *testing.T) {
	t.Parallel()
	svc := setupSvc(t)
	serveAnyServer(t, svc)
	u := &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "u1"}
	other := &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "u2"}
	if r := as(svc, "updates.subscribe", map[string]any{"op": "status"}, u); !r.OK || r.Output["enabled"] != false {
		t.Fatalf("default off: %+v", r)
	}
	if r := as(svc, "updates.subscribe", map[string]any{"op": "on"}, u); !r.OK || r.Output["enabled"] != true || r.Output["changed"] != true {
		t.Fatalf("on: %+v", r)
	}
	if r := as(svc, "updates.subscribe", map[string]any{"op": "on"}, u); r.Output["changed"] != false {
		t.Fatalf("again: %+v", r)
	}
	// a person cannot subscribe somebody else, whatever they send
	if r := as(svc, "updates.subscribe", map[string]any{"op": "on", "user_id": "u2"}, u); !r.OK {
		t.Fatal(r.Error)
	}
	if r := as(svc, "updates.subscribe", nil, other); r.Output["enabled"] != false {
		t.Fatalf("u2 stays off: %+v", r)
	}
	users := out(t, svc, "preferences.set", nil)["users"].([]any)
	if len(users) != 1 || users[0].(map[string]any)["update_news"] != true {
		t.Fatalf("the console list shows it: %v", users)
	}
	// other settings come and go without touching it
	if r := as(svc, "preferences.set", map[string]any{"weather_region": "부산"}, u); !r.OK {
		t.Fatal(r.Error)
	}
	if r := as(svc, "preferences.set", map[string]any{"reset": true}, u); !r.OK {
		t.Fatal(r.Error)
	}
	if r := as(svc, "updates.subscribe", nil, u); r.Output["enabled"] != true {
		t.Fatalf("reset keeps the subscription: %+v", r)
	}
	if r := as(svc, "updates.subscribe", map[string]any{"op": "off"}, u); !r.OK || r.Output["enabled"] != false {
		t.Fatalf("off: %+v", r)
	}
	if users := out(t, svc, "preferences.set", nil)["users"].([]any); len(users) != 0 {
		t.Fatalf("the row goes when nothing is left in it: %v", users)
	}
	if r := as(svc, "updates.subscribe", map[string]any{"op": "maybe"}, u); r.OK {
		t.Fatal("unknown op")
	}
	// the console names the person
	if r := exec(svc, "updates.subscribe", map[string]any{"op": "on"}); r.OK {
		t.Fatal("the console must name a person")
	}
	if r := exec(svc, "updates.subscribe", map[string]any{"enabled": true, "user_id": "u3"}); !r.OK || r.Output["enabled"] != true {
		t.Fatalf("console subscribes u3: %+v", r)
	}
}

// A room's news switch starts off, is changed by channels.configure like the other room switches, and
// a server manager reaches it through the admin command in that room.
func TestRoomUpdateNewsSwitch(t *testing.T) {
	t.Parallel()
	svc := setupSvc(t)
	serveAnyServer(t, svc)
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "c", "respond": "mention"}); !r.OK || r.Output["update_news"] != false {
		t.Fatalf("default off: %+v", r)
	}
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "c", "update_news": "maybe"}); r.OK {
		t.Fatal("on or off only")
	}
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "c", "update_news": "on"}); !r.OK || r.Output["update_news"] != true {
		t.Fatalf("on: %+v", r)
	}
	if svc.ChannelPolicyFor("g", "c").UpdateNews != "on" {
		t.Fatal("stored")
	}
	manager := &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "222222", Manager: true}
	member := &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "111111"}
	if r := as(svc, "access.policy", map[string]any{"op": "news", "mode": "off"}, member); r.OK {
		t.Fatal("a member cannot")
	}
	if r := as(svc, "access.policy", map[string]any{"op": "news", "mode": "off"}, manager); !r.OK || svc.ChannelPolicyFor("g", "c").UpdateNews != "" {
		t.Fatalf("a manager turns it off: %+v", r)
	}
	if r := as(svc, "access.policy", map[string]any{"op": "news", "mode": "on"}, manager); !r.OK || svc.ChannelPolicyFor("g", "c").UpdateNews != "on" {
		t.Fatalf("and on: %+v", r)
	}
	if r := as(svc, "access.policy", map[string]any{"op": "news", "mode": "sometimes"}, manager); r.OK {
		t.Fatal("bad mode")
	}
	found := false
	for _, f := range as(svc, "access.policy", map[string]any{"op": "status"}, manager).Output["card"].(action.Card).Fields {
		if f.Name == "Update news" && f.Value == "On" {
			found = true
		}
	}
	if !found {
		t.Fatal("the status card shows the switch")
	}
}
