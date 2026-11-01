package openai

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"masamong/internal/errclass"
)

// A ByokOnly family goes to its one upstream with no fallback, keeping tool routing; other families,
// and hosts other than OpenRouter, are sent as before.
func TestByokOnlyPinsTheFamilyToItsUpstream(t *testing.T) {
	ok := `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`
	srv := &rawServer{replies: []reply{{200, ok}, {200, ok}, {200, twoCallsReply}}}
	a := testAdapter(t, APIOpenRouter, srv.handler)
	a.ByokOnly = map[string]string{"anthropic": "anthropic"}
	msgs := []Message{{Role: "user", Content: "x"}}
	for _, m := range []string{"anthropic/claude-sonnet-5.5", "deepseek/deepseek-v4.1-flash"} {
		if _, err := a.Chat(context.Background(), ChatRequest{Model: m, Messages: msgs}); err != nil {
			t.Fatal(err)
		}
	}
	if got := srv.json(t, 0)["provider"]; !reflect.DeepEqual(got, map[string]any{"only": []any{"anthropic"}, "allow_fallbacks": false}) {
		t.Fatalf("claude: %v", got)
	}
	if got, sent := srv.json(t, 1)["provider"]; sent {
		t.Fatalf("another family is not pinned: %v", got)
	}
	if _, err := a.Chat(context.Background(), ChatRequest{Model: "anthropic/claude-opus-5.5", Messages: msgs, Tools: []Tool{weatherTool}}); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"only": []any{"anthropic"}, "allow_fallbacks": false, "require_parameters": true}
	if got := srv.json(t, 2)["provider"]; !reflect.DeepEqual(got, want) {
		t.Fatalf("with tools: %v", got)
	}

	other := &rawServer{replies: []reply{{200, ok}}}
	b := testAdapter(t, APICompatible, other.handler)
	b.ByokOnly = a.ByokOnly
	if _, err := b.Chat(context.Background(), ChatRequest{Model: "anthropic/claude-sonnet-5.5", Messages: msgs}); err != nil {
		t.Fatal(err)
	}
	if got, sent := other.json(t, 0)["provider"]; sent {
		t.Fatalf("only OpenRouter reads provider routing: %v", got)
	}
}

// A call OpenRouter ran on your own key costs its fee plus what the upstream provider billed; on
// OpenRouter credits usage.cost already is the whole price.
func TestUsageCostCountsTheUpstreamBillOfAByokCall(t *testing.T) {
	for raw, want := range map[string]float64{
		`{"cost":0.0001,"is_byok":true,"cost_details":{"upstream_inference_cost":0.002}}`: 0.0021,
		`{"cost":0.002,"is_byok":false,"cost_details":{"upstream_inference_cost":0.002}}`: 0.002,
		`{"cost":0.002}`: 0.002,
	} {
		var usage map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &usage); err != nil {
			t.Fatal(err)
		}
		if got := usageCost(usage); got == nil || *got < want-1e-12 || *got > want+1e-12 {
			t.Errorf("%s: %v, want %v", raw, got, want)
		}
	}
}

// Anthropic refusing the BYOK key is not OpenRouter refusing ours: the provider stays trusted (not
// ErrAuth) and the model rests while its fallback answers.
func TestARefusedByokKeyIsNotARejectedOpenRouterKey(t *testing.T) {
	refused := `{"error":{"message":"Provider returned error","code":401,"metadata":{"raw":"{\"type\":\"error\",\"error\":{\"type\":\"authentication_error\",\"message\":\"invalid x-api-key\"}}","provider_name":"Anthropic","is_byok":true}}}`
	srv := &rawServer{replies: []reply{{401, refused}, {401, `{"error":{"message":"No auth credentials found","code":401}}`}}}
	a := testAdapter(t, APIOpenRouter, srv.handler)
	a.ByokOnly = map[string]string{"anthropic": "anthropic"}
	_, err := a.Chat(context.Background(), ChatRequest{Model: "anthropic/claude-sonnet-5.5", Messages: []Message{{Role: "user", Content: "x"}}})
	if err == nil || errors.Is(err, ErrAuth) || errclass.Of(err) != errclass.Permission || !strings.Contains(err.Error(), "BYOK") {
		t.Fatalf("byok refusal: %v (%v)", err, errclass.Of(err))
	}
	_, err = a.Chat(context.Background(), ChatRequest{Model: "deepseek/deepseek-v4.1-flash", Messages: []Message{{Role: "user", Content: "x"}}})
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("our own key refused is still ErrAuth: %v", err)
	}
}
