package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"masamong/internal/application"
)

func TestToolFailureAnswerIsNotReplayedIntoLaterTurn(t *testing.T) {
	var plannerCalls atomic.Int32
	plannerURL := chatServer(t, `{"memory_requirement":"none","tool_plan":[{"tool_name":"weather","parameters":{"region":"서울","source":"global"}}]}`, "stop", &plannerCalls)
	const failedAnswer = "I couldn't fetch the weather earlier; here is another idea."
	var mu sync.Mutex
	var answerPrompts []string
	response := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		var req struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode completion: %v", err)
			return
		}
		var joined strings.Builder
		for _, msg := range req.Messages {
			joined.WriteString(msg.Content)
			joined.WriteByte('\n')
		}
		mu.Lock()
		answerPrompts = append(answerPrompts, joined.String())
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{
			"message": map[string]any{"role": "assistant", "content": failedAnswer}, "finish_reason": "stop",
		}}, "usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.00001}})
	}))
	t.Cleanup(response.Close)
	svc := routedSvc(t, plannerURL, response.URL)
	first, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", Message: "What is the weather in Atlantis?", Speaker: "Ann", SpeakerID: "u1"})
	if err != nil || len(first.Tools) == 0 || first.Tools[0].Status != "unconfigured" {
		t.Fatalf("expected an unconfigured tool result: %+v err=%v", first.Tools, err)
	}
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	turns, err := eng.Store.RecentChatTurns("discord:g:c", 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, turn := range turns {
		if turn.Role == "assistant" && turn.Content == failedAnswer {
			t.Fatal("failed tool answer was stored for replay")
		}
	}
	if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", Message: "What is 2 + 2?", Speaker: "Ann", SpeakerID: "u1"}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(answerPrompts) != 2 || strings.Contains(answerPrompts[1], failedAnswer) {
		t.Fatalf("unrelated follow-up was given the earlier failure: %v", answerPrompts)
	}
}
