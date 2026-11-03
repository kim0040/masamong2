package openai

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/errclass"
	"masamong/internal/providers"
)

// Capability entries as Anthropic's GET /v1/models/{id} returned them (2026-11-03), trimmed to the
// fields read here.
const (
	capsHaiku55 = `{"type":"model","id":"claude-haiku-5-5","display_name":"Claude Haiku 5.5","max_input_tokens":1000000,"max_tokens":128000,"capabilities":{` +
		`"effort":{"supported":true,"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},"xhigh":{"supported":true},"max":{"supported":true}},` +
		`"image_input":{"supported":true},` +
		`"thinking":{"supported":true,"types":{"enabled":{"supported":false},"adaptive":{"supported":true},"disabled":{"supported":true},"between_tools":{"supported":false}}}}}`
	capsSonnet55 = `{"type":"model","id":"claude-sonnet-5-5","display_name":"Claude Sonnet 5.5","max_input_tokens":1000000,"capabilities":{` +
		`"effort":{"supported":true,"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},"xhigh":{"supported":true},"max":{"supported":true}},` +
		`"thinking":{"supported":true,"types":{"enabled":{"supported":false},"adaptive":{"supported":true},"disabled":{"supported":false},"between_tools":{"supported":true}}}}}`
	capsOpus55 = `{"type":"model","id":"claude-opus-5-5","display_name":"Claude Opus 5.5","max_input_tokens":1000000,"capabilities":{` +
		`"effort":{"supported":true,"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},"xhigh":{"supported":true},"max":{"supported":true}},` +
		`"thinking":{"supported":true,"types":{"enabled":{"supported":false},"adaptive":{"supported":true},"disabled":{"supported":false},"between_tools":{"supported":false}}}}}`
	capsHaiku45 = `{"type":"model","id":"claude-haiku-4-5-20251001","display_name":"Claude Haiku 4.5","max_input_tokens":200000,"capabilities":{` +
		`"effort":{"supported":false,"low":{"supported":false},"medium":{"supported":false},"high":{"supported":false},"xhigh":{"supported":false},"max":{"supported":false}},` +
		`"thinking":{"supported":true,"types":{"enabled":{"supported":true},"adaptive":{"supported":false},"disabled":{"supported":true},"between_tools":{"supported":false}}}}}`
)

// Native answers shaped like the live ones (2026-11-03, Claude Haiku 5.5).
const (
	nativeText     = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[{"type":"text","text":"0.05"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":56,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0,"output_tokens_details":{"thinking_tokens":0}}}`
	nativeToolCall = `{"id":"msg_2","type":"message","role":"assistant","model":"claude-haiku-5-5","content":[` +
		`{"type":"thinking","thinking":"Two cities, one call each.","signature":"sig-test-1"},{"type":"text","text":"Checking."},` +
		`{"type":"tool_use","id":"toolu_1","name":"temperature_c","input":{"city":"Oslo"}},{"type":"tool_use","id":"toolu_2","name":"temperature_c","input":{"city":"Cairo"}}],` +
		`"stop_reason":"tool_use","usage":{"input_tokens":400,"output_tokens":236,"cache_creation_input_tokens":100,"cache_read_input_tokens":200,"output_tokens_details":{"thinking_tokens":102}}}`
)

// anthropicHost is a fake Anthropic-format host: model lookups answer from caps (404 for a model not in
// it), POST …/messages goes to chat. Paths and headers of every request are kept.
type anthropicHost struct {
	mu      sync.Mutex
	caps    map[string]string
	lookups int
	bodies  []map[string]any
	paths   []string
	headers []http.Header
	chat    func(w http.ResponseWriter, body map[string]any)
	// manual enforces the rule of thinking type "enabled" (manual mode): the final assistant turn of a
	// tool loop must start with a thinking block
	manual bool
}

func (h *anthropicHost) serve(w http.ResponseWriter, r *http.Request) {
	h.mu.Lock()
	h.paths = append(h.paths, r.Method+" "+r.URL.Path)
	h.headers = append(h.headers, r.Header.Clone())
	h.mu.Unlock()
	if r.Method == http.MethodGet {
		h.mu.Lock()
		h.lookups++
		h.mu.Unlock()
		id := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]
		if id == "models" && r.URL.Query().Get("limit") != "" && len(h.caps) > 0 { // the list
			var items []string
			for _, c := range h.caps {
				items = append(items, c)
			}
			_, _ = w.Write([]byte(`{"data":[` + strings.Join(items, ",") + `],"has_more":false}`))
			return
		}
		if c, ok := h.caps[id]; ok {
			_, _ = w.Write([]byte(c))
			return
		}
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"not_found_error","message":"model: ` + id + `"}}`))
		return
	}
	var rec recorded
	body := rec.add(r)
	h.mu.Lock()
	h.bodies = append(h.bodies, body)
	h.mu.Unlock()
	if h.manual && thinkingType(body) == "enabled" && !startsWithThinking(body) {
		anthropicError(w, 400, "invalid_request_error", "messages.1.content.0.type: Expected `thinking` or `redacted_thinking`, but found `text`. "+
			"When `thinking` is enabled, a final `assistant` message must start with a thinking block (preceeding the lastmost set of `tool_use` and `tool_result` blocks).")
		return
	}
	if h.chat != nil {
		h.chat(w, body)
		return
	}
	_, _ = w.Write([]byte(nativeText))
}

// startsWithThinking reports a request whose last assistant turn, when it calls a tool, starts with a
// thinking block (true when there is no such turn).
func startsWithThinking(body map[string]any) bool {
	msgs, _ := body["messages"].([]any)
	for i := len(msgs) - 1; i >= 0; i-- {
		m, _ := msgs[i].(map[string]any)
		if m["role"] != "assistant" {
			continue
		}
		content, _ := m["content"].([]any)
		calls := false
		for _, b := range content {
			calls = calls || b.(map[string]any)["type"] == "tool_use"
		}
		if !calls || len(content) == 0 {
			return true
		}
		first := content[0].(map[string]any)["type"]
		return first == "thinking" || first == "redacted_thinking"
	}
	return true
}

func newAnthropicHost(t *testing.T, base string, h *anthropicHost) *Adapter {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(srv.Close)
	c := providers.NewClient(5*time.Second, nil)
	c.MaxRetries = 0
	t.Setenv("K", "test-key")
	cred, _ := providers.Resolve("env:K")
	a := New(c, srv.URL+base, cred)
	a.API = APIAnthropic
	return a
}

func anthropicError(w http.ResponseWriter, status int, typ, msg string) {
	w.WriteHeader(status)
	_, _ = w.Write([]byte(`{"type":"error","error":{"type":"` + typ + `","message":` + strconv.Quote(msg) + `}}`))
}

func thinkingType(body map[string]any) any {
	th, _ := body["thinking"].(map[string]any)
	if th == nil {
		return nil
	}
	return th["type"]
}

func outputEffort(body map[string]any) any {
	oc, _ := body["output_config"].(map[string]any)
	if oc == nil {
		return nil
	}
	return oc["effort"]
}

// The chat is translated once, here: system messages joined into the top-level system, turns of one
// role merged, pictures as image blocks before the text, tool calls as tool_use and tool answers as
// tool_result blocks first in their user turn. An Anthropic answer sent back raw keeps its blocks (the
// signed thinking block included); an answer of another host is translated like any message.
func TestAnthropicMessagesConversion(t *testing.T) {
	native, err := ParseAnthropicChat("application/json", []byte(nativeToolCall))
	if err != nil {
		t.Fatal(err)
	}
	other := json.RawMessage(`{"role":"assistant","content":null,"tool_calls":[{"id":"call_9","type":"function","function":{"name":"time.now","arguments":"{}"}}]}`)
	msgs := []Message{
		{Role: "system", Content: "be brief"}, {Role: "system", Content: "in Korean"},
		{Role: "assistant", Content: "earlier answer"},
		{Role: "user", Content: "first"}, {Role: "user", Content: "what is this?", Images: []ImagePart{{MIME: "image/png", Data: []byte{1, 2, 3}}}},
		{Role: "assistant", Raw: native.Message, ToolCalls: native.ToolCalls},
		{Role: "tool", ToolCallID: "toolu_1", Content: `{"temp_c":4}`}, {Role: "tool", ToolCallID: "toolu_2", Content: `{"temp_c":27}`},
		{Role: "assistant", Raw: other},
		{Role: "tool", ToolCallID: "call_9", Content: "12:00"},
		{Role: "assistant", Content: "", ToolCalls: []ToolCall{{ID: "call_8", Function: ToolCallFunction{Name: "weather.now", Arguments: `not json`}}}},
		{Role: "tool", ToolCallID: "call_8", Content: ""},
	}
	p, err := (&Adapter{API: APIAnthropic}).anthropicPayload(ChatRequest{Model: "m", Messages: msgs, MaxTokens: 100, Effort: "none"}, shape{}, Caps{})
	if err != nil {
		t.Fatal(err)
	}
	if p.System != "be brief\n\nin Korean" {
		t.Fatalf("system %q", p.System)
	}
	var got []string
	for _, turn := range p.Messages {
		var kinds []string
		for _, b := range turn.Content {
			kinds = append(kinds, blockType(b))
		}
		got = append(got, turn.Role+":"+strings.Join(kinds, ","))
	}
	// the bot's own line a cut history starts with is left out: the API takes a user turn first
	want := []string{"user:text,image,text", "assistant:thinking,text,tool_use,tool_use", "user:tool_result,tool_result",
		"assistant:tool_use", "user:tool_result", "assistant:tool_use", "user:tool_result"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("turns\n got %v\nwant %v", got, want)
	}
	if string(p.Messages[1].Content[0]) != `{"type":"thinking","thinking":"Two cities, one call each.","signature":"sig-test-1"}` {
		t.Fatalf("the thinking block goes back byte for byte: %s", p.Messages[1].Content[0])
	}
	var img struct {
		Source map[string]string `json:"source"`
	}
	_ = json.Unmarshal(p.Messages[0].Content[1], &img)
	if img.Source["type"] != "base64" || img.Source["media_type"] != "image/png" || img.Source["data"] != "AQID" {
		t.Fatalf("image block %s", p.Messages[0].Content[1])
	}
	var tu, tr, empty map[string]any
	_ = json.Unmarshal(p.Messages[3].Content[0], &tu)
	_ = json.Unmarshal(p.Messages[2].Content[0], &tr)
	_ = json.Unmarshal(p.Messages[6].Content[0], &empty)
	if tu["id"] != "call_9" || tu["name"] != "time.now" || !reflect.DeepEqual(tu["input"], map[string]any{}) {
		t.Fatalf("another host's tool call: %v", tu)
	}
	if tr["tool_use_id"] != "toolu_1" || tr["content"] != `{"temp_c":4}` {
		t.Fatalf("tool result: %v", tr)
	}
	if _, ok := empty["content"]; ok {
		t.Fatalf("an empty tool answer sends no empty content: %v", empty)
	}
	var broken map[string]any
	_ = json.Unmarshal(p.Messages[5].Content[0], &broken)
	if !reflect.DeepEqual(broken["input"], map[string]any{}) {
		t.Fatalf("broken arguments become an empty object: %v", broken)
	}
	// a user text after the tool answers stays after them in the merged turn
	_, turns, _ := anthropicMessages([]Message{{Role: "user", Content: "q"}, {Role: "assistant", ToolCalls: []ToolCall{{ID: "a", Function: ToolCallFunction{Name: "f"}}}},
		{Role: "user", Content: "also this"}, {Role: "tool", ToolCallID: "a", Content: "x"}}, "")
	if blockType(turns[2].Content[0]) != "tool_result" || blockType(turns[2].Content[1]) != "text" {
		t.Fatalf("tool_result first: %s", turns[2].Content)
	}
}

func TestAnthropicToolsAndChoice(t *testing.T) {
	no := false
	for _, c := range []struct {
		choice   any
		parallel *bool
		want     map[string]any
	}{
		{"auto", nil, map[string]any{"type": "auto"}},
		{"required", nil, map[string]any{"type": "auto"}}, // forced choice is refused with thinking on
		{map[string]any{"type": "function", "function": map[string]any{"name": "weather.now"}}, nil, map[string]any{"type": "auto"}},
		{"none", nil, map[string]any{"type": "none"}},
		{nil, nil, nil},
		{nil, &no, map[string]any{"type": "auto", "disable_parallel_tool_use": true}},
	} {
		p, err := (&Adapter{API: APIAnthropic}).anthropicPayload(ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 10,
			Tools: []Tool{weatherTool, {Function: ToolFunction{Name: "time.now"}}}, ToolChoice: c.choice, ParallelToolCalls: c.parallel}, shape{}, Caps{})
		if err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(p.ToolChoice, c.want) {
			t.Errorf("choice %v: %v want %v", c.choice, p.ToolChoice, c.want)
		}
		if len(p.Tools) != 2 || p.Tools[0].Name != "weather.now" || string(p.Tools[0].InputSchema) != string(weatherTool.Function.Parameters) ||
			string(p.Tools[1].InputSchema) != `{"type":"object","properties":{}}` {
			t.Fatalf("tools %+v", p.Tools)
		}
	}
}

func TestAnthropicParsesTheAnswer(t *testing.T) {
	out, err := ParseAnthropicChat("application/json", []byte(nativeToolCall))
	if err != nil {
		t.Fatal(err)
	}
	if out.FinishReason != "tool_calls" || out.Content != "Checking." || len(out.ToolCalls) != 2 || out.ToolCalls[1].Function.Arguments != `{"city":"Cairo"}` ||
		out.ToolCalls[0].Type != "function" || out.ToolCalls[0].ID != "toolu_1" {
		t.Fatalf("answer %+v", out)
	}
	if out.PromptTokens != 700 || out.CachedTokens != 200 || out.CacheWriteTokens != 100 || out.CompletionTokens != 236 || out.ReasoningTokens != 102 {
		t.Fatalf("usage %+v", out)
	}
	var m struct {
		Role    string            `json:"role"`
		Content []json.RawMessage `json:"content"`
	}
	if json.Unmarshal(out.Message, &m) != nil || m.Role != "assistant" || len(m.Content) != 4 {
		t.Fatalf("message keeps the blocks: %s", out.Message)
	}
	for stop, want := range map[string]string{"end_turn": "stop", "stop_sequence": "stop", "max_tokens": "length", "tool_use": "tool_calls",
		"model_context_window_exceeded": "length", "pause_turn": "stop"} {
		if got := anthropicFinish(stop); got != want {
			t.Errorf("%s → %s want %s", stop, got, want)
		}
	}
	refused := strings.Replace(nativeText, `"end_turn"`, `"refusal"`, 1)
	res, err := ParseAnthropicChat("application/json", []byte(refused))
	if !errors.Is(err, ErrRefused) || errclass.Of(err) != errclass.Empty || !failoverWorthy(err) || res.CompletionTokens != 5 {
		t.Fatalf("a refusal asks the fallback, its tokens still counted: %+v %v", res, err)
	}
	_, err = ParseAnthropicChat("application/json", []byte(`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`))
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 529 || errclass.Of(err) != errclass.Overloaded {
		t.Fatalf("an error envelope with 200: %v", err)
	}
	if _, err := ParseAnthropicChat("application/json", []byte(`{"type":"message"}`)); err == nil {
		t.Fatal("an answer without content is not an answer")
	}
}

// Errors come back in the Anthropic envelope; they classify as every host's do.
func TestAnthropicErrorClasses(t *testing.T) {
	for _, c := range []struct {
		status   int
		typ, msg string
		want     errclass.Class
	}{
		{529, "overloaded_error", "Overloaded", errclass.Overloaded},
		{429, "rate_limit_error", "Number of request tokens has exceeded your per-minute rate limit", errclass.RateLimited},
		{401, "authentication_error", "invalid x-api-key", errclass.Auth},
		{400, "invalid_request_error", "prompt is too long: 1000001 tokens > 1000000 maximum", errclass.ContextLength},
		{400, "invalid_request_error", "Your credit balance is too low to access the Anthropic API.", errclass.Quota},
		{404, "not_found_error", "model: claude-nope", errclass.NotFound},
		{500, "api_error", "Internal server error", errclass.Overloaded},
	} {
		h := &anthropicHost{caps: map[string]string{"claude-haiku-5-5": capsHaiku55}, chat: func(w http.ResponseWriter, _ map[string]any) { anthropicError(w, c.status, c.typ, c.msg) }}
		a := newAnthropicHost(t, "/v1", h)
		_, err := a.Chat(context.Background(), ChatRequest{Model: "claude-haiku-5-5", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 10, Effort: "low"})
		if got := errclass.Of(err); got != c.want {
			t.Errorf("%d %s: %s want %s (%v)", c.status, c.typ, got, c.want, err)
		}
		if c.status == 401 && !errors.Is(err, ErrAuth) {
			t.Errorf("401 is a refused key: %v", err)
		}
	}
}

// Input read from the prompt cache costs a tenth of the input price and input written to it 1.25×
// (the published Anthropic table carries the cached column).
func TestAnthropicCacheTokensArePricedApart(t *testing.T) {
	pub, ok := Published(APIAnthropic, "claude-haiku-5-5")
	if !ok || math.Abs(pub.Cached-pub.In/10) > 1e-15 {
		t.Fatalf("published cache read price is a tenth of input: %+v", pub)
	}
	h := &anthropicHost{caps: map[string]string{}, chat: func(w http.ResponseWriter, _ map[string]any) { _, _ = w.Write([]byte(nativeToolCall)) }}
	a := newAnthropicHost(t, "/v1", h)
	a.Price = func(string) (float64, float64, bool) { return pub.In, pub.Out, true }
	a.CachedPrice = func(string) (float64, bool) { return pub.Cached, true }
	out, err := a.Chat(context.Background(), ChatRequest{Model: "claude-haiku-5-5", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 10, Effort: "none"})
	if err != nil || out.CostUSD == nil {
		t.Fatalf("%+v %v", out, err)
	}
	want := 400*pub.In + 200*pub.Cached + 100*pub.In*1.25 + 236*pub.Out
	if math.Abs(*out.CostUSD-want) > 1e-12 || !out.CostEstimated {
		t.Fatalf("cost %v want %v", *out.CostUSD, want)
	}
}

// The effort table per capability set: how each level is written for a model like Haiku 5.5 (can turn
// thinking off), Sonnet 5.5 (thinks between tools at most), Opus 5.5 (cannot stop thinking), Haiku 4.5
// (budgeted thinking, no effort levels), one with fewer levels, one that does not think, and one whose
// capabilities are unknown.
func TestAnthropicEffortTable(t *testing.T) {
	yes, no := true, false
	all := []string{"low", "medium", "high", "xhigh", "max"}
	sets := map[string]Caps{
		"haiku55":  {Reasoning: &yes, Efforts: all, Thinking: []string{"adaptive", "disabled"}},
		"sonnet55": {Reasoning: &yes, Efforts: all, Thinking: []string{"adaptive", "between_tools"}},
		"opus55":   {Reasoning: &yes, Efforts: all, Thinking: []string{"adaptive"}},
		"haiku45":  {Reasoning: &yes, Efforts: []string{}, Thinking: []string{"enabled", "disabled"}},
		"fewer":    {Reasoning: &yes, Efforts: []string{"low", "medium", "high"}, Thinking: []string{"adaptive", "disabled"}},
		"plain":    {Reasoning: &no, Efforts: []string{}, Thinking: []string{}},
		"unknown":  {},
	}
	type row struct {
		thinking any
		effort   string
		thinks   bool
	}
	want := map[string]map[string]row{
		"haiku55": {"none": {"disabled", "", false}, "minimal": {"disabled", "", false}, "low": {"adaptive", "low", true}, "medium": {"adaptive", "medium", true},
			"high": {"adaptive", "high", true}, "xhigh": {"adaptive", "xhigh", true}, "max": {"adaptive", "max", true}},
		"sonnet55": {"none": {"between_tools", "", false}, "low": {"adaptive", "low", true}, "max": {"adaptive", "max", true}},
		"opus55":   {"none": {nil, "low", true}, "minimal": {nil, "low", true}, "low": {"adaptive", "low", true}, "high": {"adaptive", "high", true}},
		"haiku45":  {"none": {"disabled", "", false}, "low": {"enabled", "", true}, "high": {"enabled", "", true}},
		"fewer":    {"xhigh": {"adaptive", "high", true}, "max": {"adaptive", "high", true}, "low": {"adaptive", "low", true}},
		"plain":    {"none": {nil, "", false}, "high": {nil, "", false}},
		"unknown":  {"none": {"disabled", "", false}, "low": {"adaptive", "low", true}, "max": {"adaptive", "max", true}},
	}
	for set, rows := range want {
		for effort, w := range rows {
			th, lvl, thinks := anthropicEffort(effort, sets[set], shape{}, false)
			var typ any
			if th != nil {
				typ = th["type"]
			}
			if typ != w.thinking || lvl != w.effort || thinks != w.thinks {
				t.Errorf("%s %s: thinking %v effort %q thinks %v; want %v %q %v", set, effort, typ, lvl, thinks, w.thinking, w.effort, w.thinks)
			}
		}
	}
	// a budgeted model gets the level's budget, and the same room on top of its answer limit
	p, _ := (&Adapter{API: APIAnthropic}).anthropicPayload(ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 80, Effort: "high"}, shape{}, sets["haiku45"])
	if p.Thinking["budget_tokens"] != 4096 || p.MaxTokens != 80+4096 || p.OutputConfig != nil {
		t.Fatalf("budgeted thinking: %+v", p)
	}
	// Sonnet thinks between tool calls when it has tools: room for that
	if _, _, thinks := anthropicEffort("none", sets["sonnet55"], shape{}, true); !thinks {
		t.Fatal("between_tools with tools may think")
	}
	// a short planner budget gets room for the thinking at low; with thinking off it is left as asked
	low, _ := (&Adapter{API: APIAnthropic}).anthropicPayload(ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 80, Effort: "low"}, shape{}, sets["haiku55"])
	off, _ := (&Adapter{API: APIAnthropic}).anthropicPayload(ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 80, Effort: "none"}, shape{}, sets["haiku55"])
	probe, _ := (&Adapter{API: APIAnthropic}).anthropicPayload(ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 1}, shape{}, sets["opus55"])
	if low.MaxTokens != 80+thinkingHeadroom || off.MaxTokens != 80 || probe.MaxTokens != 1 || probe.Thinking != nil || probe.OutputConfig != nil {
		t.Fatalf("room: low %d off %d probe %+v", low.MaxTokens, off.MaxTokens, probe)
	}
	if def, _ := (&Adapter{API: APIAnthropic}).anthropicPayload(ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, Effort: "none"}, shape{}, sets["haiku55"]); def.MaxTokens != anthropicDefaultMaxTokens {
		t.Fatalf("the API needs an output limit: %d", def.MaxTokens)
	}
}

// Without capabilities (a model the host's list does not have, or a host without a list) each model's
// way of turning thinking off is learned from its refusal, which names it (seen live 2026-10-31): Haiku
// 5.5 takes "disabled", Sonnet 5.5 wants "between_tools", Opus 5.5 cannot stop thinking and gets its
// lowest effort level instead. The thinking levels go out as adaptive thinking at that level.
func TestAnthropicLearnsThinkingOffWithoutCaps(t *testing.T) {
	h := &anthropicHost{caps: map[string]string{}}
	h.chat = func(w http.ResponseWriter, body map[string]any) {
		model, _ := body["model"].(string)
		switch typ := thinkingType(body); {
		case body["temperature"] != nil || body["reasoning_effort"] != nil:
			t.Errorf("no chat-completions fields go to Anthropic: %v", body)
			anthropicError(w, 400, "invalid_request_error", "temperature: Extra inputs are not permitted")
		case model == "claude-sonnet-5-5" && typ == "disabled":
			anthropicError(w, 400, "invalid_request_error", `To turn thinking off on this model, send "thinking": {"type": "between_tools"} instead of {"type": "disabled"}.`)
		case model == "claude-opus-5-5" && (typ == "disabled" || typ == "between_tools"):
			anthropicError(w, 400, "invalid_request_error", `"thinking.type.disabled" is not supported for this model. Use "thinking.type.adaptive" and "output_config.effort" to control thinking behavior.`)
		default:
			_, _ = w.Write([]byte(nativeText))
		}
	}
	a := newAnthropicHost(t, "/v1", h)
	temp := 0.2
	for _, c := range []struct {
		model  string
		off    any
		effort any
		calls  int
	}{{"claude-haiku-5-5", "disabled", nil, 1}, {"claude-sonnet-5-5", "between_tools", nil, 2}, {"claude-opus-5-5", nil, "low", 2}} {
		h.bodies = nil
		req := ChatRequest{Model: c.model, Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: "none", Temperature: &temp}
		if _, err := a.Chat(context.Background(), req); err != nil || len(h.bodies) != c.calls {
			t.Fatalf("%s none: %v after %d calls %v", c.model, err, len(h.bodies), h.bodies)
		}
		last := h.bodies[len(h.bodies)-1]
		if thinkingType(last) != c.off || outputEffort(last) != c.effort {
			t.Fatalf("%s none written as %v / %v: %v", c.model, thinkingType(last), outputEffort(last), last)
		}
		if _, err := a.Chat(context.Background(), req); err != nil || len(h.bodies) != c.calls+1 {
			t.Fatalf("%s: the next none goes out learned at once: %v %d", c.model, err, len(h.bodies))
		}
		high := req
		high.Effort = "high"
		if _, err := a.Chat(context.Background(), high); err != nil {
			t.Fatal(err)
		}
		hb := h.bodies[len(h.bodies)-1]
		if thinkingType(hb) != "adaptive" || outputEffort(hb) != "high" || hb["max_tokens"] != float64(40+4096) {
			t.Fatalf("%s high: adaptive at high with room: %v", c.model, hb)
		}
	}
	// a host that takes no output_config (a third-party copy of the format) loses that field once
	h.bodies = nil
	h.chat = func(w http.ResponseWriter, body map[string]any) {
		if body["output_config"] != nil {
			anthropicError(w, 400, "invalid_request_error", "output_config: Extra inputs are not permitted")
			return
		}
		_, _ = w.Write([]byte(nativeText))
	}
	req := ChatRequest{Model: "vendor-model", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: "medium"}
	if _, err := a.Chat(context.Background(), req); err != nil || len(h.bodies) != 2 || h.bodies[1]["output_config"] != nil || thinkingType(h.bodies[1]) != "adaptive" {
		t.Fatalf("output_config dropped: %v %v", err, h.bodies)
	}
}

// Known capabilities decide at once: no refusal is needed to learn how a model turns thinking off. They
// come from the model list or an explicit lookup (CapsOf: a provider test), never from the chat call:
// a chat call never asks GET /models/{id}, and a model nothing is known about follows the defaults.
func TestAnthropicCapsAreLookedUpOnce(t *testing.T) {
	h := &anthropicHost{caps: map[string]string{"claude-sonnet-5-5": capsSonnet55, "claude-opus-5-5": capsOpus55}}
	a := newAnthropicHost(t, "/v1", h)
	none := func(model string) ChatRequest {
		return ChatRequest{Model: model, Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: "none"}
	}
	for _, model := range []string{"claude-sonnet-5-5", "claude-opus-5-5"} {
		if _, err := a.Chat(context.Background(), none(model)); err != nil {
			t.Fatal(err)
		}
	}
	if h.lookups != 0 || thinkingType(h.bodies[0]) != "disabled" || thinkingType(h.bodies[1]) != "disabled" {
		t.Fatalf("a chat call asks nothing, unknown caps write the default: %d lookups %v", h.lookups, h.bodies)
	}
	// the explicit lookup (the provider test) fills the cache, once per model, a missing one too
	for i := 0; i < 2; i++ {
		for _, model := range []string{"claude-sonnet-5-5", "claude-opus-5-5", "vendor-x"} {
			a.CapsOf(context.Background(), model)
		}
	}
	if h.lookups != 3 {
		t.Fatalf("one lookup per model: %d", h.lookups)
	}
	h.bodies = nil
	for i := 0; i < 2; i++ {
		for _, model := range []string{"claude-sonnet-5-5", "claude-opus-5-5", "vendor-x"} {
			if _, err := a.Chat(context.Background(), none(model)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if h.lookups != 3 || len(h.bodies) != 6 {
		t.Fatalf("no lookup on the chat path, one call per request: %d lookups, %d calls", h.lookups, len(h.bodies))
	}
	if thinkingType(h.bodies[0]) != "between_tools" || thinkingType(h.bodies[1]) != nil || outputEffort(h.bodies[1]) != "low" || thinkingType(h.bodies[2]) != "disabled" {
		t.Fatalf("sonnet between_tools, opus its lowest level, unknown disabled: %v", h.bodies[:3])
	}
	// the list fills the same cache: no lookup for a listed model
	h2 := &anthropicHost{caps: map[string]string{"claude-haiku-5-5": capsHaiku55}}
	b := newAnthropicHost(t, "/v1", h2)
	if ms, err := b.ListModels(context.Background()); err != nil || len(ms) != 1 || h2.lookups != 1 {
		t.Fatalf("list: %v %v", ms, err)
	}
	if _, err := b.Chat(context.Background(), none("claude-haiku-5-5")); err != nil {
		t.Fatal(err)
	}
	if h2.lookups != 1 || thinkingType(h2.bodies[0]) != "disabled" {
		t.Fatalf("listed caps used without a lookup: %d %v", h2.lookups, h2.bodies)
	}
}

func TestAnthropicCapabilityParsing(t *testing.T) {
	list := `{"data":[` + capsHaiku55 + `,` + capsSonnet55 + `,` + capsOpus55 + `,` + capsHaiku45 + `,{"type":"model","id":"claude-old","display_name":"Old"}],"has_more":false}`
	models, err := ParseModels("application/json", []byte(list))
	if err != nil || len(models) != 5 {
		t.Fatal(err)
	}
	by := map[string]Caps{}
	for _, m := range models {
		by[m.ID] = m.Caps()
	}
	if c := by["claude-haiku-5-5"]; !*c.Reasoning || len(c.Efforts) != 5 || c.ThinkingOff() != "disabled" || !*models[0].ImageInput {
		t.Fatalf("haiku 5.5: %+v", c)
	}
	if c := by["claude-sonnet-5-5"]; c.ThinkingOff() != "between_tools" {
		t.Fatalf("sonnet 5.5: %+v", c)
	}
	if c := by["claude-opus-5-5"]; c.ThinkingOff() != "never" || !reflect.DeepEqual(c.Thinking, []string{"adaptive"}) {
		t.Fatalf("opus 5.5: %+v", c)
	}
	if c := by["claude-haiku-4-5-20251001"]; c.Efforts == nil || len(c.Efforts) != 0 || !reflect.DeepEqual(c.Thinking, []string{"enabled", "disabled"}) {
		t.Fatalf("haiku 4.5 takes no effort level: %+v", c)
	}
	if c := by["claude-old"]; c.Known() || c.ThinkingOff() != "" {
		t.Fatalf("no capabilities: unknown, not none: %+v", c)
	}
	// OpenRouter's supported_parameters, NanoGPT's detailed capabilities, Gemini's native thinking flag
	other := `{"data":[{"id":"deepseek/deepseek-v4.1-flash","context_length":1000,"supported_parameters":["tools","reasoning","include_reasoning"]},` +
		`{"id":"mistralai/small","context_length":1000,"supported_parameters":["tools","temperature"]},` +
		`{"id":"openai/gpt-6.1-sol","object":"model","capabilities":{"vision":true,"reasoning":true,"tool_calling":true}},` +
		`{"id":"models/gemini-3.8-flash","thinking":true},{"id":"gpt-5.4-mini","object":"model"}]}`
	models, err = ParseModels("application/json", []byte(other))
	if err != nil {
		t.Fatal(err)
	}
	wantR := []any{true, false, true, true, nil}
	for i, m := range models {
		if got := deref(m.Reasoning); got != wantR[i] || m.Efforts != nil {
			t.Errorf("%s: reasoning %v want %v (efforts %v)", m.ID, got, wantR[i], m.Efforts)
		}
	}
}

// api: anthropic means the Messages format at any address: Anthropic's own (/v1), a third-party root
// (/anthropic, to which /v1 is added) or the full endpoint pasted. Third-party hosts get the key as a
// bearer token too; a host without a model list is not a failure but "list unavailable".
func TestAnthropicFormatOnAnyHost(t *testing.T) {
	for base, path := range map[string]string{"/v1": "/v1/messages", "/anthropic": "/anthropic/v1/messages", "/api/anthropic/": "/api/anthropic/v1/messages", "/v1/messages": "/v1/messages"} {
		h := &anthropicHost{caps: map[string]string{}}
		a := newAnthropicHost(t, base, h)
		res, err := a.Chat(context.Background(), ChatRequest{Model: "vendor-chat", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 20, Effort: "low"})
		if err != nil || res.Content != "0.05" {
			t.Fatalf("%s: %v", base, err)
		}
		if got := h.paths[len(h.paths)-1]; got != "POST "+path {
			t.Fatalf("%s: posted to %s want %s", base, got, path)
		}
		hd := h.headers[len(h.headers)-1]
		if hd.Get("x-api-key") != "test-key" || hd.Get("Authorization") != "Bearer test-key" || hd.Get("anthropic-version") != anthropicVersion {
			t.Fatalf("%s: headers %v", base, hd)
		}
		if _, err := a.ListModels(context.Background()); !errors.Is(err, ErrNoModelList) {
			t.Fatalf("%s: a host without a list: %v", base, err)
		}
	}
	official := &Adapter{API: APIAnthropic, BaseURL: "https://api.anthropic.com/v1"}
	if h := official.anthropicHeaders(); h["Authorization"] != "" || h["anthropic-version"] == "" {
		t.Fatalf("Anthropic's own host gets x-api-key alone: %v", h)
	}
	// a refused key on the list is still a refused key, not "no list"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		anthropicError(w, 401, "authentication_error", "invalid x-api-key")
	}))
	t.Cleanup(srv.Close)
	a := New(providers.NewClient(5*time.Second, nil), srv.URL+"/anthropic", official.Cred)
	a.Client.MaxRetries = 0
	a.Cred, _ = providers.Resolve("env:K")
	a.API = APIAnthropic
	if _, err := a.ListModels(context.Background()); !errors.Is(err, ErrAuth) || errors.Is(err, ErrNoModelList) {
		t.Fatalf("401 on the list: %v", err)
	}
}

// A Quick call (a classifier, a one-line summary) asks for no thinking unless the role's effort was set;
// then it asks for that level with room for the thinking. A fallback with an effort of its own uses it.
func TestQuickCallsFollowAnExplicitRoleEffort(t *testing.T) {
	var rec recorded
	a := testAdapter(t, APICompatible, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	})
	quick := ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: "none", Quick: true}
	if _, err := a.Chat(context.Background(), quick); err != nil {
		t.Fatal(err)
	}
	a.RoleEffort = "medium"
	if _, err := a.Chat(context.Background(), quick); err != nil {
		t.Fatal(err)
	}
	if rec.bodies[0]["reasoning_effort"] != "none" || rec.bodies[0]["max_tokens"] != float64(40) {
		t.Fatalf("unset: none, the limit as asked: %v", rec.bodies[0])
	}
	if rec.bodies[1]["reasoning_effort"] != "medium" || rec.bodies[1]["max_tokens"] != float64(40+thinkingHeadroom) {
		t.Fatalf("set: the role's level with room: %v", rec.bodies[1])
	}
	plain := quick
	plain.Quick, plain.Effort = false, "low"
	if _, err := a.Chat(context.Background(), plain); err != nil || rec.bodies[2]["reasoning_effort"] != "low" || rec.bodies[2]["max_tokens"] != float64(40) {
		t.Fatalf("other calls keep their own effort and limit: %v %v", rec.bodies[2], err)
	}
	// on Anthropic the room follows the level
	h := &anthropicHost{caps: map[string]string{"claude-haiku-5-5": capsHaiku55}}
	b := newAnthropicHost(t, "/v1", h)
	b.RoleEffort = "high"
	quick.Model = "claude-haiku-5-5"
	if _, err := b.Chat(context.Background(), quick); err != nil || thinkingType(h.bodies[0]) != "adaptive" || outputEffort(h.bodies[0]) != "high" || h.bodies[0]["max_tokens"] != float64(40+4096) {
		t.Fatalf("anthropic quick at the role's level: %v %v", h.bodies, err)
	}
}

func TestFallbackEffortReplacesTheLevel(t *testing.T) {
	primary := testAdapter(t, APICompatible, func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(503)
		_, _ = w.Write([]byte(`{"error":{"message":"overloaded"}}`))
	})
	var rec recorded
	fb := testAdapter(t, APICompatible, func(w http.ResponseWriter, r *http.Request) {
		rec.add(r)
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	})
	fb.RoleEffort = "high"
	primary.Failover = &Failover{Provider: "p", Model: "m", Fallbacks: []Fallback{{A: fb, Provider: "f", Model: "m2", Effort: "high"}}}
	if _, err := primary.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 50, Effort: "low"}); err != nil {
		t.Fatal(err)
	}
	if _, err := primary.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 50, Effort: "none", Quick: true}); err != nil {
		t.Fatal(err)
	}
	if rec.bodies[0]["reasoning_effort"] != "high" || rec.bodies[0]["model"] != "m2" || rec.bodies[1]["reasoning_effort"] != "high" || rec.bodies[1]["max_tokens"] != float64(50+thinkingHeadroom) {
		t.Fatalf("the fallback's own level: %v", rec.bodies)
	}
}

// Elsewhere capabilities only remove or clamp: a model the list says does not reason gets no reasoning
// field, a level above what the family takes is clamped (Gemini: high; OpenAI, OpenRouter: xhigh), and a
// refused level above high is learned as high. Requests of models nothing is known about are unchanged.
func TestCapabilitiesShapeChatCompletionsEffort(t *testing.T) {
	no := false
	or := &Adapter{API: APIOpenRouter}
	or.caps().Put("mistralai/small", Caps{Reasoning: &no})
	req := ChatRequest{Model: "mistralai/small", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 10, Effort: "low"}
	if p := or.payload(req, shape{}); p["reasoning"] != nil {
		t.Fatalf("a model that does not reason gets no field: %v", p)
	}
	req.Model = "deepseek/deepseek-v4.1-flash"
	if p := or.payload(req, shape{}); p["reasoning"].(map[string]any)["effort"] != "low" {
		t.Fatalf("unknown stays as before: %v", p)
	}
	for api, want := range map[API]string{APIGemini: "high", APIOpenAI: "xhigh", APIOpenRouter: "xhigh", APICompatible: "max"} {
		r := ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 10, Effort: "max"}
		p := (&Adapter{API: api}).payload(r, shape{})
		got := p["reasoning_effort"]
		if api == APIOpenRouter {
			got = p["reasoning"].(map[string]any)["effort"]
		}
		if got != want {
			t.Errorf("%s max → %v want %s", api, got, want)
		}
	}
	var rec recorded
	a := testAdapter(t, APICompatible, func(w http.ResponseWriter, r *http.Request) {
		body := rec.add(r)
		if e := body["reasoning_effort"]; e == "xhigh" || e == "max" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":{"message":"Invalid value for reasoning_effort: 'max'. Supported values are: 'low', 'medium', and 'high'."}}`))
			return
		}
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	})
	r := ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 10, Effort: "max"}
	if _, err := a.Chat(context.Background(), r); err != nil || len(rec.bodies) != 2 || rec.bodies[1]["reasoning_effort"] != "high" {
		t.Fatalf("a refused max is sent as high: %v %v", err, rec.bodies)
	}
	if _, err := a.Chat(context.Background(), r); err != nil || len(rec.bodies) != 3 || rec.bodies[2]["reasoning_effort"] != "high" {
		t.Fatalf("and kept: %v %v", err, rec.bodies)
	}
}

// An Anthropic answer sent back raw to a chat-completions host (a fallback after a failover inside a
// tool loop) goes out as that format: its text and tool calls, no thinking.
func TestNativeAnswerToChatCompletionsHost(t *testing.T) {
	native, _ := ParseAnthropicChat("application/json", []byte(nativeToolCall))
	b, err := json.Marshal(Message{Role: "assistant", Raw: native.Message})
	if err != nil {
		t.Fatal(err)
	}
	want := `{"role":"assistant","content":"Checking.","tool_calls":[{"id":"toolu_1","type":"function","function":{"name":"temperature_c","arguments":"{\"city\":\"Oslo\"}"}},` +
		`{"id":"toolu_2","type":"function","function":{"name":"temperature_c","arguments":"{\"city\":\"Cairo\"}"}}]}`
	if string(b) != want {
		t.Fatalf("\n got %s\nwant %s", b, want)
	}
	// a chat-completions raw message is still sent byte for byte
	plain := json.RawMessage(`{"role":"assistant","content":null,"reasoning_content":"r","tool_calls":[]}`)
	if b, _ := json.Marshal(Message{Raw: plain}); string(b) != string(plain) {
		t.Fatalf("raw kept: %s", b)
	}
}

func TestClampEffort(t *testing.T) {
	for _, c := range []struct {
		effort string
		levels []string
		want   string
		ok     bool
	}{
		{"high", []string{"low", "medium", "high"}, "high", true},
		{"max", []string{"low", "medium", "high"}, "high", true},
		{"none", []string{"low", "medium", "high"}, "low", true},
		{"xhigh", []string{"low", "max"}, "low", true},
		{"medium", nil, "", false},
		{"odd", []string{"medium"}, "medium", true},
	} {
		got, ok := clampEffort(c.effort, c.levels)
		if got != c.want || ok != c.ok {
			t.Errorf("%s in %v: %q %v want %q %v", c.effort, c.levels, got, ok, c.want, c.ok)
		}
	}
}
