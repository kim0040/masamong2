package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"

	"masamong/internal/errclass"
)

// rawServer answers every chat request with the next canned (status, body) and keeps the raw request
// bodies, so tests can compare bytes and nested objects, not only the top-level keys.
type rawServer struct {
	mu      sync.Mutex
	bodies  [][]byte
	replies []reply
}

type reply struct {
	status int
	body   string
}

func (s *rawServer) handler(w http.ResponseWriter, r *http.Request) {
	raw, _ := io.ReadAll(r.Body)
	s.mu.Lock()
	s.bodies = append(s.bodies, raw)
	i := len(s.bodies) - 1
	if i >= len(s.replies) {
		i = len(s.replies) - 1
	}
	rep := s.replies[i]
	s.mu.Unlock()
	w.Header().Set("Content-Type", "application/json")
	if rep.status != 0 {
		w.WriteHeader(rep.status)
	}
	_, _ = w.Write([]byte(rep.body))
}

func (s *rawServer) json(t *testing.T, i int) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(s.bodies[i], &m); err != nil {
		t.Fatal(err)
	}
	return m
}

var weatherTool = Tool{Function: ToolFunction{
	Name:        "weather.now",
	Description: "current weather",
	Parameters:  json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}`),
}}

const twoCallsReply = `{"id":"x","model":"m","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,` +
	`"reasoning_content":"need the weather first","tool_calls":[` +
	`{"id":"call_1","type":"function","function":{"name":"weather.now","arguments":"{\"city\":\"서울\"}"},"extra_content":{"google":{"thought_signature":"abc"}}},` +
	`{"id":"call_2","type":"function","function":{"name":"time.now","arguments":"{}"}}]}}],"usage":{"prompt_tokens":30,"completion_tokens":12}}`

func TestRequestWithoutToolsIsByteForByteUnchanged(t *testing.T) {
	temp := 0.3
	req := ChatRequest{Model: "m", Messages: []Message{{Role: "system", Content: "be brief"}, {Role: "user", Content: "hi"}}, MaxTokens: 50, Temperature: &temp}
	want := `{"max_tokens":50,"messages":[{"role":"system","content":"be brief"},{"role":"user","content":"hi"}],"model":"m","temperature":0.3}`
	for _, api := range APIs {
		body, err := json.Marshal((&Adapter{API: api}).payload(req, shape{}))
		if err != nil {
			t.Fatal(err)
		}
		got := string(body)
		if api == APIOpenAI {
			continue // max_completion_tokens: covered by TestPayloadPerFamily
		}
		if api == APIAnthropic && got == strings.Replace(want, `,"temperature":0.3`, "", 1) {
			continue // Anthropic takes no temperature (shape.go)
		}
		if got != want {
			t.Errorf("%s: payload changed:\n got %s\nwant %s", api, got, want)
		}
		for _, k := range []string{"tools", "tool_choice", "parallel_tool_calls", "provider"} {
			if strings.Contains(got, `"`+k+`"`) {
				t.Errorf("%s: a request without tools carries %q", api, k)
			}
		}
	}
	// tool_choice and parallel_tool_calls mean nothing without tools and some hosts reject them then
	yes := true
	req.ToolChoice, req.ParallelToolCalls = "auto", &yes
	p := (&Adapter{API: APICompatible}).payload(req, shape{})
	if p["tool_choice"] != nil || p["parallel_tool_calls"] != nil {
		t.Fatalf("tool fields without tools must not be sent: %v", p)
	}
}

func TestToolRequestIsSentWithToolFields(t *testing.T) {
	srv := &rawServer{replies: []reply{{200, twoCallsReply}}}
	a := testAdapter(t, APICompatible, srv.handler)
	no := false
	_, err := a.Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "서울 날씨?"}},
		Tools: []Tool{weatherTool}, ToolChoice: "auto", ParallelToolCalls: &no,
	})
	if err != nil {
		t.Fatal(err)
	}
	body := srv.json(t, 0)
	wantTools := `[{"type":"function","function":{"name":"weather.now","description":"current weather","parameters":{"type":"object","properties":{"city":{"type":"string"}},"required":["city"]}}}]`
	var want any
	_ = json.Unmarshal([]byte(wantTools), &want)
	if !reflect.DeepEqual(body["tools"], want) {
		t.Fatalf("tools: %v", body["tools"])
	}
	if body["tool_choice"] != "auto" || body["parallel_tool_calls"] != false {
		t.Fatalf("tool_choice/parallel_tool_calls: %v", body)
	}
	if _, ok := body["provider"]; ok {
		t.Fatal("require_parameters is an OpenRouter routing field only")
	}

	// an object tool_choice goes through as written
	srv2 := &rawServer{replies: []reply{{200, twoCallsReply}}}
	b := testAdapter(t, APIOpenRouter, srv2.handler)
	_, err = b.Chat(context.Background(), ChatRequest{
		Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, Tools: []Tool{weatherTool},
		ToolChoice: map[string]any{"type": "function", "function": map[string]any{"name": "weather.now"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	body = srv2.json(t, 0)
	tc, _ := json.Marshal(body["tool_choice"])
	if string(tc) != `{"function":{"name":"weather.now"},"type":"function"}` {
		t.Fatalf("object tool_choice: %s", tc)
	}
	if !reflect.DeepEqual(body["provider"], map[string]any{"require_parameters": true}) {
		t.Fatalf("OpenRouter must route only to providers that take tools: %v", body["provider"])
	}
	if _, ok := body["parallel_tool_calls"]; ok {
		t.Fatal("parallel_tool_calls unset must not be sent")
	}
}

func TestParseTwoToolCallsWithNullContent(t *testing.T) {
	res, err := ParseChat("application/json", []byte(twoCallsReply))
	if err != nil {
		t.Fatal(err)
	}
	if res.Content != "" || res.FinishReason != "tool_calls" || len(res.ToolCalls) != 2 {
		t.Fatalf("tool-only answer is not an error: %+v", res)
	}
	c := res.ToolCalls[0]
	if c.ID != "call_1" || c.Type != "function" || c.Function.Name != "weather.now" || c.Function.Arguments != `{"city":"서울"}` {
		t.Fatalf("call 0: %+v", c)
	}
	if res.ToolCalls[1].Function.Name != "time.now" || res.ToolCalls[1].Function.Arguments != "{}" {
		t.Fatalf("call 1: %+v", res.ToolCalls[1])
	}
	if res.PromptTokens != 30 || res.CompletionTokens != 12 {
		t.Fatalf("usage: %+v", res)
	}
	// the message is kept whole, provider-only fields included
	var msg map[string]any
	if err := json.Unmarshal(res.Message, &msg); err != nil || msg["reasoning_content"] != "need the weather first" {
		t.Fatalf("message: %s %v", res.Message, err)
	}
}

func TestToolCallVariants(t *testing.T) {
	// arguments as an object (local servers), a missing type, and a plain answer without tool calls
	body := `{"choices":[{"finish_reason":"tool_calls","message":{"content":"","tool_calls":[{"id":"a","function":{"name":"f","arguments":{"x":1}}}]}}]}`
	res, err := ParseChat("application/json", []byte(body))
	if err != nil || len(res.ToolCalls) != 1 || res.ToolCalls[0].Type != "function" || res.ToolCalls[0].Function.Arguments != `{"x":1}` {
		t.Fatalf("variants: %v %+v", err, res)
	}
	res, err = ParseChat("application/json", []byte(geminiChatOK))
	if err != nil || res.Content != "하늘은 파래요." || len(res.ToolCalls) != 0 || res.Message == nil {
		t.Fatalf("plain answer: %v %+v", err, res)
	}
}

func TestMessageRoundTripKeepsProviderFields(t *testing.T) {
	srv := &rawServer{replies: []reply{{200, twoCallsReply}, {200, geminiChatOK}}}
	a := testAdapter(t, APIGemini, srv.handler)
	first := ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "서울 날씨랑 시간?"}}, Tools: []Tool{weatherTool}}
	res, err := a.Chat(context.Background(), first)
	if err != nil {
		t.Fatal(err)
	}
	second := ChatRequest{Model: "m", Tools: []Tool{weatherTool}, Messages: []Message{
		first.Messages[0],
		{Raw: res.Message},
		{Role: "tool", ToolCallID: "call_1", Content: `{"temp":18}`},
		{Role: "tool", ToolCallID: "call_2", Name: "time.now", Content: "10:00"},
	}}
	if _, err := a.Chat(context.Background(), second); err != nil {
		t.Fatal(err)
	}
	msgs := srv.json(t, 1)["messages"].([]any)
	if len(msgs) != 4 {
		t.Fatalf("messages: %v", msgs)
	}
	var sent, returned map[string]any
	_ = json.Unmarshal(res.Message, &returned)
	sent = msgs[1].(map[string]any)
	if !reflect.DeepEqual(sent, returned) {
		t.Fatalf("assistant message must go back unchanged:\n sent %v\n got  %v", sent, returned)
	}
	if sent["reasoning_content"] != "need the weather first" {
		t.Fatalf("reasoning_content lost: %v", sent)
	}
	calls := sent["tool_calls"].([]any)
	sig := calls[0].(map[string]any)["extra_content"].(map[string]any)["google"].(map[string]any)["thought_signature"]
	if sig != "abc" {
		t.Fatalf("thought signature lost: %v", calls[0])
	}
	// tool answers carry tool_call_id
	t1, t2 := msgs[2].(map[string]any), msgs[3].(map[string]any)
	if t1["role"] != "tool" || t1["tool_call_id"] != "call_1" || t1["content"] != `{"temp":18}` {
		t.Fatalf("tool message: %v", t1)
	}
	if _, ok := t1["name"]; ok {
		t.Fatalf("name is optional: %v", t1)
	}
	if t2["tool_call_id"] != "call_2" || t2["name"] != "time.now" {
		t.Fatalf("tool message 2: %v", t2)
	}
}

func TestToolMessagesSerialize(t *testing.T) {
	tool, _ := json.Marshal(Message{Role: "tool", ToolCallID: "c1", Content: "ok"})
	if string(tool) != `{"role":"tool","content":"ok","tool_call_id":"c1"}` {
		t.Fatalf("tool message: %s", tool)
	}
	asst, _ := json.Marshal(Message{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunction{Name: "f"}}}})
	if string(asst) != `{"role":"assistant","content":null,"tool_calls":[{"id":"c1","type":"function","function":{"name":"f","arguments":"{}"}}]}` {
		t.Fatalf("assistant with only tool_calls: %s", asst)
	}
	plain, _ := json.Marshal(Message{Role: "assistant", Content: ""})
	if string(plain) != `{"role":"assistant","content":""}` {
		t.Fatalf("an empty plain message keeps its empty content: %s", plain)
	}
	if _, err := json.Marshal(Message{Raw: json.RawMessage(`{broken`)}); err == nil {
		t.Fatal("invalid Raw must fail, not corrupt the request")
	}
	var back Message
	if err := json.Unmarshal(asst, &back); err != nil || back.Role != "assistant" || len(back.ToolCalls) != 1 || back.ToolCalls[0].ID != "c1" {
		t.Fatalf("unmarshal: %v %+v", err, back)
	}
}

func TestUnsupportedToolsAreRememberedAndFailFast(t *testing.T) {
	refusal := `{"error":{"message":"Unsupported parameter: 'tools' is not supported with this model.","type":"invalid_request_error","param":"tools","code":"unsupported_parameter"}}`
	srv := &rawServer{replies: []reply{{400, refusal}, {200, geminiChatOK}}}
	a := testAdapter(t, APIOpenAI, srv.handler)
	temp := 0.2
	req := ChatRequest{Model: "old-model", Messages: []Message{{Role: "user", Content: "hi"}}, Tools: []Tool{weatherTool}, Temperature: &temp, Effort: "low"}
	if a.ToolsUnsupported("old-model") {
		t.Fatal("nothing known yet")
	}
	_, err := a.Chat(context.Background(), req)
	if errclass.Of(err) != errclass.Unsupported {
		t.Fatalf("want Unsupported, got %v (%v)", errclass.Of(err), err)
	}
	if len(srv.bodies) != 1 {
		t.Fatalf("a tools refusal must not drop effort/temperature and retry: %d requests", len(srv.bodies))
	}
	if !a.ToolsUnsupported("old-model") || a.ToolsUnsupported("other-model") {
		t.Fatal("memory is per model")
	}
	// fail fast: no network call, same class, errors.Is works
	_, err = a.Chat(context.Background(), req)
	if len(srv.bodies) != 1 || errclass.Of(err) != errclass.Unsupported || !errors.Is(err, ErrToolsUnsupported) {
		t.Fatalf("second tool request must fail without a call: %v after %d requests", err, len(srv.bodies))
	}
	// the same model still answers requests without tools
	req.Tools = nil
	if _, err := a.Chat(context.Background(), req); err != nil || len(srv.bodies) != 2 {
		t.Fatalf("plain request: %v %d", err, len(srv.bodies))
	}
	if _, ok := srv.json(t, 1)["tools"]; ok {
		t.Fatal("no tools in the plain request")
	}
}

func TestOpenRouterNoEndpointsForToolsIsUnsupported(t *testing.T) {
	srv := &rawServer{replies: []reply{{404, `{"error":{"message":"No endpoints found that support tool use. To learn more about provider routing, visit: https://openrouter.ai/docs/provider-routing","code":404}}`}}}
	a := testAdapter(t, APIOpenRouter, srv.handler)
	_, err := a.Chat(context.Background(), ChatRequest{Model: "x/y", Messages: []Message{{Role: "user", Content: "hi"}}, Tools: []Tool{weatherTool}})
	if errclass.Of(err) != errclass.Unsupported || !a.ToolsUnsupported("x/y") {
		t.Fatalf("%v %v", errclass.Of(err), err)
	}
}

func TestOtherFailuresDoNotMarkToolsUnsupported(t *testing.T) {
	srv := &rawServer{replies: []reply{{429, `{"error":{"message":"Rate limit reached","type":"requests","code":"rate_limit_exceeded"}}`}}}
	a := testAdapter(t, APICompatible, srv.handler)
	_, err := a.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}, Tools: []Tool{weatherTool}})
	if errclass.Of(err) != errclass.RateLimited || a.ToolsUnsupported("m") {
		t.Fatalf("%v %v", errclass.Of(err), err)
	}
	// a schema complaint is the caller's bug, not a model without tools
	srv = &rawServer{replies: []reply{{400, `{"error":{"message":"Invalid schema for function 'weather.now': 'additionalProperties' is required to be supplied and to be false.","type":"invalid_request_error","param":"tools[0].function.parameters","code":"invalid_function_parameters"}}`}}}
	a = testAdapter(t, APIOpenAI, srv.handler)
	_, err = a.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}, Tools: []Tool{weatherTool}})
	if errclass.Of(err) != errclass.BadRequest || a.ToolsUnsupported("m") {
		t.Fatalf("%v %v", errclass.Of(err), err)
	}
}

func TestParallelToolCallsRefusalIsDroppedNotFatal(t *testing.T) {
	refusal := `{"error":{"message":"Unsupported parameter: 'parallel_tool_calls' is not supported with this model.","type":"invalid_request_error","param":"parallel_tool_calls","code":"unsupported_parameter"}}`
	srv := &rawServer{replies: []reply{{400, refusal}, {200, twoCallsReply}}}
	a := testAdapter(t, APIOpenAI, srv.handler)
	yes := true
	req := ChatRequest{Model: "o-model", Messages: []Message{{Role: "user", Content: "hi"}}, Tools: []Tool{weatherTool}, ParallelToolCalls: &yes}
	res, err := a.Chat(context.Background(), req)
	if err != nil || len(res.ToolCalls) != 2 || len(srv.bodies) != 2 {
		t.Fatalf("%v %+v %d", err, res, len(srv.bodies))
	}
	if _, ok := srv.json(t, 1)["parallel_tool_calls"]; ok {
		t.Fatal("retry must drop parallel_tool_calls")
	}
	if _, ok := srv.json(t, 1)["tools"]; !ok || a.ToolsUnsupported("o-model") {
		t.Fatal("tools stay")
	}
	if _, err := a.Chat(context.Background(), req); err != nil || len(srv.bodies) != 3 {
		t.Fatalf("shape is remembered: %v %d", err, len(srv.bodies))
	}
}

// OpenRouter with require_parameters answers 404 naming no field when no endpoint takes one of them
// (openai/gpt-6-luna and anthropic/claude-haiku-5.5 list neither parallel_tool_calls nor temperature):
// parallel_tool_calls and temperature are dropped together, then effort, and the tools stay. The shape
// is kept for the model's tool requests only: a plain planner call keeps its temperature.
func TestOpenRouterNoEndpointForParametersDropsOptionalFields(t *testing.T) {
	noEndpoint := `{"error":{"message":"No endpoints found that can handle the requested parameters. To learn more about provider routing, visit: https://openrouter.ai/docs/guides/routing/provider-selection","code":404}}`
	srv := &rawServer{replies: []reply{{404, noEndpoint}, {200, twoCallsReply}, {200, twoCallsReply}, {200, `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`}}}
	a := testAdapter(t, APIOpenRouter, srv.handler)
	yes, temp := true, 0.0
	req := ChatRequest{Model: "openai/gpt-6-luna", Messages: []Message{{Role: "user", Content: "hi"}}, Tools: []Tool{weatherTool},
		ParallelToolCalls: &yes, Temperature: &temp, Effort: "low"}
	res, err := a.Chat(context.Background(), req)
	if err != nil || len(res.ToolCalls) != 2 || len(srv.bodies) != 2 {
		t.Fatalf("%v %+v %d", err, res, len(srv.bodies))
	}
	second := srv.json(t, 1)
	_, par := second["parallel_tool_calls"]
	_, tmp := second["temperature"]
	if par || tmp {
		t.Fatalf("parallel_tool_calls and temperature go together: %v", second)
	}
	if second["reasoning"] == nil || second["tools"] == nil || second["provider"] == nil || a.ToolsUnsupported("openai/gpt-6-luna") {
		t.Fatalf("effort, tools and the routing rule stay: %v", second)
	}
	if _, err := a.Chat(context.Background(), req); err != nil || len(srv.bodies) != 3 {
		t.Fatalf("shape is remembered: %v %d", err, len(srv.bodies))
	}
	plain := ChatRequest{Model: "openai/gpt-6-luna", Messages: []Message{{Role: "user", Content: "hi"}}, Temperature: &temp}
	if _, err := a.Chat(context.Background(), plain); err != nil || len(srv.bodies) != 4 {
		t.Fatalf("plain: %v %d", err, len(srv.bodies))
	}
	if _, ok := srv.json(t, 3)["temperature"]; !ok {
		t.Fatal("the tool requests' shape leaked into a plain request")
	}
	// a request without tools is not shaped by this 404 (it carries no require_parameters)
	srv = &rawServer{replies: []reply{{404, noEndpoint}}}
	a = testAdapter(t, APIOpenRouter, srv.handler)
	if _, err := a.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}, Temperature: &temp}); err == nil || len(srv.bodies) != 1 {
		t.Fatalf("plain request retried: %v %d", err, len(srv.bodies))
	}
}

// A model refusing effort "none" (the quick classifiers ask it) drops the field for that request only:
// its later "low" requests still send the effort.
func TestRefusedNoneIsNotLearned(t *testing.T) {
	refused := `{"error":{"message":"reasoning effort 'none' is not supported for this model"}}`
	okReply := `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`
	srv := &rawServer{replies: []reply{{400, refused}, {200, okReply}, {200, okReply}}}
	a := testAdapter(t, APIOpenRouter, srv.handler)
	req := ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}, Effort: "none"}
	if _, err := a.Chat(context.Background(), req); err != nil || len(srv.bodies) != 2 {
		t.Fatalf("%v %d", err, len(srv.bodies))
	}
	req.Effort = "low"
	if _, err := a.Chat(context.Background(), req); err != nil || len(srv.bodies) != 3 {
		t.Fatalf("%v %d", err, len(srv.bodies))
	}
	if eff, _ := srv.json(t, 2)["reasoning"].(map[string]any); eff == nil || eff["effort"] != "low" {
		t.Fatalf("low is still sent: %v", srv.json(t, 2))
	}
}

func TestModelListExposesToolSupport(t *testing.T) {
	list := `{"data":[` +
		`{"id":"a/with","context_length":8000,"supported_parameters":["temperature","tools","tool_choice"]},` +
		`{"id":"b/without","context_length":8000,"supported_parameters":["temperature"]},` +
		`{"id":"c/unknown","context_length":8000}]}`
	ms, err := ParseModels("application/json", []byte(list))
	if err != nil || len(ms) != 3 {
		t.Fatal(err)
	}
	if ms[0].SupportsTools == nil || !*ms[0].SupportsTools || ms[1].SupportsTools == nil || *ms[1].SupportsTools || ms[2].SupportsTools != nil {
		t.Fatalf("SupportsTools: %v %v %v", ms[0].SupportsTools, ms[1].SupportsTools, ms[2].SupportsTools)
	}
}

// Bodies as the hosts send them (OpenAI, OpenRouter, Gemini's compatibility endpoint, DeepSeek).
func TestErrorClassification(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   errclass.Class
	}{
		{"openai context", 400, `{"error":{"message":"This model's maximum context length is 128000 tokens. However, your messages resulted in 130000 tokens. Please reduce the length of the messages.","type":"invalid_request_error","param":"messages","code":"context_length_exceeded"}}`, errclass.ContextLength},
		{"openai no quota", 429, `{"error":{"message":"You exceeded your current quota, please check your plan and billing details.","type":"insufficient_quota","param":null,"code":"insufficient_quota"}}`, errclass.Quota},
		{"openai rate limit", 429, `{"error":{"message":"Rate limit reached for gpt-4o in organization org-x on tokens per min (TPM): Limit 30000, Used 29000.","type":"tokens","param":null,"code":"rate_limit_exceeded"}}`, errclass.RateLimited},
		{"openai bad key", 401, `{"error":{"message":"Incorrect API key provided: sk-abc***.","type":"invalid_request_error","param":null,"code":"invalid_api_key"}}`, errclass.Auth},
		{"openai tools", 400, `{"error":{"message":"Unsupported parameter: 'tools' is not supported with this model.","type":"invalid_request_error","param":"tools","code":"unsupported_parameter"}}`, errclass.Unsupported},
		{"openai tool_choice", 400, `{"error":{"message":"Invalid parameter: 'tool_choice' is not supported for this model.","type":"invalid_request_error","param":"tool_choice","code":null}}`, errclass.Unsupported},
		{"openai parallel only", 400, `{"error":{"message":"Unsupported parameter: 'parallel_tool_calls' is not supported with this model.","type":"invalid_request_error","param":"parallel_tool_calls","code":"unsupported_parameter"}}`, errclass.BadRequest},
		{"openai tool schema", 400, `{"error":{"message":"Invalid schema for function 'x': In context=(), 'additionalProperties' is required to be supplied and to be false.","type":"invalid_request_error","param":"tools[0].function.parameters","code":"invalid_function_parameters"}}`, errclass.BadRequest},
		{"openai model", 404, `{"error":{"message":"The model ` + "`gpt-9`" + ` does not exist or you do not have access to it.","type":"invalid_request_error","param":null,"code":"model_not_found"}}`, errclass.NotFound},
		{"openai temperature", 400, `{"error":{"message":"Unsupported value: 'temperature' does not support 0.3 with this model. Only the default (1) value is supported.","type":"invalid_request_error","param":"temperature","code":"unsupported_value"}}`, errclass.BadRequest},
		{"openai 500", 500, `{"error":{"message":"The server had an error while processing your request. Sorry about that!","type":"server_error","param":null,"code":null}}`, errclass.Overloaded},
		{"openrouter credits", 402, `{"error":{"message":"Insufficient credits. Add more using https://openrouter.ai/settings/credits","code":402}}`, errclass.Quota},
		{"openrouter free limit", 429, `{"error":{"message":"Rate limit exceeded: free-models-per-day. Add 10 credits to unlock 1000 free model requests per day","code":429}}`, errclass.RateLimited},
		{"openrouter tools", 404, `{"error":{"message":"No endpoints found that support tool use. To learn more about provider routing, visit: https://openrouter.ai/docs/provider-routing","code":404}}`, errclass.Unsupported},
		{"openrouter model", 404, `{"error":{"message":"No endpoints found for foo/bar.","code":404}}`, errclass.NotFound},
		{"openrouter context", 400, `{"error":{"message":"This endpoint's maximum context length is 32768 tokens. However, you requested about 40000 tokens (38000 of text input, 2000 in the output).","code":400}}`, errclass.ContextLength},
		{"openrouter key", 401, `{"error":{"message":"No auth credentials found","code":401}}`, errclass.Auth},
		{"openrouter wrapped 529", 200, `{"error":{"message":"Provider returned error","code":529}}`, errclass.Overloaded},
		{"gemini quota", 429, `[{"error":{"code":429,"message":"You exceeded your current quota, please check your plan and billing details.","status":"RESOURCE_EXHAUSTED"}}]`, errclass.Quota},
		{"gemini exhausted", 429, `[{"error":{"code":429,"message":"Resource has been exhausted (e.g. check quota).","status":"RESOURCE_EXHAUSTED"}}]`, errclass.RateLimited},
		{"gemini context", 400, `[{"error":{"code":400,"message":"The input token count (1100000) exceeds the maximum number of tokens allowed (1048576).","status":"INVALID_ARGUMENT"}}]`, errclass.ContextLength},
		{"gemini key", 400, `[{"error":{"code":400,"message":"API key not valid. Please pass a valid API key.","status":"INVALID_ARGUMENT"}}]`, errclass.Auth},
		{"gemini model", 404, `[{"error":{"code":404,"message":"models/gemini-9 is not found for API version v1beta, or is not supported for generateContent.","status":"NOT_FOUND"}}]`, errclass.NotFound},
		{"gemini overloaded", 503, `[{"error":{"code":503,"message":"The model is overloaded. Please try again later.","status":"UNAVAILABLE"}}]`, errclass.Overloaded},
		{"gemini function calling", 400, `[{"error":{"code":400,"message":"Function calling is not enabled for models/gemma-3-27b-it","status":"INVALID_ARGUMENT"}}]`, errclass.Unsupported},
		{"gemini bad argument", 400, `[{"error":{"code":400,"message":"Request contains an invalid argument.","status":"INVALID_ARGUMENT"}}]`, errclass.BadRequest},
		{"deepseek balance", 402, `{"error":{"message":"Insufficient Balance","type":"unknown_error","param":null,"code":"invalid_request_error"}}`, errclass.Quota},
		{"deepseek context", 400, `{"error":{"message":"This model's maximum context length is 65536 tokens. However, you requested 70000 tokens (70000 in the messages, 0 in the completion). Please reduce the length of the messages or completion.","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`, errclass.ContextLength},
		{"deepseek reasoning_content", 400, `{"error":{"message":"The ` + "`reasoning_content`" + ` in the thinking mode must be passed back to the API.","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`, errclass.BadRequest},
		{"deepseek rate", 429, `{"error":{"message":"Rate Limit Reached","type":"rate_limit_error","param":null,"code":"rate_limit_reached"}}`, errclass.RateLimited},
		{"deepseek busy", 503, `{"error":{"message":"Server Overloaded","type":"server_error"}}`, errclass.Overloaded},
		{"deepseek auth", 401, `{"error":{"message":"Authentication Fails (no such user)","type":"authentication_error","param":null,"code":"invalid_request_error"}}`, errclass.Auth},
		{"deepseek no function calling", 400, `{"error":{"message":"deepseek-reasoner does not support Function Calling","type":"invalid_request_error","param":null,"code":"invalid_request_error"}}`, errclass.Unsupported},
		{"plain 422", 422, `{"error":{"message":"Failed to deserialize the JSON body","type":"invalid_request_error"}}`, errclass.BadRequest},
		{"empty 403 body", 403, `{"error":{"message":"forbidden"}}`, errclass.Auth},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			srv := &rawServer{replies: []reply{{c.status, c.body}}}
			a := testAdapter(t, APICompatible, srv.handler)
			_, err := a.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
			if err == nil {
				t.Fatal("want an error")
			}
			if got := errclass.Of(err); got != c.want {
				t.Fatalf("class %q, want %q (%v)", got, c.want, err)
			}
			if c.want == errclass.Auth && !errors.Is(err, ErrAuth) {
				t.Fatalf("a refused key stays ErrAuth: %v", err)
			}
		})
	}
}

func TestBareStatusAndTransportErrorsAreClassed(t *testing.T) {
	srv := &rawServer{replies: []reply{{401, ``}}}
	a := testAdapter(t, APICompatible, srv.handler)
	_, err := a.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if errclass.Of(err) != errclass.Auth {
		t.Fatalf("401 without a body: %v %v", errclass.Of(err), err)
	}
	srv = &rawServer{replies: []reply{{503, `<html>bad gateway</html>`}}}
	a = testAdapter(t, APICompatible, srv.handler)
	_, err = a.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if errclass.Of(err) != errclass.Overloaded || !errclass.Of(err).Retryable() {
		t.Fatalf("503 with an HTML body: %v %v", errclass.Of(err), err)
	}
	a.BaseURL = "http://127.0.0.1:1" // nothing listens
	_, err = a.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if errclass.Of(err) != errclass.Network {
		t.Fatalf("connection refused: %v %v", errclass.Of(err), err)
	}
}

// OpenRouter's GET /key reports the key's own limit; a key without one has no remaining figure, and a
// host other than OpenRouter reports nothing.
func TestCreditReadsTheKeyLimit(t *testing.T) {
	srv := &rawServer{replies: []reply{{200, `{"data":{"limit":20,"limit_remaining":8.5,"usage":11.5,"limit_reset":null}}`}}}
	a := testAdapter(t, APIOpenRouter, srv.handler)
	c, err := a.Credit(context.Background())
	if err != nil || c.Limit == nil || *c.Limit != 20 || c.Remaining == nil || *c.Remaining != 8.5 || c.Usage != 11.5 || c.Reset != "" {
		t.Fatalf("%+v %v", c, err)
	}
	srv = &rawServer{replies: []reply{{200, `{"data":{"limit":null,"limit_remaining":null,"usage":3,"limit_reset":"monthly"}}`}}}
	a = testAdapter(t, APIOpenRouter, srv.handler)
	if c, err := a.Credit(context.Background()); err != nil || c.Limit != nil || c.Remaining != nil || c.Reset != "monthly" {
		t.Fatalf("no limit: %+v %v", c, err)
	}
	a = testAdapter(t, APIOpenAI, srv.handler)
	if _, err := a.Credit(context.Background()); !errors.Is(err, ErrNoCredit) || len(srv.bodies) != 1 {
		t.Fatalf("other host: %v (%d calls)", err, len(srv.bodies))
	}
}

// OpenAI's reasoning models behind a compatible host take tools only without thinking: the request is
// sent again with effort "none", and the model keeps its tools.
func TestToolsWithEffortRefusalDropsEffort(t *testing.T) {
	refusal := `{"error":{"message":"Function tools with reasoning_effort are not supported for gpt-6-luna in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'.","type":"invalid_request_error"}}`
	srv := &rawServer{replies: []reply{{400, refusal}, {200, geminiChatOK}}}
	a := testAdapter(t, APICompatible, srv.handler)
	req := ChatRequest{Model: "openai/gpt-6-luna", Messages: []Message{{Role: "user", Content: "hi"}}, Tools: []Tool{weatherTool}, Effort: "low"}
	if _, err := a.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if len(srv.bodies) != 2 || a.ToolsUnsupported("openai/gpt-6-luna") {
		t.Fatalf("retried without effort, tools kept: %d requests", len(srv.bodies))
	}
	if got := srv.json(t, 1)["reasoning_effort"]; got != "none" {
		t.Fatalf("the retry asks for no thinking: %v", got)
	}
}
