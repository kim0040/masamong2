package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// Regression tests for the effort-control review (2026-11-03, round 2): one per finding, named after it.

// fixedBudgetHost answers like Anthropic for a model with a fixed thinking budget (Claude Haiku 4.5): type
// "adaptive" and the output_config effort are refused, "enabled" with a budget is taken.
func fixedBudgetHost() func(w http.ResponseWriter, body map[string]any) {
	return func(w http.ResponseWriter, body map[string]any) {
		switch thinkingType(body) {
		case "adaptive":
			anthropicError(w, 400, "invalid_request_error", "thinking.type: Input tag 'adaptive' found using 'type' does not match any of the expected tags: 'disabled', 'enabled'")
			return
		case "enabled":
			th := body["thinking"].(map[string]any)
			budget, _ := th["budget_tokens"].(float64)
			if budget < 1024 || body["max_tokens"].(float64) <= budget {
				anthropicError(w, 400, "invalid_request_error", "max_tokens must be greater than thinking.budget_tokens")
				return
			}
		}
		if body["output_config"] != nil {
			anthropicError(w, 400, "invalid_request_error", "This model does not support the effort parameter.")
			return
		}
		_, _ = w.Write([]byte(nativeText))
	}
}

// Finding 1: the shape learned from a model's refusals is shared by every adapter of the provider (the
// application builds one per turn), so the next turn's first call is right; and a model with unknown
// capabilities that refuses adaptive thinking thinks with a budget instead of not at all.
func TestLearnedShapesAreSharedAcrossAdapters(t *testing.T) {
	h := &anthropicHost{chat: fixedBudgetHost()}
	a := newAnthropicHost(t, "/v1", h)
	shared := NewCapsCache()
	a.Caps = shared
	req := ChatRequest{Model: "claude-haiku-4-5", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: "high"}
	if _, err := a.Chat(context.Background(), req); err != nil {
		t.Fatalf("first adapter: %v after %v", err, h.bodies)
	}
	last := h.bodies[len(h.bodies)-1]
	if thinkingType(last) != "enabled" || last["output_config"] != nil {
		t.Fatalf("a budget model thinks with a budget: %v", last)
	}

	// the next turn's adapter: one call, already right
	b := New(a.Client, a.BaseURL, a.Cred)
	b.API, b.Caps = APIAnthropic, shared
	h.bodies = nil
	if _, err := b.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(h.bodies) != 1 {
		t.Fatalf("the second adapter relearned: %d calls %v", len(h.bodies), h.bodies)
	}
	th, _ := h.bodies[0]["thinking"].(map[string]any)
	if th["type"] != "enabled" || th["budget_tokens"] != float64(4096) || h.bodies[0]["max_tokens"] != float64(40+4096) {
		t.Fatalf("enabled with the high budget: %v", h.bodies[0])
	}

	// a host that refuses the budget too: the model's default, after that refusal only
	h2 := &anthropicHost{chat: func(w http.ResponseWriter, body map[string]any) {
		switch thinkingType(body) {
		case "adaptive":
			anthropicError(w, 400, "invalid_request_error", "thinking.type: 'adaptive' is not supported")
		case "enabled":
			anthropicError(w, 400, "invalid_request_error", "thinking: this model does not think")
		default:
			_, _ = w.Write([]byte(nativeText))
		}
	}}
	c := newAnthropicHost(t, "/v1", h2)
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("%v after %v", err, h2.bodies)
	}
	if len(h2.bodies) != 3 || thinkingType(h2.bodies[1]) != "enabled" || h2.bodies[2]["thinking"] != nil {
		t.Fatalf("adaptive, then enabled, then no thinking field: %v", h2.bodies)
	}
}

// Finding 1: a refusal learned while a model's capabilities were unknown is dropped once a list makes
// them known, so a stale "no thinking" does not outlive the list that says how the model thinks.
func TestKnownCapsPutClearsStaleNoEffort(t *testing.T) {
	refuse := true
	h := &anthropicHost{}
	h.chat = func(w http.ResponseWriter, body map[string]any) {
		if refuse && body["thinking"] != nil {
			anthropicError(w, 400, "invalid_request_error", "thinking: Extra inputs are not permitted")
			return
		}
		_, _ = w.Write([]byte(nativeText))
	}
	a := newAnthropicHost(t, "/v1", h)
	const model = "claude-haiku-4-5-20251001"
	req := ChatRequest{Model: model, Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: "medium"}
	if _, err := a.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if h.bodies[len(h.bodies)-1]["thinking"] != nil {
		t.Fatalf("the refused field is left out: %v", h.bodies)
	}
	// the route takes thinking now, and the list says how this model thinks
	refuse = false
	models, err := ParseModels("application/json", []byte(`{"data":[`+capsHaiku45+`]}`))
	if err != nil {
		t.Fatal(err)
	}
	a.caps().Put(model, models[0].Caps())
	h.bodies = nil
	if _, err := a.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(h.bodies) != 1 || thinkingType(h.bodies[0]) != "enabled" {
		t.Fatalf("known caps decide after the list: %v", h.bodies)
	}
	if h.bodies[0]["output_config"] != nil {
		t.Fatalf("a model without effort levels gets no output_config: %v", h.bodies[0])
	}
}

// Finding 3: a model thinking with a fixed budget (Claude Haiku 4.5) refuses a tool loop's next round
// when the last assistant turn calls a tool without a thinking block. After a failover (the primary's
// thinking left out) or after another host's answer, that round goes out with thinking off; its own
// answer, sent back with its thinking, keeps thinking on.
func TestBudgetModelAfterBareToolTurn(t *testing.T) {
	const budget = "claude-haiku-4-5-20251001"
	models, err := ParseModels("application/json", []byte(`{"data":[`+capsHaiku45+`]}`))
	if err != nil {
		t.Fatal(err)
	}
	manualHost := func(answer string) (*anthropicHost, *Adapter) {
		h := &anthropicHost{manual: true, chat: func(w http.ResponseWriter, _ map[string]any) { _, _ = w.Write([]byte(answer)) }}
		a := newAnthropicHost(t, "/v1", h)
		a.caps().Put(budget, models[0].Caps())
		return h, a
	}
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "temperature_c", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	ask := []Message{{Role: "user", Content: "Oslo and Cairo?"}}
	round2 := func(assistant Message) ChatRequest {
		msgs := append(append([]Message(nil), ask...), assistant,
			Message{Role: "tool", ToolCallID: "toolu_1", Content: "4"}, Message{Role: "tool", ToolCallID: "toolu_2", Content: "27"})
		return ChatRequest{Model: budget, Messages: msgs, MaxTokens: 100, Effort: "high", Tools: tools}
	}

	// round 1 on another model, which is down for round 2: the budget model is the fallback
	first := &anthropicHost{chat: func(w http.ResponseWriter, _ map[string]any) { _, _ = w.Write([]byte(nativeToolCall)) }}
	a := newAnthropicHost(t, "/v1", first)
	res, err := a.Chat(context.Background(), ChatRequest{Model: "claude-a", Messages: ask, MaxTokens: 100, Effort: "high", Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	fbHost, fb := manualHost(nativeText)
	down := failingAnthropic(t)
	down.Failover = &Failover{Provider: "p", Model: "claude-a", Fallbacks: []Fallback{{A: fb, Provider: "f", Model: budget}}}
	req := round2(Message{Role: "assistant", Raw: res.Message, ToolCalls: res.ToolCalls})
	req.Model = "claude-a"
	if _, err := down.Chat(context.Background(), req); err != nil {
		t.Fatalf("failover to a budget model: %v (%v)", err, fbHost.bodies)
	}
	if len(fbHost.bodies) != 1 || thinkingType(fbHost.bodies[0]) != "disabled" {
		t.Fatalf("the fallback's round goes out with thinking off: %v", fbHost.bodies)
	}

	// the previous round came from a chat-completions host: no thinking block either
	ccHost, cc := manualHost(nativeText)
	foreign := Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "toolu_1", Type: "function", Function: ToolCallFunction{Name: "temperature_c", Arguments: `{"city":"Oslo"}`}},
		{ID: "toolu_2", Type: "function", Function: ToolCallFunction{Name: "temperature_c", Arguments: `{"city":"Cairo"}`}}}}
	if _, err := cc.Chat(context.Background(), round2(foreign)); err != nil || thinkingType(ccHost.bodies[0]) != "disabled" {
		t.Fatalf("after another host's answer: %v %v", err, ccHost.bodies)
	}

	// the budget model's own answer goes back with its thinking: thinking stays on
	ownHost, own := manualHost(strings.Replace(nativeToolCall, "claude-haiku-5-5", budget, 1))
	res, err = own.Chat(context.Background(), ChatRequest{Model: budget, Messages: ask, MaxTokens: 100, Effort: "high", Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := own.Chat(context.Background(), round2(Message{Role: "assistant", Raw: res.Message, ToolCalls: res.ToolCalls})); err != nil {
		t.Fatalf("own round trip: %v", err)
	}
	if thinkingType(ownHost.bodies[1]) != "enabled" || blockTypes(ownHost.bodies[1])[1] != "thinking" {
		t.Fatalf("own thinking kept, thinking on: %v", ownHost.bodies[1])
	}
}
