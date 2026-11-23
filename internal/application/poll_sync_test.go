package application_test

import (
	"encoding/json"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/errclass"
)

// Votes in a Discord poll reach the bot's record of it; a deleted poll message closes the poll.
func TestDiscordPollVotesSync(t *testing.T) {
	t.Parallel()
	svc := setupSvc(t)
	r := exec(svc, "polls.create", map[string]any{"question": "점심?", "options_text": "국밥\n라멘", "scope": "discord:c"})
	if !r.OK {
		t.Fatal(r)
	}
	id := r.Output["id"].(string)
	svc.LinkPollMessage(id, "c", "m1")
	svc.PollVote("m1", "u1", 2, true)
	svc.PollVote("m1", "u2", 2, true)
	svc.PollVote("m1", "u2", 1, true) // single choice: the newer one replaces
	svc.PollVote("m1", "u3", 1, true)
	svc.PollVote("m1", "u3", 1, false)
	res := exec(svc, "polls.result", map[string]any{"id": id})
	if res.Output["unique_voters"] != 2 {
		t.Fatalf("voters: %v", res.Output)
	}
	if err := svc.MessagesDeleted("g", "c", []string{"m1"}); err != nil {
		t.Fatal(err)
	}
	if res := exec(svc, "polls.result", map[string]any{"id": id}); res.Output["status"] != "closed" {
		t.Fatalf("status after the message was deleted: %v", res.Output["status"])
	}
}

// A reply Discord refused for lack of permission shows in doctor until its alert goes stale (LIVE-16).
func TestDoctorShowsDeliveryPermissionProblems(t *testing.T) {
	t.Parallel()
	svc := setupSvc(t)
	svc.NoteDeliveryProblem("g", "c", errclass.Permission)
	raw, _ := json.Marshal(exec(svc, "runtime.doctor", nil).Output)
	if !strings.Contains(string(raw), "delivery_permission:1") {
		t.Fatalf("doctor: %s", raw)
	}
}

// The poll's options arrive as a list (the console's saved defaults, a model's call) or, from a
// configuration saved while the console flattened lists, as one comma-separated text: both name the
// same choices. Nothing named gives yes/no.
func TestPollCreateAcceptsOptionsAsListOrText(t *testing.T) {
	t.Parallel()
	svc := setupSvc(t)
	labels := func(r action.Result) []string {
		t.Helper()
		if !r.OK {
			t.Fatalf("create %+v", r.Error)
		}
		var out []string
		for _, o := range r.Output["options"].([]any) {
			out = append(out, o.(map[string]any)["label"].(string))
		}
		return out
	}
	for name, options := range map[string]any{"list": []string{"Tea", "Coffee"}, "text": "Tea, Coffee", "lines": "Tea\nCoffee"} {
		got := labels(exec(svc, "polls.create", map[string]any{"question": "drink? " + name, "options": options}))
		if len(got) != 2 || got[0] != "Tea" || got[1] != "Coffee" {
			t.Fatalf("%s: options %v", name, got)
		}
	}
	if got := labels(exec(svc, "polls.create", map[string]any{"question": "plain"})); len(got) != 2 || got[0] != "Yes" {
		t.Fatalf("default options %v", got)
	}
}
