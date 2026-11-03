package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for the effort-control review (2026-11-03, round 3): one or more per finding, named
// after it.

// refuseThinking answers like a host that takes no thinking field ("Extra inputs are not permitted").
func refuseThinking(w http.ResponseWriter, body map[string]any) {
	if body["thinking"] != nil {
		anthropicError(w, 400, "invalid_request_error", "thinking: Extra inputs are not permitted")
		return
	}
	_, _ = w.Write([]byte(nativeText))
}

// P2-1: a learned shape is used for an hour, then learned again from the next refusal.
func TestLearnedShapesExpire(t *testing.T) {
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	now = func() time.Time { return at }
	t.Cleanup(func() { now = time.Now })
	h := &anthropicHost{chat: refuseThinking}
	a := newAnthropicHost(t, "/v1", h)
	req := ChatRequest{Model: "claude-x", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: "low"}
	calls := func() int {
		h.bodies = nil
		if _, err := a.Chat(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		return len(h.bodies)
	}
	if n := calls(); n != 2 {
		t.Fatalf("refused, then sent without thinking: %d calls", n)
	}
	at = at.Add(59 * time.Minute)
	if n := calls(); n != 1 {
		t.Fatalf("within the hour the shape holds: %d calls", n)
	}
	at = at.Add(2 * time.Minute)
	if n := calls(); n != 2 || h.bodies[0]["thinking"] == nil {
		t.Fatalf("after an hour thinking is tried again: %v", h.bodies)
	}
}

// P2-2: each refusal learned is kept at once, so a chain longer than one call's retries is finished by
// the next call rather than replayed (a compatible host allows two retries).
func TestRefusalProgressIsKept(t *testing.T) {
	var rec recorded
	a := testAdapter(t, APICompatible, func(w http.ResponseWriter, r *http.Request) {
		body := rec.add(r)
		refuse := func(msg string) {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"` + msg + `","type":"invalid_request_error"}}`))
		}
		switch {
		case body["temperature"] != nil:
			refuse("temperature is not supported for this model")
		case body["reasoning_effort"] != nil:
			refuse("reasoning_effort is not supported for this model")
		case body["max_tokens"] != nil:
			refuse("max_tokens is not supported, use max_completion_tokens")
		default:
			_, _ = w.Write([]byte(geminiChatOK))
		}
	})
	temp := 0.2
	req := ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: "low", Temperature: &temp}
	if _, err := a.Chat(context.Background(), req); err == nil {
		t.Fatalf("three refusals with two retries end the first call: %v", rec.bodies)
	}
	rec.bodies = nil
	if _, err := a.Chat(context.Background(), req); err != nil {
		t.Fatalf("the next call goes on from the refusals learned: %v (%v)", err, rec.bodies)
	}
	if len(rec.bodies) != 1 || rec.bodies[0]["max_completion_tokens"] != float64(40) {
		t.Fatalf("one call with every refused field left out: %v", rec.bodies)
	}
}

// P2-2: the longest Anthropic chain, at xhigh and max on a model that does not think and whose
// capabilities are unknown, ends with a good request: a level above high, the effort field, adaptive and
// budgeted thinking may each be refused. A refusal of the effort field that names no level is the field,
// not the level.
func TestLongestAnthropicChain(t *testing.T) {
	const noField = "This model does not support the effort parameter."
	hosts := map[string]func(w http.ResponseWriter, body map[string]any){
		// a gateway that checks the level first, then a model without the field
		"level named": func(w http.ResponseWriter, body map[string]any) {
			oc, _ := body["output_config"].(map[string]any)
			switch {
			case oc != nil && oc["effort"] != "high":
				anthropicError(w, 400, "invalid_request_error", "output_config.effort: Input should be 'low', 'medium' or 'high'")
			case oc != nil:
				anthropicError(w, 400, "invalid_request_error", noField)
			case thinkingType(body) == "adaptive":
				anthropicError(w, 400, "invalid_request_error", "thinking.type: Input tag 'adaptive' found using 'type' does not match any of the expected tags: 'disabled', 'enabled'")
			case thinkingType(body) == "enabled":
				anthropicError(w, 400, "invalid_request_error", "thinking: this model does not support thinking")
			default:
				_, _ = w.Write([]byte(nativeText))
			}
		},
		// the model refuses the field at any level, naming none
		"no level": func(w http.ResponseWriter, body map[string]any) {
			switch {
			case body["output_config"] != nil:
				anthropicError(w, 400, "invalid_request_error", noField)
			case thinkingType(body) == "adaptive":
				anthropicError(w, 400, "invalid_request_error", "thinking.type: Input tag 'adaptive' found using 'type' does not match any of the expected tags: 'disabled', 'enabled'")
			case thinkingType(body) == "enabled":
				anthropicError(w, 400, "invalid_request_error", "thinking: this model does not support thinking")
			default:
				_, _ = w.Write([]byte(nativeText))
			}
		},
	}
	for name, host := range hosts {
		for _, effort := range []string{"xhigh", "max"} {
			t.Run(name+"/"+effort, func(t *testing.T) {
				h := &anthropicHost{chat: host}
				a := newAnthropicHost(t, "/v1", h)
				req := ChatRequest{Model: "claude-plain", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: effort}
				_, err := a.Chat(context.Background(), req)
				if err != nil {
					h.bodies = nil
					if _, err := a.Chat(context.Background(), req); err != nil {
						t.Fatalf("the second call finishes the chain: %v (%v)", err, h.bodies)
					}
				}
				last := h.bodies[len(h.bodies)-1]
				if last["thinking"] != nil || last["output_config"] != nil {
					t.Fatalf("neither field in the request that worked: %v", last)
				}
				h.bodies = nil
				if _, err := a.Chat(context.Background(), req); err != nil || len(h.bodies) != 1 {
					t.Fatalf("the shape is kept: %v, %d calls", err, len(h.bodies))
				}
				if name == "no level" && a.shapeOf(req).MaxHigh {
					t.Fatalf("a refusal naming no level is not read as one: %+v", a.shapeOf(req))
				}
			})
		}
	}
}

// P3-3: a call that learned nothing does not write its stale shape over a lesson another call stored
// while it ran. The host holds call A until call B has learned that the model takes at most high.
func TestStaleShapeDoesNotEraseAnotherCallsLesson(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	h := &anthropicHost{chat: func(w http.ResponseWriter, body map[string]any) {
		oc, _ := body["output_config"].(map[string]any)
		if body["max_tokens"] == float64(41+thinkingRoom("high")) { // call A
			close(arrived)
			<-release
		}
		if oc["effort"] == "xhigh" {
			anthropicError(w, 400, "invalid_request_error", "output_config.effort: Input should be 'low', 'medium' or 'high'")
			return
		}
		_, _ = w.Write([]byte(nativeText))
	}}
	a := newAnthropicHost(t, "/v1", h)
	shared := NewCapsCache()
	a.Caps = shared
	b := New(a.Client, a.BaseURL, a.Cred)
	b.API, b.Caps = APIAnthropic, shared
	reqA := ChatRequest{Model: "claude-x", Messages: []Message{{Role: "user", Content: "a"}}, MaxTokens: 41, Effort: "high"}
	reqB := ChatRequest{Model: "claude-x", Messages: []Message{{Role: "user", Content: "b"}}, MaxTokens: 40, Effort: "xhigh"}
	done := make(chan error, 1)
	go func() { _, err := a.Chat(context.Background(), reqA); done <- err }()
	<-arrived
	if _, err := b.Chat(context.Background(), reqB); err != nil {
		t.Fatal(err)
	}
	if !b.shapeOf(reqB).MaxHigh {
		t.Fatal("call B learned that the model takes at most high")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !a.shapeOf(reqB).MaxHigh {
		t.Fatalf("call A erased call B's lesson: %+v", a.shapeOf(reqB))
	}
}

// P3-3: a refusal read by a call that began before a list made the model's capabilities known is not
// kept: the list dropped the shapes, and the lesson was read against what was unknown then.
func TestPrePutLessonDoesNotReturn(t *testing.T) {
	arrived, release := make(chan struct{}), make(chan struct{})
	var seen atomic.Bool
	h := &anthropicHost{chat: func(w http.ResponseWriter, body map[string]any) {
		if !seen.Swap(true) {
			close(arrived)
			<-release
		}
		refuseThinking(w, body)
	}}
	a := newAnthropicHost(t, "/v1", h)
	const model = "claude-haiku-4-5-20251001"
	req := ChatRequest{Model: model, Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: "medium"}
	done := make(chan error, 1)
	go func() { _, err := a.Chat(context.Background(), req); done <- err }()
	<-arrived
	models, err := ParseModels("application/json", []byte(`{"data":[`+capsHaiku45+`]}`))
	if err != nil {
		t.Fatal(err)
	}
	a.caps().Put(model, models[0].Caps())
	close(release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if a.shapeOf(req).NoEffort {
		t.Fatalf("a lesson from before the list came back after it: %+v", a.shapeOf(req))
	}
}

// bareRound is a tool loop's next round after an answer from a chat-completions host: the last assistant
// turn calls a tool and carries no thinking block.
func bareRound(model, effort string) ChatRequest {
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "temperature_c", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	msgs := []Message{{Role: "user", Content: "Oslo?"},
		{Role: "assistant", ToolCalls: []ToolCall{{ID: "toolu_1", Type: "function", Function: ToolCallFunction{Name: "temperature_c", Arguments: `{"city":"Oslo"}`}}}},
		{Role: "tool", ToolCallID: "toolu_1", Content: "4"}}
	return ChatRequest{Model: model, Messages: msgs, MaxTokens: 100, Effort: effort, Tools: tools}
}

// P3-5: a bare tool turn sends a budget as thinking off, so the Budgeted it learns from a refused adaptive
// is not kept: no request with a budget has worked yet.
func TestBareToolTurnDoesNotKeepBudget(t *testing.T) {
	h := &anthropicHost{chat: fixedBudgetHost()}
	a := newAnthropicHost(t, "/v1", h)
	req := bareRound("claude-haiku-4-5", "high")
	if _, err := a.Chat(context.Background(), req); err != nil {
		t.Fatalf("%v after %v", err, h.bodies)
	}
	if len(h.bodies) < 2 || thinkingType(h.bodies[0]) != "adaptive" || thinkingType(h.bodies[1]) != "disabled" {
		t.Fatalf("adaptive refused, then thinking off for the bare turn: %v", h.bodies)
	}
	if a.shapeOf(req).Budgeted {
		t.Fatalf("Budgeted kept from a request that sent thinking off: %+v", a.shapeOf(req))
	}
}

// P3-6: a model that thinks adaptively keeps adaptive thinking after a bare tool turn (only a budget
// needs the turn's thinking block).
func TestAdaptiveStaysAfterBareToolTurn(t *testing.T) {
	h := &anthropicHost{manual: true}
	a := newAnthropicHost(t, "/v1", h)
	models, err := ParseModels("application/json", []byte(`{"data":[`+capsHaiku55+`]}`))
	if err != nil {
		t.Fatal(err)
	}
	a.caps().Put("claude-haiku-5-5", models[0].Caps())
	if _, err := a.Chat(context.Background(), bareRound("claude-haiku-5-5", "high")); err != nil {
		t.Fatal(err)
	}
	if len(h.bodies) != 1 || thinkingType(h.bodies[0]) != "adaptive" {
		t.Fatalf("adaptive after a bare tool turn: %v", h.bodies)
	}
}

// P3-6: a list read again with the same capabilities keeps the learned shapes (the route's refusals still
// hold); different capabilities drop them.
func TestPutKeepsShapesOnlyForTheSameCaps(t *testing.T) {
	h := &anthropicHost{chat: func(w http.ResponseWriter, body map[string]any) {
		if body["output_config"] != nil {
			anthropicError(w, 400, "invalid_request_error", "output_config: Extra inputs are not permitted")
			return
		}
		_, _ = w.Write([]byte(nativeText))
	}}
	a := newAnthropicHost(t, "/v1", h)
	parse := func(entry string) Caps {
		models, err := ParseModels("application/json", []byte(`{"data":[`+entry+`]}`))
		if err != nil {
			t.Fatal(err)
		}
		return models[0].Caps()
	}
	known := parse(capsHaiku55)
	a.caps().Put("claude-haiku-5-5", known)
	req := ChatRequest{Model: "claude-haiku-5-5", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: "high"}
	if _, err := a.Chat(context.Background(), req); err != nil || !a.shapeOf(req).NoOutputConfig {
		t.Fatalf("the route refused output_config: %v %+v", err, a.shapeOf(req))
	}
	a.caps().Put("claude-haiku-5-5", parse(capsHaiku55))
	if !a.shapeOf(req).NoOutputConfig {
		t.Fatal("the same list read again keeps the shape")
	}
	other := known
	other.Efforts = []string{"low", "medium", "high"}
	a.caps().Put("claude-haiku-5-5", other)
	if a.shapeOf(req).NoOutputConfig {
		t.Fatal("different capabilities drop the shape")
	}
}

// P3-6: a batch item's refusal is read against the request the item sent: a bare tool turn's budget went
// out as thinking off, so a refusal of that turns thinking off another way, not thinking off altogether.
func TestBatchRefusalReadsTheBareToolTurn(t *testing.T) {
	a := newAnthropicHost(t, "/v1", &anthropicHost{})
	const model = "claude-haiku-4-5-20251001"
	models, err := ParseModels("application/json", []byte(`{"data":[`+capsHaiku45+`]}`))
	if err != nil {
		t.Fatal(err)
	}
	a.caps().Put(model, models[0].Caps())
	req := bareRound(model, "high")
	refusal := &APIError{Status: 400, Message: `"thinking.type.disabled" is not supported for this model.`}
	if !a.LearnRefusal(req, refusal) {
		t.Fatal("the refusal teaches something")
	}
	if sh := a.shapeOf(req); sh.NoEffort || !sh.NoThinkingOff {
		t.Fatalf("thinking off was refused, not thinking: %+v", sh)
	}
}

// P3-4: an override key this code does not know is skipped only when it is named like a price; any other
// unknown key (a threshold, a tier, an hour) drops the entry, which would otherwise apply to every request.
func TestPricingOverrideSkipsOnlyPriceNames(t *testing.T) {
	raw := json.RawMessage(`[` +
		`{"above_tokens":200000,"prompt":"0.000006"},{"threshold":"200000","prompt":"0.000006"},{"tier":2,"prompt":"0.000006"},{"hour":14,"prompt":"0.000006"},` +
		`{"min_prompt_tokens":100000,"prompt":"0.000002","input_video":"0.00001","cache_storage":"0.1","search_price":0.01,"output_reasoning":"0.00002"}]`)
	got, ignored := pricingOverrides(raw)
	if len(got) != 1 || got[0].MinPromptTokens == nil || *got[0].MinPromptTokens != 100000 {
		t.Fatalf("only the entry with price-named extra keys stays: %+v", got)
	}
	if len(ignored) != 4 {
		t.Fatalf("its four unknown prices are reported: %v", ignored)
	}
}
