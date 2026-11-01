package application_test

import (
	"context"
	"sync/atomic"
	"testing"

	"masamong/internal/application"
	"masamong/internal/chat"
)

// The channel conversation keeps what reached the channel: a reply that was withheld or failed to send
// is not one the next turn builds on, and a reply cut short is kept as far as it went.
func TestChatTurnKeepsOnlyTheDeliveredReply(t *testing.T) {
	var planCalls, respCalls atomic.Int32
	planURL := chatServer(t, `{"memory_requirement":"none","tool_plan":[]}`, "stop", &planCalls)
	respURL := chatServer(t, "첫 부분. 둘째 부분.", "stop", &respCalls)
	svc := routedSvc(t, planURL, respURL)
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	turn := func(channel string, deliver func(context.Context, chat.Result) string) []string {
		t.Helper()
		if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: channel, MessageID: "m-" + channel,
			Message: "안녕", Speaker: "철수", SpeakerID: "u1", Deliver: deliver}); err != nil {
			t.Fatal(err)
		}
		turns, _ := eng.Store.RecentChatTurns("discord:g:"+channel, 10)
		var out []string
		for _, tr := range turns {
			out = append(out, tr.Role+":"+tr.Content)
		}
		return out
	}
	if got := turn("none", func(context.Context, chat.Result) string { return "" }); len(got) != 0 {
		t.Fatalf("an unsent reply is kept: %v", got)
	}
	if got := turn("part", func(context.Context, chat.Result) string { return "첫 부분." }); len(got) != 2 || got[1] != "assistant:첫 부분." {
		t.Fatalf("a reply cut short: %v", got)
	}
	if got := turn("plain", nil); len(got) != 2 || got[1] != "assistant:첫 부분. 둘째 부분." {
		t.Fatalf("without Deliver the reply is kept as generated: %v", got)
	}
}

// The reply is posted before the turn is recorded, so the turn id must already be known when it is
// posted: reply feedback is tied to it.
func TestChatTurnIDIsKnownWhenTheReplyIsPosted(t *testing.T) {
	var planCalls, respCalls atomic.Int32
	planURL := chatServer(t, `{"memory_requirement":"none","tool_plan":[]}`, "stop", &planCalls)
	respURL := chatServer(t, "안녕!", "stop", &respCalls)
	svc := routedSvc(t, planURL, respURL)
	var during string
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "안녕",
		Speaker: "철수", SpeakerID: "u1", Deliver: func(_ context.Context, r chat.Result) string {
			during = r.TurnID
			svc.NoteReplyMessages(r.TurnID, "g", "c", []string{"reply-1"})
			return r.Answer
		}})
	if err != nil {
		t.Fatal(err)
	}
	if during == "" || during != res.TurnID {
		t.Fatalf("turn id while posting %q, after %q", during, res.TurnID)
	}
	if got, ok := svc.ReplyTurn("reply-1"); !ok || got != res.TurnID {
		t.Fatalf("reply not tied to the turn: %q %v", got, ok)
	}
}
