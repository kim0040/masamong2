package application_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"masamong/internal/application"
	"masamong/internal/config"
)

// nameCallSvc is a service whose channel answers mentions, with a classifier that hears verdict.
func nameCallSvc(t *testing.T, verdict string) (*application.Service, *recordingServer, *recordingServer) {
	t.Helper()
	var plan, resp recordingServer
	planURL := plan.start(t, func(p string) string {
		if strings.Contains(p, "about_bot") {
			return verdict
		}
		return `{"memory_requirement":"none","tool_plan":[]}`
	})
	respURL := resp.start(t, func(string) string { return "왜 불러" })
	svc := routedSvc(t, planURL, respURL)
	applyDoc(t, svc, func(d *config.Document) {
		if d.Guilds == nil {
			d.Guilds = map[string]config.GuildSettings{}
		}
		d.Guilds["g"] = config.GuildSettings{Channels: map[string]config.ChannelPolicy{"c": {Respond: "mention"}}}
	})
	return svc, &plan, &resp
}

func classified(plan *recordingServer) int {
	n := 0
	for _, p := range plan.all() {
		if strings.Contains(p, "about_bot") {
			n++
		}
	}
	return n
}

// A name said to the bot tells the caller once, before the answer; talk about the bot does not.
func TestNameOnlyCallTellsTheCallerItWasAddressed(t *testing.T) {
	for _, c := range []struct {
		verdict string
		calls   int
	}{{`{"to":"bot","tone":"neutral"}`, 1}, {`{"to":"about_bot","tone":"playful"}`, 0}} {
		svc, _, resp := nameCallSvc(t, c.verdict)
		calls, answersAtCall := 0, -1
		_, _ = svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "9",
			Message: "마사몽 또 고장났네 ㅋㅋ", Speaker: "Chris", SpeakerID: "uc", NameOnly: true,
			Addressed: func() { calls++; answersAtCall = len(resp.all()) }})
		if calls != c.calls {
			t.Fatalf("%s: Addressed called %d times", c.verdict, calls)
		}
		if calls == 1 && answersAtCall != 0 {
			t.Fatalf("%s: Addressed came after the answer model was asked", c.verdict)
		}
	}
}

// Where the monthly budget is spent, a name alone is not classified (a model call) and gets no budget
// notice: the turn is "not addressed".
func TestNameOnlyCallOverTheBudgetIsNotClassified(t *testing.T) {
	svc, plan, resp := nameCallSvc(t, `{"to":"bot","tone":"neutral"}`)
	if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "1",
		Message: "@마사몽 안녕", Speaker: "Chris", SpeakerID: "uc"}); err != nil {
		t.Fatal(err)
	}
	applyDoc(t, svc, func(d *config.Document) { d.Limits.MonthlyBudgetUSD = "0.000001" })
	answers, called := len(resp.all()), false
	_, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "2",
		Message: "마사몽 또 고장났네 ㅋㅋ", Speaker: "Chris", SpeakerID: "uc", NameOnly: true, Addressed: func() { called = true }})
	if !errors.Is(err, application.ErrNotAddressed) || errors.Is(err, application.ErrChatBudget) {
		t.Fatalf("got %v, want ErrNotAddressed alone", err)
	}
	if classified(plan) != 0 || len(resp.all()) != answers || called {
		t.Fatalf("nothing is asked: classifier %d, answers %d → %d, addressed %v", classified(plan), answers, len(resp.all()), called)
	}
	// the same turn said with a mention is refused for the budget, as before
	if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "3",
		Message: "@마사몽 안녕", Speaker: "Chris", SpeakerID: "uc"}); !errors.Is(err, application.ErrChatBudget) {
		t.Fatalf("a mention over the budget: %v", err)
	}
}
