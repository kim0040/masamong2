package application

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
)

// The reference, name-call and follow-up classifiers ask without thinking; every other planner-route
// call keeps "low".
func TestQuickAnswerAsksWithoutThinking(t *testing.T) {
	s, err := Open(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if r := s.Execute(context.Background(), action.Request{Action: "setup.resume", Input: []byte(`{"instance":"quick"}`)}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	var mu sync.Mutex
	var efforts []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if r.URL.Path != "/chat/completions" { // config.apply also looks the models up
			http.NotFound(w, r)
			return
		}
		mu.Lock()
		efforts = append(efforts, body["reasoning_effort"])
		mu.Unlock()
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": `{"to":"bot"}`}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	t.Cleanup(srv.Close)
	a := openai.New(providers.NewClient(5*time.Second, nil), srv.URL, providers.Offline())
	llm := chatLLM{s: s, routes: map[string]chatRoute{"planner": {model: "m", provider: "p", a: a}}}
	msgs := []chat.Msg{{Role: "user", Content: "hi"}}
	if _, err := llm.ChatDetailed(withQuickAnswer(withLedgerRole(context.Background(), "name_call")), "planner", msgs, 60, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := llm.ChatDetailed(context.Background(), "planner", msgs, 400, 0); err != nil {
		t.Fatal(err)
	}
	// a configured effort replaces low on ordinary calls; a quick classifier still asks for none
	llm.routes["planner"] = chatRoute{model: "m", provider: "p", a: a, effort: "medium"}
	if _, err := llm.ChatDetailed(context.Background(), "planner", msgs, 400, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := llm.ChatDetailed(withQuickAnswer(context.Background()), "planner", msgs, 60, 0); err != nil {
		t.Fatal(err)
	}
	t.Setenv("QUICK_KEY", "k")
	doc := s.Doc()
	doc.Providers["fake"] = config.Provider{Kind: config.KindOpenAI, BaseURL: srv.URL, CredentialRef: "env:QUICK_KEY"}
	doc.Models["planner"] = config.Model{Provider: "fake", Model: "m"}
	doc.Models["response"] = config.Model{Provider: "fake", Model: "m"}
	raw, _ := config.Marshal(doc)
	if r := s.Execute(context.Background(), action.Request{Action: "config.apply", Input: mustJSON(map[string]any{"yaml": string(raw), "base_revision": s.Revision()})}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	voice := s.voiceFor("g", config.ChannelPolicy{})
	if got, _ := s.classifyFollow(context.Background(), ObservedMessage{GuildID: "g", ChannelID: "c", AuthorName: "Chris", Content: "응"}, "우산 챙겨", nil, voice); got != "answer" {
		t.Fatalf("follow decision: %s", got)
	}
	mu.Lock()
	defer mu.Unlock()
	if want := "[none low medium none none]"; fmt.Sprint(efforts) != want {
		t.Fatalf("efforts sent: %v", efforts)
	}
}
