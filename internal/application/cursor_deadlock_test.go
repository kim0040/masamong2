package application_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
)

// On SQLite the writer and reader share one connection. A loop that keeps a rows cursor open and calls
// MemoryAllowed, whose opt-out cache queries the database again after any consent change, waited on its
// own cursor forever and froze every write in the process. These tests run the call with a deadline so a
// regression fails instead of hanging the suite.

func withinDeadline(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); f() }()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatalf("%s did not return: the database connection is held by its own cursor", what)
	}
}

func TestRankingAfterConsentChangeDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	svc := setupSvc(t)
	serveAnyServer(t, svc)
	_ = exec(svc, "channels.configure", map[string]any{"guild_id": "g1", "id": "c1", "respond": "mention"})
	svc.Observe(application.ObservedMessage{GuildID: "g1", ChannelID: "c1", MessageID: "m1", AuthorID: "1001", AuthorName: "Amy", Content: "x", At: time.Now()})
	svc.Observe(application.ObservedMessage{GuildID: "g1", ChannelID: "c1", MessageID: "m2", AuthorID: "1002", AuthorName: "Bob", Content: "y", At: time.Now()})
	// another person's withdrawal leaves the opt-out cache cold for the ranking loop
	if r := as(svc, "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}, &action.Invoker{Via: "command", GuildID: "g1", ChannelID: "c1", UserID: "1002"}); !r.OK {
		t.Fatal(r.Error)
	}
	raw, _ := json.Marshal(map[string]any{"period": "all", "scope": "discord:c1"})
	var r action.Result
	withinDeadline(t, "activity.ranking", func() {
		r = svc.Execute(context.Background(), action.Request{Action: "activity.ranking", Input: raw, Invoker: &action.Invoker{Via: "command", GuildID: "g1", ChannelID: "c1", UserID: "1001"}})
	})
	if !r.OK {
		t.Fatal(r.Error)
	}
	b, _ := json.Marshal(r.Output)
	if !strings.Contains(string(b), "Amy") || strings.Contains(string(b), "Bob") {
		t.Fatalf("ranking should list Amy and leave out Bob, who withdrew: %s", b)
	}
	// the writer is free afterwards
	withinDeadline(t, "a write after the ranking", func() {
		svc.Observe(application.ObservedMessage{GuildID: "g1", ChannelID: "c1", MessageID: "m3", AuthorID: "1001", AuthorName: "Amy", Content: "z", At: time.Now()})
	})
}

func TestReminderContextAfterConsentChangeDoesNotDeadlock(t *testing.T) {
	t.Parallel()
	now, _ := repSeoulNow()
	svc, _ := remCtxSvc(t, now)
	base := now.Add(-time.Minute)
	remObserve(svc, "a1", "u1", "Alex", "alpha words", base)
	remObserve(svc, "call", "u1", "Alex", "remind me about that", base.Add(10*time.Second), "bot")
	run := application.ChatToolForTest(svc, application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "call", Message: "remind me about that", Speaker: "Alex", SpeakerID: "u1"})
	res := run(context.Background(), "reminders.propose", map[string]any{"when": "tomorrow 9am", "what": "that", "with_context": true})
	c, _ := res.Data["card"].(action.Card)
	id := ""
	for _, b := range c.Buttons {
		if v, ok := strings.CutPrefix(b.ID, "remind:cancel:"); ok {
			id = v
		}
	}
	if id == "" {
		t.Fatalf("no reminder id: %+v", res)
	}
	// someone else withdraws: the cache is cold when the lines are read
	if r := as(svc, "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}, inChannel("u2")); !r.OK {
		t.Fatal(r.Error)
	}
	var got string
	withinDeadline(t, "reminder context lines", func() { got = application.ReminderContextForTest(svc, id) })
	if !strings.Contains(got, "alpha words") {
		t.Fatalf("lines of a person who did not withdraw must stay: %q", got)
	}
}
