package openai

import (
	"context"
	"net/http"
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
