package application_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/chat"
)

// scopeFixture is one person's reminders in two rooms and in DMs, all still proposed.
func scopeFixture(t *testing.T) (svc *application.Service, here, there, dm string) {
	t.Helper()
	seoul, _ := time.LoadLocation("Asia/Seoul")
	svc, _ = remindSvc(t, time.Date(2026, 9, 28, 10, 0, 0, 0, seoul))
	propose := func(text string, inv *action.Invoker) string {
		t.Helper()
		r := as(svc, "reminders.propose", map[string]any{"text": "tomorrow at 3pm " + text}, inv)
		if !r.OK {
			t.Fatalf("propose %q: %+v", text, r)
		}
		return reminderID(t, r)
	}
	room := func(ch string) *action.Invoker {
		return &action.Invoker{Via: "command", GuildID: "g", ChannelID: ch, UserID: "u1", Name: "member"}
	}
	here = propose("water the plants", room("c"))
	propose("feed the cat", room("c"))
	there = propose("walrus fixture errand", room("other"))
	dm = propose("zebra fixture errand", &action.Invoker{Via: "command", ChannelID: "dm1", UserID: "u1", Private: true})
	return svc, here, there, dm
}

func chatRun(svc *application.Service, guild, channel string) func(map[string]any) chat.ToolResult {
	run := application.ChatToolForTest(svc, application.ChatInput{GuildID: guild, ChannelID: channel, SpeakerID: "u1", Speaker: "member"})
	return func(in map[string]any) chat.ToolResult { return run(context.Background(), "reminders.update", in) }
}

func seen(r chat.ToolResult) string {
	b, _ := json.Marshal(r)
	return string(b)
}

// A chat turn in a room changes, and is shown, only the reminders made in that room: cancel, pause and
// resume by words, by words that point at another room's or a DM's reminder, or by its id, never reach
// a reminder made elsewhere, and their names never come back to the model. A task's turn is the same.
func TestChatTurnInARoomChangesOnlyThatRoomsReminders(t *testing.T) {
	svc, here, there, dm := scopeFixture(t)
	run := chatRun(svc, "g", "c")
	for _, in := range []map[string]any{
		{"op": "cancel"},
		{"op": "cancel", "match": "zebra fixture"},
		{"op": "cancel", "match": "walrus fixture"},
		{"op": "cancel", "match": "nothing like it"},
		{"op": "cancel", "id": dm},
		{"op": "cancel", "id": there},
		{"op": "pause", "id": dm},
		{"op": "resume", "id": there},
		{"op": "pause", "match": "zebra"},
	} {
		out := seen(run(in))
		if strings.Contains(out, "zebra") || strings.Contains(out, "walrus") {
			t.Errorf("%v from a chat turn in c showed a reminder made elsewhere: %s", in, out)
		}
		for _, id := range []string{dm, there} {
			if got := remStatus(t, svc, id); got != "proposed" {
				t.Fatalf("%v from a chat turn in c left a reminder made elsewhere %s", in, got)
			}
		}
	}
	// a scheduled task's turn in the room is held to the same rule
	task := application.TaskToolForTest(svc, application.ChatInput{GuildID: "g", ChannelID: "c", SpeakerID: "u1", Speaker: "member"})
	if out := seen(task(context.Background(), "reminders.update", map[string]any{"op": "list"})); strings.Contains(out, "zebra") || strings.Contains(out, "walrus") || !strings.Contains(out, "plants") {
		t.Errorf("a task's turn in c listed: %s", out)
	}
	// the room's own reminder is still reached by words
	if r := run(map[string]any{"op": "cancel", "match": "plants"}); !r.OK || remStatus(t, svc, here) != "cancelled" {
		t.Fatalf("cancel of this room's reminder: %+v", r)
	}
	// the person's own command (its card is shown to them alone) reaches every reminder of theirs
	if r := as(svc, "reminders.update", map[string]any{"op": "cancel", "id": dm}, inChannel("u1")); !r.OK || remStatus(t, svc, dm) != "cancelled" {
		t.Fatalf("the person's own cancel of their DM reminder: %+v", r)
	}
}

// A DM chat is the person's alone: it lists, and changes, the reminders they made by DM command (stored
// with no channel) as well as the ones made in rooms. A chat turn in a room is the exclusion control.
func TestDMChatListsTheRemindersMadeInDMs(t *testing.T) {
	svc, _, there, dm := scopeFixture(t)
	if out := seen(chatRun(svc, "", "dm1")(map[string]any{"op": "list"})); !strings.Contains(out, "zebra") || !strings.Contains(out, "walrus") {
		t.Fatalf("a DM chat's list leaves out the person's reminders: %s", out)
	}
	if out := seen(chatRun(svc, "g", "c")(map[string]any{"op": "list"})); strings.Contains(out, "zebra") || strings.Contains(out, "walrus") {
		t.Fatalf("a chat turn in c lists reminders made elsewhere: %s", out)
	}
	run := chatRun(svc, "", "dm1")
	if r := run(map[string]any{"op": "cancel", "match": "zebra fixture"}); !r.OK || remStatus(t, svc, dm) != "cancelled" {
		t.Fatalf("a DM chat's cancel of a DM reminder by words: %+v", r)
	}
	if r := run(map[string]any{"op": "cancel", "id": there}); !r.OK || remStatus(t, svc, there) != "cancelled" {
		t.Fatalf("a DM chat's cancel of a room reminder by id: %+v", r)
	}
}
