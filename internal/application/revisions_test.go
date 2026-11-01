package application_test

import (
	"context"
	"encoding/json"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/lang"
	"masamong/internal/memory/engine"
)

// A Discord message the bot answered and then its author deleted: the turn history keeps no text of it,
// the chat context forgets it and the stored message becomes a tombstone.
func TestDeletedDiscordMessageIsForgotten(t *testing.T) {
	var calls atomic.Int32
	url := chatServer(t, `{"memory_requirement":"none","tool_plan":[]}`, "stop", &calls)
	svc := routedSvc(t, url, url)
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "900", AuthorID: "u1", AuthorName: "철수",
		Content: "내 주소는 비밀동 1번지", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "900",
		Message: "내 주소는 비밀동 1번지", Speaker: "철수", SpeakerID: "u1"}); err != nil {
		t.Fatal(err)
	}
	turns := exec(svc, "turns.list", map[string]any{"guild_id": "g", "channel_id": "c"})
	if !strings.Contains(asJSON(t, turns.Output), "비밀동") {
		t.Fatalf("the turn must record the message before the deletion: %s", asJSON(t, turns.Output))
	}

	svc.MessagesDeleted("g", "c", []string{"900"})
	svc.DrainRedactions(context.Background()) // the copies in the turn history go in a durable background job

	if out := asJSON(t, exec(svc, "turns.list", map[string]any{"guild_id": "g", "channel_id": "c"}).Output); strings.Contains(out, "비밀동") {
		t.Fatalf("turn history keeps the deleted text: %s", out)
	}
	ctx, _ := eng.Store.RecentChatTurns("discord:g:c", 10)
	for _, tr := range ctx {
		if strings.Contains(tr.Content, "비밀동") {
			t.Fatalf("chat context keeps the deleted text: %+v", ctx)
		}
	}
	if msgs, _ := eng.Store.RecentMessages(engine.RoomKey("g", "c"), 10); len(msgs) != 0 {
		t.Fatalf("stored message not removed: %+v", msgs)
	}
	// an edit of another captured message replaces its text
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "901", AuthorID: "u1", AuthorName: "철수",
		Content: "오타 있는 말", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	svc.MessageEdited("g", "c", "901", "고친 말", time.Now())
	if msgs, _ := eng.Store.RecentMessages(engine.RoomKey("g", "c"), 10); len(msgs) != 1 || msgs[0].Content != "고친 말" {
		t.Fatalf("edit not applied: %+v", msgs)
	}
}

func TestPromptRecitationGuardRunsBeforeDeliveryAndHistory(t *testing.T) {
	var calls atomic.Int32
	pack, _ := lang.Get("en")
	leaked := pack.Chat.PlannerHeader
	url := chatServer(t, `{"memory_requirement":"none","tool_plan":[]}`, leaked, &calls)
	svc := routedSvc(t, url, url)
	var delivered string
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", Message: "hello", Speaker: "Bob", SpeakerID: "bob", Deliver: func(_ context.Context, r chat.Result) string { delivered = r.Answer; return r.Answer }})
	if err != nil || delivered == "" || strings.Contains(delivered, leaked) || res.Answer != delivered {
		t.Fatalf("delivery: %q %v", delivered, err)
	}
	var body string
	if err := svc.Store.DB.QueryRow(`SELECT body FROM turn_log LIMIT 1`).Scan(&body); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, leaked) {
		t.Fatalf("history retained blocked reply: %s", body)
	}
	eng, _ := svc.MemoryEngine()
	turns, _ := eng.Store.RecentChatTurns("discord:g:c", 10)
	for _, turn := range turns {
		if strings.Contains(turn.Content, leaked) {
			t.Fatal("chat context retained blocked reply")
		}
	}
}

func asJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}
