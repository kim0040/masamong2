package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/tools"
)

// chatServer answers every chat completion with content and finish reason.
func chatServer(t *testing.T, content, finish string, calls *atomic.Int32) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r) // the price list lookup: no prices
			return
		}
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": finish}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5, "cost": 0.00001},
		})
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func routedSvc(t *testing.T, planURL, respURL string) *application.Service {
	svc := setupSvc(t)
	t.Setenv("FAKE_OR_KEY", "k")
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["plan-host"] = config.Provider{Kind: config.KindOpenAI, BaseURL: planURL, CredentialRef: "env:FAKE_OR_KEY"}
		d.Providers["resp-host"] = config.Provider{Kind: config.KindOpenAI, BaseURL: respURL, CredentialRef: "env:FAKE_OR_KEY"}
		d.Providers["openmeteo"] = config.Provider{Kind: config.KindOpenMeteo, Disabled: true}
		// the same model name on two hosts
		d.Models["planner"] = config.Model{Provider: "plan-host", Model: "shared/model"}
		d.Models["response"] = config.Model{Provider: "resp-host", Model: "shared/model"}
		d.Memory.Retrieve = "off"
		d.Tools = map[string]tools.Settings{"search.web": {Chat: boolp(false)}}
		// these tests are about the planner pipeline; the tool loop (the default) has its own tests
		d.Chat.ToolMode = "planner"
	})
	return svc
}

func TestChatRolesSharingAModelNameKeepTheirProviders(t *testing.T) {
	var planCalls, respCalls atomic.Int32
	planURL := chatServer(t, `{"memory_requirement":"none","tool_plan":[]}`, "stop", &planCalls)
	respURL := chatServer(t, "응답 호스트", "stop", &respCalls)
	svc := routedSvc(t, planURL, respURL)
	for i := 0; i < 4; i++ { // map order used to decide which host served both roles
		res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", Message: "안녕", Speaker: "u", SpeakerID: "u1", DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		if res.Answer != "응답 호스트" {
			t.Fatalf("turn %d answered by the wrong host: %q", i, res.Answer)
		}
	}
	if planCalls.Load() != 4 || respCalls.Load() != 4 {
		t.Fatalf("planner host %d calls, response host %d calls", planCalls.Load(), respCalls.Load())
	}
}

// A reasoning model that spends the whole output budget thinking returns no text, also when asked again
// without thinking and with room past the thinking (the adapter's two retries): the other chat model answers instead, and when that one comes back empty too the turn fails saying why, never with an empty
// reply.
func TestEmptyReplyCutByTokenBudgetIsAnError(t *testing.T) {
	var planCalls, respCalls atomic.Int32
	planURL := chatServer(t, `{"memory_requirement":"none","tool_plan":[]}`, "stop", &planCalls)
	respURL := chatServer(t, "", "length", &respCalls)
	svc := routedSvc(t, planURL, respURL)
	in := application.ChatInput{GuildID: "g", ChannelID: "c", Message: "안녕", Speaker: "u", SpeakerID: "u1", DryRun: true}
	res, err := svc.ChatTurn(context.Background(), in)
	if err != nil || strings.TrimSpace(res.Answer) == "" || respCalls.Load() != 3 || planCalls.Load() != 2 {
		t.Fatalf("the planner's model answers when the response model runs out: err=%v answer=%q plan=%d resp=%d", err, res.Answer, planCalls.Load(), respCalls.Load())
	}
	var emptyCalls atomic.Int32
	emptyURL := chatServer(t, "", "length", &emptyCalls)
	svc = routedSvc(t, emptyURL, respURL)
	if _, err := svc.ChatTurn(context.Background(), in); err == nil || !strings.Contains(err.Error(), "finish_reason=length") {
		t.Fatalf("both models out of budget: err = %v", err)
	}
}

// When the answer prompt is longer than both chat models take, the turn asks the answer once more with
// less context instead of failing, and the turn record says why.
func TestContextLengthErrorRetriesTheAnswerWithLessContext(t *testing.T) {
	var planCalls, respCalls atomic.Int32
	tooLong := func(w http.ResponseWriter) {
		w.WriteHeader(400)
		_, _ = w.Write([]byte(`{"error":{"message":"This model's maximum context length is 8192 tokens","type":"invalid_request_error","code":"context_length_exceeded"}}`))
	}
	answer := func(w http.ResponseWriter, content string) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}
	plan := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		planCalls.Add(1)
		var body strings.Builder
		_, _ = io.Copy(&body, r.Body)
		if strings.Contains(body.String(), "tool_plan") {
			answer(w, `{"memory_requirement":"none","tool_plan":[]}`)
			return
		}
		tooLong(w) // standing in for the answer model, it is too small as well
	}))
	t.Cleanup(plan.Close)
	resp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		if respCalls.Add(1) == 1 {
			tooLong(w)
			return
		}
		answer(w, "짧게 답할게")
	}))
	t.Cleanup(resp.Close)
	svc := routedSvc(t, plan.URL, resp.URL)
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "안녕", Speaker: "u", SpeakerID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if res.Answer != "짧게 답할게" || res.AnswerRetry != "context_length" || respCalls.Load() != 2 || planCalls.Load() != 2 {
		t.Fatalf("answer %q retry %q resp calls %d plan calls %d", res.Answer, res.AnswerRetry, respCalls.Load(), planCalls.Load())
	}
	turn, ledger := turnLedger(t, svc, res.TurnID, 4, 0)
	if body, _ := turn["body"].(map[string]any); body["answer_retry"] != "context_length" {
		t.Fatalf("turn body: %+v", turn["body"])
	}
	var classes []string
	for _, r := range rows(ledger["llm_calls"]) {
		if r["role"] == "response" {
			classes = append(classes, r["class"].(string))
		}
	}
	if strings.Join(classes, ",") != "context_length,context_length," {
		t.Fatalf("response calls in the ledger: %v", classes)
	}
}

// A role's fallbacks answer before the other chat role is asked: the response host is down, its
// fallback on another host answers, and the planner host is not asked to stand in.
func TestResponseFallbackAnswersWhenItsHostIsDown(t *testing.T) {
	var planCalls, downCalls, fbCalls atomic.Int32
	planURL := chatServer(t, `{"memory_requirement":"none","tool_plan":[]}`, "stop", &planCalls)
	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		downCalls.Add(1)
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
	}))
	t.Cleanup(down.Close)
	fbURL := chatServer(t, "예비 호스트", "stop", &fbCalls)
	svc := routedSvc(t, planURL, down.URL)
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["fb-host"] = config.Provider{Kind: config.KindOpenAI, BaseURL: fbURL, CredentialRef: "env:FAKE_OR_KEY"}
		m := d.Models["response"]
		m.Fallbacks = []config.ModelFallback{{Provider: "fb-host", Model: "fb/model", PriceInPerMTok: 1, PriceOutPerMTok: 2}}
		d.Models["response"] = m
	})
	in := application.ChatInput{GuildID: "g", ChannelID: "c", Message: "안녕", Speaker: "u", SpeakerID: "u1", DryRun: true}
	res, err := svc.ChatTurn(context.Background(), in)
	if err != nil || res.Answer != "예비 호스트" {
		t.Fatalf("fallback answer: %v %q", err, res.Answer)
	}
	if fbCalls.Load() != 1 || planCalls.Load() != 1 || downCalls.Load() < 1 {
		t.Fatalf("calls: plan %d down %d fallback %d", planCalls.Load(), downCalls.Load(), fbCalls.Load())
	}
	// the provider list shows nothing resting yet (one 503 is not a run of failures)
	r := svc.Execute(context.Background(), action.Request{Action: "providers.list"})
	if rest, _ := r.Output["resting_models"].([]map[string]any); len(rest) != 0 {
		t.Fatalf("resting %v", rest)
	}
	// a fallback on a removed provider goes with it
	if r := svc.Execute(context.Background(), action.Request{Action: "providers.remove", Input: []byte(`{"id":"fb-host"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	if fb := svc.Doc().Models["response"].Fallbacks; len(fb) != 0 {
		t.Fatalf("fallbacks after remove: %+v", fb)
	}
}

// A model provider's daily call limit counts its calls; when it is reached the role's fallback answers.
func TestModelProviderDailyLimitHandsOverToTheFallback(t *testing.T) {
	var planCalls, respCalls, fbCalls atomic.Int32
	planURL := chatServer(t, `{"memory_requirement":"none","tool_plan":[]}`, "stop", &planCalls)
	respURL := chatServer(t, "기본 호스트", "stop", &respCalls)
	fbURL := chatServer(t, "예비 호스트", "stop", &fbCalls)
	svc := routedSvc(t, planURL, respURL)
	applyDoc(t, svc, func(d *config.Document) {
		p := d.Providers["resp-host"]
		p.DailyLimit = 1
		d.Providers["resp-host"] = p
		d.Providers["fb-host"] = config.Provider{Kind: config.KindOpenAI, BaseURL: fbURL, CredentialRef: "env:FAKE_OR_KEY"}
		m := d.Models["response"]
		m.Fallbacks = []config.ModelFallback{{Provider: "fb-host", Model: "fb/model"}}
		d.Models["response"] = m
	})
	in := application.ChatInput{GuildID: "g", ChannelID: "c", Message: "안녕", Speaker: "u", SpeakerID: "u1", DryRun: true}
	for i, want := range []string{"기본 호스트", "예비 호스트"} {
		res, err := svc.ChatTurn(context.Background(), in)
		if err != nil || res.Answer != want {
			t.Fatalf("turn %d: %v %q, want %q", i, err, res.Answer, want)
		}
	}
	if respCalls.Load() != 1 || fbCalls.Load() != 1 {
		t.Fatalf("limit 1: primary %d fallback %d", respCalls.Load(), fbCalls.Load())
	}
	r := svc.Execute(context.Background(), action.Request{Action: "providers.list"})
	raw, _ := json.Marshal(r.Output["providers"])
	var list []map[string]any
	_ = json.Unmarshal(raw, &list)
	found := false
	for _, p := range list {
		if p["id"] == "resp-host" {
			found = fmt.Sprint(p["used_today"]) == "1"
		}
	}
	if !found {
		t.Fatalf("resp-host used today: %s", raw)
	}
}
