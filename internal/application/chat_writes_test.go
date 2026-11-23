package application_test

import (
	"reflect"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
)

func chatAs(channel, user string) *action.Invoker {
	return &action.Invoker{Via: "chat", GuildID: "g", ChannelID: channel, UserID: user, Name: "철수"}
}

// A model in a chat turn reads the proposal's id, but agreeing to a time is the person's own press of a
// button: reminders.update {op: confirm | pick | reschedule} from a chat turn is refused and the reminder
// stays proposed. The button (Via command) still sets it.
func TestChatTurnCannotConfirmAReminder(t *testing.T) {
	t.Parallel()
	seoul, _ := time.LoadLocation("Asia/Seoul")
	svc, _ := remindSvc(t, time.Date(2026, 9, 28, 10, 0, 0, 0, seoul))
	r := as(svc, "reminders.propose", map[string]any{"text": "tomorrow at 3pm call mom"}, inChannel("u1"))
	if !r.OK || r.Output["status"] != "proposed" {
		t.Fatalf("propose: %+v", r)
	}
	id := reminderID(t, r)
	for _, in := range []map[string]any{
		{"op": "confirm", "id": id, "destination": "dm"},
		{"op": "confirm", "id": id, "destination": "channel"},
		{"op": "pick", "id": id, "n": 0},
		{"op": "reschedule", "id": id, "text": "tomorrow at 4pm"},
	} {
		if r := as(svc, "reminders.update", in, chatAs("c", "u1")); r.OK || r.Error.Code != errcode.InvalidInput {
			t.Fatalf("%v from a chat turn: %+v", in["op"], r)
		}
		if got := remStatus(t, svc, id); got != "proposed" {
			t.Fatalf("%v from a chat turn left it %s", in["op"], got)
		}
	}
	// what a chat turn may do still works
	if r := as(svc, "reminders.update", map[string]any{"op": "list"}, chatAs("c", "u1")); !r.OK {
		t.Fatalf("list from a chat turn: %+v", r)
	}
	if r := as(svc, "reminders.update", map[string]any{"op": "confirm", "id": id, "destination": "dm"}, inChannel("u1")); !r.OK || r.Output["status"] != "scheduled" {
		t.Fatalf("the button: %+v", r)
	}
}

// A chat turn in a room is given only the reminders the person made in that room; the person's own
// command (shown to them alone) and a DM chat list all of them.
func TestChatTurnListsOnlyThisRoomsReminders(t *testing.T) {
	t.Parallel()
	seoul, _ := time.LoadLocation("Asia/Seoul")
	svc, _ := remindSvc(t, time.Date(2026, 9, 28, 10, 0, 0, 0, seoul))
	for _, ch := range []string{"c", "other"} {
		inv := &action.Invoker{Via: "command", GuildID: "g", ChannelID: ch, UserID: "u1", Name: "철수"}
		if r := as(svc, "reminders.propose", map[string]any{"text": "tomorrow at 3pm note " + ch}, inv); !r.OK {
			t.Fatalf("propose in %s: %+v", ch, r)
		}
	}
	count := func(r action.Result) int {
		t.Helper()
		if !r.OK {
			t.Fatalf("list: %+v", r)
		}
		v := reflect.ValueOf(r.Output["reminders"])
		if v.Kind() != reflect.Slice {
			t.Fatalf("list output %T", r.Output["reminders"])
		}
		return v.Len()
	}
	if n := count(as(svc, "reminders.update", map[string]any{"op": "list"}, chatAs("c", "u1"))); n != 1 {
		t.Fatalf("chat turn in c saw %d reminders, want 1", n)
	}
	if n := count(as(svc, "reminders.update", map[string]any{"op": "list"}, inChannel("u1"))); n != 2 {
		t.Fatalf("the person's own list has %d, want 2", n)
	}
	dm := &action.Invoker{Via: "chat", ChannelID: "dm1", UserID: "u1", Private: true}
	if n := count(as(svc, "reminders.update", map[string]any{"op": "list"}, dm)); n != 2 {
		t.Fatalf("a DM chat has %d, want 2", n)
	}
}

// Making a poll never replaces one: a chat turn or a command always gets a new id (the model may have
// read another poll's), and the console's own id is refused when it is taken.
func TestPollCreateNeverOverwrites(t *testing.T) {
	t.Parallel()
	svc := setupSvc(t)
	serveAnyServer(t, svc)
	first := exec(svc, "polls.create", map[string]any{"id": "poll_fixed", "question": "lunch?", "options_text": "a\nb", "scope": "discord:c"})
	if !first.OK || first.Output["id"] != "poll_fixed" {
		t.Fatalf("console create: %+v", first)
	}
	for _, inv := range []*action.Invoker{chatAs("c", "u1"), inChannel("u1")} {
		r := as(svc, "polls.create", map[string]any{"id": "poll_fixed", "question": "free money?"}, inv)
		if !r.OK || r.Output["id"] == "poll_fixed" || r.Output["id"] == "" {
			t.Fatalf("%s create with a taken id: %+v", inv.Via, r)
		}
	}
	if r := exec(svc, "polls.create", map[string]any{"id": "poll_fixed", "question": "again?"}); r.OK {
		t.Fatalf("console create over a taken id: %+v", r)
	}
	if r := exec(svc, "polls.result", map[string]any{"id": "poll_fixed"}); !r.OK || r.Output["question"] != "lunch?" {
		t.Fatalf("the first poll changed: %+v", r)
	}
}
