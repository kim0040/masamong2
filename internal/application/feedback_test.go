package application_test

import (
	"context"
	"testing"

	"masamong/internal/application"
)

// Reactions people put on a reply and the "Rate this reply" menu end up on the turn that produced the
// reply; reactions that say nothing and messages that are not replies are ignored, and a withdrawn
// reaction is withdrawn.
func TestFeedbackOnRepliesReachesTheTurn(t *testing.T) {
	svc, _ := chatSvc(t, `{"memory_requirement":"none","question_kind":"other","tool_plan":[]}`)
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", Message: "안녕", Speaker: "영희", SpeakerID: "u1"})
	if err != nil || res.TurnID == "" {
		t.Fatalf("turn id: %+v %v", res, err)
	}
	svc.NoteReplyMessages(res.TurnID, "g", "c", []string{"reply-1", "reply-2"})
	if turn, ok := svc.ReplyTurn("reply-2"); !ok || turn != res.TurnID {
		t.Fatalf("reply-2 belongs to the turn: %q %v", turn, ok)
	}
	svc.ReactionFeedback("g", "reply-1", "u2", "👍", true)
	svc.ReactionFeedback("g", "reply-1", "u3", "❤️", true)
	svc.ReactionFeedback("g", "reply-1", "u3", "🍕", true)      // says nothing about the reply
	svc.ReactionFeedback("g", "someone-else", "u2", "👎", true) // not a reply
	svc.ReactionFeedback("g", "reply-1", "u3", "❤️", false)    // withdrawn
	if !svc.RateReply("g", "reply-2", "u4", false, "forgot") {
		t.Fatal("rating a reply")
	}
	if svc.RateReply("g", "not-a-reply", "u4", true, "") {
		t.Fatal("a message that is not a reply cannot be rated")
	}
	turn := out(t, svc, "turns.get", map[string]any{"id": res.TurnID})["turn"].(map[string]any)
	fb, _ := turn["feedback"].(map[string]any)
	if fb == nil || fb["good"].(float64) != 1 || fb["bad"].(float64) != 1 || fb["reasons"].([]any)[0] != "forgot" {
		t.Fatalf("feedback on the turn: %v", fb)
	}
	listed := out(t, svc, "turns.list", nil)["turns"].([]any)[0].(map[string]any)
	if listed["feedback"] == nil {
		t.Fatal("the turn list shows feedback")
	}
	sum := out(t, svc, "feedback.summary", nil)
	if sum["good"].(float64) != 1 || sum["bad"].(float64) != 1 || sum["reasons"].(map[string]any)["forgot"].(float64) != 1 {
		t.Fatalf("summary: %v", sum)
	}
}
