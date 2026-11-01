package application_test

import (
	"context"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/application"
)

// A channel the bot answers in without storing its messages still keeps them as conversation context:
// deleting or editing the message in Discord reaches that context too.
func TestUncapturedMessageEditsAndDeletionsReachChatContext(t *testing.T) {
	var calls atomic.Int32
	url := chatServer(t, `{"memory_requirement":"none","tool_plan":[]}`, "stop", &calls)
	svc := routedSvc(t, url, url)
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range []struct{ id, text string }{{"910", "내 주소는 비밀동 1번지"}, {"911", "오타 있는 말"}} {
		if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: m.id,
			Message: m.text, Speaker: "철수", SpeakerID: "u1"}); err != nil {
			t.Fatal(err)
		}
	}
	svc.MessagesDeleted("g", "c", []string{"910"})
	svc.MessageEdited("g", "c", "911", "고친 말", time.Now())
	turns, _ := eng.Store.RecentChatTurns("discord:g:c", 10)
	var lines []string
	for _, tr := range turns {
		if tr.Role == "user" {
			lines = append(lines, tr.Content)
		}
	}
	if strings.Contains(strings.Join(lines, "|"), "비밀동") || !slices.Contains(lines, "고친 말") || slices.Contains(lines, "오타 있는 말") {
		t.Fatalf("chat context: %q", lines)
	}
}

// Withdrawing memory consent clears the person's lines from conversation context and the text of their
// messages from the turn history and the participation log, and drops their reply ratings; other
// people's stay.
func TestWithdrawnMemoryConsentClearsContextAndRecords(t *testing.T) {
	svc, gw, _ := partSvc(t, "conservative")
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	// u1's question is answered by participation, u2 talks to the bot directly
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지? 내 번호는 010-비밀")
	partLog(t, svc, 1)
	if gw.postCount() != 1 {
		t.Fatalf("posts: %d", gw.postCount())
	}
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m2",
		Message: "영희 생일 언제야?", Speaker: "영희", SpeakerID: "u2"})
	if err != nil {
		t.Fatal(err)
	}
	svc.NoteReplyMessages(res.TurnID, "g", "c", []string{"r2"})
	svc.RateReply("g", "r2", "u1", false, "wrong")

	if r := as(svc, "privacy.consent", map[string]any{"scope": "memory", "decision": "withdraw"}, inChannel("u1")); !r.OK {
		t.Fatal(r.Error)
	}
	turns, _ := eng.Store.RecentChatTurns("discord:g:c", 10)
	all := ""
	for _, tr := range turns {
		all += tr.Role + ":" + tr.Content + "|"
	}
	if strings.Contains(all, "010-비밀") || !strings.Contains(all, "영희 생일") {
		t.Fatalf("chat context after withdrawal: %s", all)
	}
	list := asJSON(t, exec(svc, "turns.list", map[string]any{"guild_id": "g", "channel_id": "c"}).Output)
	if strings.Contains(list, "010-비밀") || !strings.Contains(list, "영희 생일") {
		t.Fatalf("turn history after withdrawal: %s", list)
	}
	if strings.Contains(list, `"bad":1`) {
		t.Fatalf("the withdrawn person's rating stays: %s", list)
	}
	plog := asJSON(t, exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"}).Output)
	if strings.Contains(plog, "010-비밀") {
		t.Fatalf("participation log after withdrawal: %s", plog)
	}
}

// Deleting a message in Discord also clears it from the participation log.
func TestDeletedMessageLeavesTheParticipationLog(t *testing.T) {
	svc, _, _ := partSvc(t, "conservative")
	say(svc, "m1", "u1", "누가 텐트 챙기기로 했지? 비밀 메모")
	partLog(t, svc, 1)
	svc.MessagesDeleted("g", "c", []string{"m1"})
	svc.DrainRedactions(context.Background())
	if plog := asJSON(t, exec(svc, "participation.mode", map[string]any{"guild_id": "g", "channel_id": "c"}).Output); strings.Contains(plog, "비밀 메모") {
		t.Fatalf("participation log keeps the deleted text: %s", plog)
	}
}
