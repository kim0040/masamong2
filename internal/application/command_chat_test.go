package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/errcode"
)

// A command whose tool has no answer ("!시세 AAPL" with no quote service) carries on as a conversation:
// the turn starts from the failed result, the planner is not offered that tool again, and the reply
// comes from the answer model, not a canned card. Answered commands, refusals, tools the chat does not
// use and channels that do not chat keep their card.
func TestCommandWithoutAnswerCarriesOnAsConversation(t *testing.T) {
	var plannerPrompt, answerPrompt string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, _ := json.Marshal(body["messages"])
		content := "애플 시세는 지금 못 보겠네, 대신 요즘 애플 얘기 해 볼까?"
		if strings.Contains(string(raw), "tool_plan") {
			plannerPrompt = string(raw)
			content = `{"memory_requirement":"none","question_kind":"other","tool_plan":[]}`
		} else {
			answerPrompt = string(raw)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": content}}}, "usage": map[string]any{"cost": 0.001}})
	}))
	defer srv.Close()
	svc := setupSvc(t)
	t.Setenv("FAKE_OR_KEY", "k")
	applyDoc(t, svc, func(d *config.Document) {
		d.Chat.ToolMode = "planner" // the planner pipeline; the tool loop has its own tests
		d.Providers["openrouter"] = config.Provider{Kind: config.KindOpenAI, BaseURL: srv.URL, CredentialRef: "env:FAKE_OR_KEY"}
		d.Models["planner"] = config.Model{Provider: "openrouter", Model: "fake/planner"}
		d.Models["response"] = config.Model{Provider: "openrouter", Model: "fake/response"}
		d.Memory.Retrieve = "off"
	})
	quote := exec(svc, "finance.quote", map[string]any{"symbol": "AAPL"})
	if !application.Unanswered(quote) {
		t.Fatalf("an unconfigured quote is no answer: %+v", quote.Output)
	}
	in := application.ChatInput{GuildID: "g", ChannelID: "c", Message: "!시세 AAPL", Speaker: "영희", SpeakerID: "u1", DryRun: true}
	res, ok := svc.CommandToChat(context.Background(), quote, in)
	if !ok || res.Answer == "" || len(res.Tools) == 0 || res.Tools[0].Tool != "finance.quote" || res.Tools[0].Data["status"] != "unconfigured" {
		t.Fatalf("carried on: ok=%v answer=%q tools=%+v", ok, res.Answer, res.Tools)
	}
	if strings.Contains(plannerPrompt, "- finance.quote:") || !strings.Contains(plannerPrompt, "- search.web:") {
		t.Fatal("the failed tool must leave the planner's menu while the others stay")
	}
	if !strings.Contains(answerPrompt, "finance.quote") || !strings.Contains(answerPrompt, `\"status\":\"unconfigured\"`) || strings.Contains(answerPrompt, `\"status\":\"unavailable\"`) {
		t.Fatal("the answer should identify that the tool is not set up without exposing operator-only details")
	}
	// nor the note for the operator ("add the key under Settings")
	if note, _ := quote.Output["message"].(string); note == "" || strings.Contains(answerPrompt, note) {
		t.Fatalf("operator note reached the answer model: %q", note)
	}

	keep := map[string]action.Result{
		"answered": {OK: true, Action: "finance.quote", Output: map[string]any{"status": "ok", "price": 1.0}},
		"refused":  {OK: false, Action: "finance.quote", Error: &action.Error{Code: errcode.PermissionDenied}},
		"not chat": {OK: false, Action: "config.show", Error: &action.Error{Code: errcode.IO}},
	}
	for name, r := range keep {
		if _, ok := svc.CommandToChat(context.Background(), r, in); ok {
			t.Fatalf("%s: the card stays", name)
		}
	}
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "quiet", "respond": "off"}); !r.OK {
		t.Fatalf("configure: %+v", r.Error)
	}
	quiet := in
	quiet.ChannelID = "quiet"
	if _, ok := svc.CommandToChat(context.Background(), quote, quiet); ok {
		t.Fatal("a channel that does not chat keeps the card")
	}
}
