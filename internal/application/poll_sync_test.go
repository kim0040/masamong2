package application_test

import (
	"encoding/json"
	"strings"
	"testing"

	"masamong/internal/errclass"
)

// Votes in a Discord poll reach the bot's record of it; a deleted poll message closes the poll.
func TestDiscordPollVotesSync(t *testing.T) {
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
	svc := setupSvc(t)
	svc.NoteDeliveryProblem("g", "c", errclass.Permission)
	raw, _ := json.Marshal(exec(svc, "runtime.doctor", nil).Output)
	if !strings.Contains(string(raw), "delivery_permission:1") {
		t.Fatalf("doctor: %s", raw)
	}
}
