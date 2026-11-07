package application_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/clock"
)

// A channel whose thread offers keep being passed over rests (6h after the second, doubling), the rest
// shows in the decision log once per conversation, an opened thread starts it over, and a restart keeps it
// (the state is read from the log).
func TestThreadOffersBackOffWhenNobodyTakesThemUp(t *testing.T) {
	var plan, resp recordingServer
	planURL := plan.start(t, func(string) string { return `{"memory_requirement":"none","tool_plan":[]}` })
	respURL := resp.start(t, func(string) string { return strings.Repeat("길게 설명하는 답이야. ", 30) })
	svc := routedSvc(t, planURL, respURL)
	svc.Discord = &postGateway{}
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	n := 0
	// a conversation of `turns` long answers from one person at the given time; reports whether an offer came
	round := func(at time.Time, turns int) (offered bool) {
		svc.Clock = clock.Fixed{T: at}
		for i := 0; i < turns; i++ {
			n++
			id := "m" + string(rune('a'+n%26)) + time.Duration(n).String()
			svc.Observe(application.ObservedMessage{GuildID: "g", ChannelID: "c", MessageID: id, AuthorID: "uc", AuthorName: "Chris",
				Content: "@마사몽 제주 일정", At: at, Mentions: []string{"bot"}})
			res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id,
				Message: "@마사몽 제주 일정", Speaker: "Chris", SpeakerID: "uc",
				Deliver: func(_ context.Context, r chat.Result) string { return r.Answer }})
			if err != nil {
				t.Fatal(err)
			}
			if _, ok := svc.TakeThreadOffer(res.TurnID); ok {
				offered = true
			}
		}
		return offered
	}
	count := func(decision string) (c int) {
		for _, d := range svc.ConversationLogForTest("thread") {
			if d == decision {
				c++
			}
		}
		return c
	}
	if !round(t0, 6) || !round(t0.Add(2*time.Hour), 6) {
		t.Fatalf("the first two offers are made: %v", svc.ConversationLogForTest("thread"))
	}
	if round(t0.Add(4*time.Hour), 7) { // the 7th answer is the same conversation: still one log row
		t.Fatalf("two offers passed over: the third is held back: %v", svc.ConversationLogForTest("thread"))
	}
	if count("skip_backoff") != 1 || count("offer") != 2 {
		t.Fatalf("the rest is logged once per conversation: %v", svc.ConversationLogForTest("thread"))
	}
	if !svc.ThreadBackedOffForTest("g", "c") {
		t.Fatal("the channel rests")
	}
	svc = restartServiceForTest(t, svc)
	svc.Clock = clock.Fixed{T: t0.Add(5 * time.Hour)}
	if !svc.ThreadBackedOffForTest("g", "c") {
		t.Fatal("a restart keeps the rest: it is read from the log")
	}
	svc.Clock = clock.Fixed{T: t0.Add(2*time.Hour + 6*time.Hour + time.Minute)}
	if svc.ThreadBackedOffForTest("g", "c") {
		t.Fatal("6 hours after the second offer the channel is asked again")
	}
}

// One opened thread starts the count over, and the rest lengthens with every offer passed over.
func TestOpenedThreadEndsTheBackoffAndRestLengthens(t *testing.T) {
	svc := setupSvc(t)
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	svc.Clock = clock.Fixed{T: t0}
	for i := 0; i < 3; i++ {
		svc.LogThreadOfferForTest("g", "c", t0.Add(time.Duration(i)*2*time.Hour))
	}
	// offers at 0h, 2h, 4h: three passed over → 12h from the last
	svc.Clock = clock.Fixed{T: t0.Add(4*time.Hour + 11*time.Hour)}
	if !svc.ThreadBackedOffForTest("g", "c") {
		t.Fatal("three passed over: 12h rest from the last offer")
	}
	svc.Clock = clock.Fixed{T: t0.Add(4*time.Hour + 12*time.Hour + time.Minute)}
	if svc.ThreadBackedOffForTest("g", "c") {
		t.Fatal("the 12h rest is over")
	}
	svc.Clock = clock.Fixed{T: t0.Add(5 * time.Hour)}
	svc.ThreadOpened(context.Background(), "g", "c", "th", "uc", "")
	if svc.ThreadBackedOffForTest("g", "c") {
		t.Fatal("an opened thread starts it over")
	}
}
