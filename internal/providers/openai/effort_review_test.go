package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"masamong/internal/errclass"
	"masamong/internal/providers"
)

// Regression tests for the effort-control review (2026-11-03, round 1): one per finding, named after it.

// blockTypes lists the content block types of every turn of an Anthropic request body.
func blockTypes(body map[string]any) []string {
	var out []string
	msgs, _ := body["messages"].([]any)
	for _, m := range msgs {
		content, _ := m.(map[string]any)["content"].([]any)
		for _, b := range content {
			if bm, ok := b.(map[string]any); ok {
				out = append(out, bm["type"].(string))
			}
		}
	}
	return out
}

func failingAnthropic(t *testing.T) *Adapter {
	t.Helper()
	return newAnthropicHost(t, "/v1", &anthropicHost{chat: func(w http.ResponseWriter, _ map[string]any) {
		anthropicError(w, 529, "overloaded_error", "Overloaded")
	}})
}

// A: a thinking block is valid only for the model (and host) that wrote it. After a failover the other
// model gets the answer's text and tool calls without its thinking; the same model gets it back byte for
// byte, and the origin record never reaches any host.
func TestThinkingBlocksStayWithTheirModel(t *testing.T) {
	first := &anthropicHost{chat: func(w http.ResponseWriter, _ map[string]any) { _, _ = w.Write([]byte(nativeToolCall)) }}
	a := newAnthropicHost(t, "/v1", first)
	tools := []Tool{{Type: "function", Function: ToolFunction{Name: "temperature_c", Parameters: json.RawMessage(`{"type":"object"}`)}}}
	ask := []Message{{Role: "user", Content: "Oslo and Cairo?"}}
	res, err := a.Chat(context.Background(), ChatRequest{Model: "claude-a", Messages: ask, MaxTokens: 100, Effort: "high", Tools: tools})
	if err != nil {
		t.Fatal(err)
	}
	round2 := append(append([]Message(nil), ask...), Message{Role: "assistant", Raw: res.Message, ToolCalls: res.ToolCalls},
		Message{Role: "tool", ToolCallID: "toolu_1", Content: "4"}, Message{Role: "tool", ToolCallID: "toolu_2", Content: "27"})
	req := ChatRequest{Model: "claude-a", Messages: round2, MaxTokens: 100, Effort: "high", Tools: tools}

	// the same model and host: the thinking block goes back
	if _, err := a.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if got := blockTypes(first.bodies[1]); !reflect.DeepEqual(got, []string{"text", "thinking", "text", "tool_use", "tool_use", "tool_result", "tool_result"}) {
		t.Fatalf("same model: %v", got)
	}

	// the primary is down: another Anthropic model (another host) and a chat-completions host answer
	other := &anthropicHost{}
	fb := newAnthropicHost(t, "/v1", other)
	var cc recorded
	ccHost := testAdapter(t, APICompatible, func(w http.ResponseWriter, r *http.Request) {
		cc.add(r)
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	})
	for _, link := range []Fallback{{A: fb, Provider: "f", Model: "claude-b"}, {A: ccHost, Provider: "c", Model: "vendor-m"}} {
		down := failingAnthropic(t)
		down.Failover = &Failover{Provider: "p", Model: "claude-a", Fallbacks: []Fallback{link}}
		if _, err := down.Chat(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if got := blockTypes(other.bodies[0]); !reflect.DeepEqual(got, []string{"text", "text", "tool_use", "tool_use", "tool_result", "tool_result"}) {
		t.Fatalf("another model gets no thinking block: %v", got)
	}
	for _, body := range []map[string]any{first.bodies[1], other.bodies[0], cc.bodies[0]} {
		raw, _ := json.Marshal(body)
		if bytes.Contains(raw, []byte(originField)) {
			t.Fatalf("the origin record is never sent: %s", raw)
		}
	}
	if raw, _ := json.Marshal(cc.bodies[0]); bytes.Contains(raw, []byte("sig-test-1")) || bytes.Contains(raw, []byte("Two cities")) {
		t.Fatalf("a chat-completions host gets no thinking: %s", raw)
	}

	// an answer recorded before origins were (no record): sent as it came, as before
	var plain map[string]json.RawMessage
	_ = json.Unmarshal(res.Message, &plain)
	delete(plain, originField)
	legacy, _ := json.Marshal(plain)
	_, turns, err := anthropicMessages([]Message{{Role: "user", Content: "q"}, {Role: "assistant", Raw: legacy}}, "claude-b@x")
	if err != nil || blockType(turns[1].Content[0]) != "thinking" {
		t.Fatalf("no origin recorded, nothing removed: %v %v", turns, err)
	}
}

// A: a 400 about the thinking blocks sent back says nothing about the model's thinking or effort fields:
// nothing is learned, nothing is retried.
func TestThinkingBlockErrorsTeachNothing(t *testing.T) {
	for _, msg := range []string{
		"messages.1.content.0: Invalid `signature` in `thinking` block",
		"messages.3.content.0: `thinking` or `redacted_thinking` blocks in the latest assistant message cannot be modified. These blocks must remain as they were in the original response.",
		"messages.1.content.0.type: Expected `thinking` or `redacted_thinking`, but found `text`. When `thinking` is enabled, a final `assistant` message must start with a thinking block",
	} {
		h := &anthropicHost{chat: func(w http.ResponseWriter, _ map[string]any) { anthropicError(w, 400, "invalid_request_error", msg) }}
		a := newAnthropicHost(t, "/v1", h)
		req := ChatRequest{Model: "claude-a", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 50, Effort: "high"}
		if _, err := a.Chat(context.Background(), req); err == nil {
			t.Fatal("refused")
		}
		if len(h.bodies) != 1 || a.shapeOf(req) != (shape{}) {
			t.Fatalf("%q: %d calls, learned %+v", msg, len(h.bodies), a.shapeOf(req))
		}
	}
}

// C: a refusal on the route narrows what the model's list says (a gateway may refuse a field the model
// takes), and a request that would go out unchanged is not sent again.
func TestRefusalNarrowsKnownCaps(t *testing.T) {
	h := &anthropicHost{chat: func(w http.ResponseWriter, body map[string]any) {
		if body["output_config"] != nil {
			anthropicError(w, 400, "invalid_request_error", "output_config: Extra inputs are not permitted")
			return
		}
		_, _ = w.Write([]byte(nativeText))
	}}
	a := newAnthropicHost(t, "/v1", h)
	models, err := ParseModels("application/json", []byte(`{"data":[`+capsOpus55+`,`+capsHaiku55+`]}`))
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range models {
		a.caps().Put(m.ID, m.Caps())
	}
	for _, c := range []struct{ model, effort string }{{"claude-opus-5-5", "none"}, {"claude-haiku-5-5", "high"}} {
		h.bodies = nil
		if _, err := a.Chat(context.Background(), ChatRequest{Model: c.model, Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: c.effort}); err != nil {
			t.Fatalf("%s: %v after %d calls %v", c.model, err, len(h.bodies), h.bodies)
		}
		if len(h.bodies) != 2 || h.bodies[1]["output_config"] != nil {
			t.Fatalf("%s: output_config dropped on the second call: %v", c.model, h.bodies)
		}
	}
	// a host that refuses every request: no two calls send the same bytes
	for _, msg := range []string{"output_config: Extra inputs are not permitted", "thinking: Extra inputs are not permitted",
		"thinking.type: Input should be 'enabled'", "effort: unsupported", "Unrecognized request argument supplied: temperature"} {
		h := &anthropicHost{chat: func(w http.ResponseWriter, _ map[string]any) { anthropicError(w, 400, "invalid_request_error", msg) }}
		b := newAnthropicHost(t, "/v1", h)
		for _, m := range models {
			b.caps().Put(m.ID, m.Caps())
		}
		_, _ = b.Chat(context.Background(), ChatRequest{Model: "claude-opus-5-5", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: "max"})
		for i := 1; i < len(h.bodies); i++ {
			if reflect.DeepEqual(h.bodies[i], h.bodies[i-1]) {
				t.Fatalf("%q: call %d repeats call %d: %v", msg, i+1, i, h.bodies[i])
			}
		}
	}
}

// E: a known-empty list survives the JSON the price list is stored as; an unknown one stays unknown.
func TestCapsJSONKeepsKnownEmpty(t *testing.T) {
	no := false
	for _, c := range []Caps{{Efforts: []string{}}, {Thinking: []string{}, Efforts: []string{}}, {Reasoning: &no}, {Efforts: []string{"low"}}} {
		raw, err := json.Marshal(c)
		if err != nil {
			t.Fatal(err)
		}
		var back Caps
		if err := json.Unmarshal(raw, &back); err != nil {
			t.Fatal(err)
		}
		if (back.Efforts == nil) != (c.Efforts == nil) || (back.Thinking == nil) != (c.Thinking == nil) || !reflect.DeepEqual(back, c) {
			t.Fatalf("%s: %+v want %+v", raw, back, c)
		}
	}
}

// H: a rerank in parts whose later part fails returns what the parts before it spent.
func TestRerankPartFailureKeepsSpend(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req struct {
			Documents []string `json:"documents"`
			TopN      int      `json:"top_n"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if calls > 1 {
			w.WriteHeader(502)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream failed"}}`))
			return
		}
		rows := []map[string]any{}
		for i := 0; i < req.TopN; i++ {
			rows = append(rows, map[string]any{"index": i, "relevance_score": 1.0 - float64(i)/10})
		}
		b, _ := json.Marshal(map[string]any{"results": rows, "usage": map[string]any{"total_tokens": len(req.Documents), "cost": 0.002}})
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	nano, _ := HostByID("nanogpt")
	h := nano
	h.ID, h.BaseURL = "nanogpt-test", srv.URL+"/api/v1"
	old := hostsData.Hosts
	hostsData.Hosts = append(append([]Host(nil), old...), h)
	t.Cleanup(func() { hostsData.Hosts = old })
	c := providers.NewClient(5*time.Second, nil)
	c.MaxRetries = 0
	t.Setenv("K", "test-key")
	cred, _ := providers.Resolve("env:K")
	a := New(c, h.BaseURL, cred)
	a.API = APICompatible
	docs := make([]string, 1100)
	for i := range docs {
		docs[i] = "doc" + strconv.Itoa(i)
	}
	res, err := a.Rerank(context.Background(), "m", "q", docs, 3)
	if err == nil || calls != 2 {
		t.Fatalf("the second part failed: %v after %d calls", err, calls)
	}
	if res.CostUSD == nil || *res.CostUSD != 0.002 || res.TotalTokens != 1024 || len(res.Indices) != 0 {
		t.Fatalf("the first part's spend comes back, no ranking: %+v", res)
	}
}

// I: Anthropic's spend limits and an empty prepaid balance are quota, not a rate limit or a bad request.
// The wording is the docs' (platform.claude.com rate-limits and api-credits pages, read 2026-11-03): a
// limit the operator set answers 400, the tier's monthly cap 429 with details.error_code and no
// retry-after, a prepaid account without credit 400.
func TestAnthropicSpendLimitsAreQuota(t *testing.T) {
	capBody := `{"type":"error","error":{"type":"rate_limit_error","message":"You have reached your API usage limits: your organization has crossed its monthly API usage threshold, set based on your organization's API tier. You will regain access on 2026-12-01 at 00:00 UTC.","details":{"error_code":"enforced_spend_limit_reached"}},"request_id":"req_test"}`
	for _, c := range []struct {
		status    int
		typ, body string
		raw       string
	}{
		{400, "invalid_request_error", "You have reached your specified API usage limits. You will regain access on 2026-12-01 at 00:00 UTC.", ""},
		{400, "invalid_request_error", "You have reached your specified workspace API usage limits. You will regain access on 2026-12-01 at 00:00 UTC.", ""},
		{429, "", "", capBody},
		{429, "rate_limit_error", "Monthly threshold crossed", strings.Replace(capBody, "You have reached your API usage limits: your organization has crossed its monthly API usage threshold, set based on your organization's API tier.", "Monthly threshold crossed.", 1)},
		{400, "invalid_request_error", "Your credit balance is too low to access the Anthropic API. Please go to Plans & Billing to upgrade or purchase credits.", ""},
	} {
		h := &anthropicHost{chat: func(w http.ResponseWriter, _ map[string]any) {
			if c.raw != "" {
				w.WriteHeader(c.status)
				_, _ = w.Write([]byte(c.raw))
				return
			}
			anthropicError(w, c.status, c.typ, c.body)
		}}
		a := newAnthropicHost(t, "/v1", h)
		_, err := a.Chat(context.Background(), ChatRequest{Model: "claude-a", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 20, Effort: "none"})
		if errclass.Of(err) != errclass.Quota || len(h.bodies) != 1 {
			t.Fatalf("%d %q: %s after %d calls (%v)", c.status, c.body, errclass.Of(err), len(h.bodies), err)
		}
	}
	// a plain rate limit stays one
	h := &anthropicHost{chat: func(w http.ResponseWriter, _ map[string]any) {
		w.Header().Set("Retry-After", "1")
		anthropicError(w, 429, "rate_limit_error", "Number of request tokens has exceeded your per-minute rate limit")
	}}
	if _, err := newAnthropicHost(t, "/v1", h).Chat(context.Background(), ChatRequest{Model: "claude-a", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 20}); errclass.Of(err) != errclass.RateLimited {
		t.Fatalf("rate limit: %s", errclass.Of(err))
	}
}

// Sonnet 8: a parameter list without "reasoning" says "does not think" on OpenRouter only.
func TestSupportedParametersSayNoReasoningOnOpenRouterOnly(t *testing.T) {
	list := `{"data":[{"id":"vendor/m","name":"M","supported_parameters":["tools","reasoning_effort"]},{"id":"vendor/r","name":"R","supported_parameters":["reasoning"]}]}`
	for api, wantNo := range map[API]bool{APIOpenRouter: true, APICompatible: false} {
		a := testAdapter(t, api, func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(list)) })
		models, err := a.ListModels(context.Background())
		if err != nil || len(models) != 2 {
			t.Fatalf("%s: %v %v", api, models, err)
		}
		by := map[string]Model{}
		for _, m := range models {
			by[m.ID] = m
		}
		if no := by["vendor/m"].Reasoning != nil && !*by["vendor/m"].Reasoning; no != wantNo {
			t.Fatalf("%s: vendor/m reasoning %v", api, by["vendor/m"].Reasoning)
		}
		if r := by["vendor/r"].Reasoning; r == nil || !*r {
			t.Fatalf("%s: a listed reasoning parameter still says yes: %v", api, r)
		}
		if c, _ := a.caps().Get("vendor/m"); c.NoReasoning() != wantNo {
			t.Fatalf("%s: cached %+v", api, c)
		}
	}
}

// Sonnet 15: an Anthropic-format gateway that refuses the list route with 400 or 403 has no list; a
// refused key stays a refused key.
func TestAnthropicListRefusedRouteIsNoList(t *testing.T) {
	for _, c := range []struct {
		status int
		typ    string
		msg    string
		want   error
	}{
		{400, "invalid_request_error", "Unknown route GET /v1/models", ErrNoModelList},
		{403, "permission_error", "this endpoint is not available", ErrNoModelList},
		{401, "authentication_error", "invalid x-api-key", ErrAuth},
	} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { anthropicError(w, c.status, c.typ, c.msg) }))
		a := New(providers.NewClient(5*time.Second, nil), srv.URL+"/v1", mustCred(t))
		a.API = APIAnthropic
		a.Client.MaxRetries = 0
		_, err := a.ListModels(context.Background())
		srv.Close()
		if !errors.Is(err, c.want) {
			t.Fatalf("%d: %v want %v", c.status, err, c.want)
		}
	}
}

func mustCred(t *testing.T) providers.Credential {
	t.Helper()
	t.Setenv("K", "test-key")
	cred, err := providers.Resolve("env:K")
	if err != nil {
		t.Fatal(err)
	}
	return cred
}

// Sonnet 17: an override entry with a key that is neither a known condition nor a known other price is
// dropped, even when its value looks like a price.
func TestPricingOverrideUnknownNumericKeyDrops(t *testing.T) {
	raw := json.RawMessage(`[{"context_over":"200000","prompt":"0.000006"},{"min_prompt_tokens":200000,"prompt":"0.000006","input_cache_write_1h":"0.00001"}]`)
	got := parsePricingOverrides(raw)
	if len(got) != 1 || got[0].MinPromptTokens == nil || *got[0].MinPromptTokens != 200000 {
		t.Fatalf("only the entry with known keys stays: %+v", got)
	}
}

// Sonnet 12: Claude Opus 4.7 and later refuse a temperature, and thinking refuses one on every model;
// an older model (hosts.json sampling list) gets the caller's temperature on a call without thinking,
// and a host that refuses it anyway is not asked with it again.
func TestAnthropicTemperatureOnlyWhereTaken(t *testing.T) {
	zero := 0.0
	a := &Adapter{API: APIAnthropic}
	models, _ := ParseModels("application/json", []byte(`{"data":[`+capsHaiku45+`,`+capsHaiku55+`]}`))
	for _, m := range models {
		a.caps().Put(m.ID, m.Caps())
	}
	for _, c := range []struct {
		model, effort string
		want          bool
	}{{"claude-haiku-4-5-20251001", "none", true}, {"claude-haiku-4-5", "none", true}, {"claude-haiku-4-5-20251001", "high", false},
		{"claude-haiku-5-5", "none", false}, {"claude-opus-4-7", "none", false}, {"vendor-model", "none", false}} {
		caps, _ := a.caps().Get(c.model)
		p, err := a.anthropicPayload(ChatRequest{Model: c.model, Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 20, Effort: c.effort, Temperature: &zero}, shape{}, caps)
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(p)
		var sent map[string]any
		_ = json.Unmarshal(raw, &sent)
		if temp, ok := sent["temperature"]; ok != c.want || (ok && temp != 0.0) {
			t.Fatalf("%s at %s: temperature %v want %v", c.model, c.effort, sent["temperature"], c.want)
		}
	}
	h := &anthropicHost{chat: func(w http.ResponseWriter, body map[string]any) {
		if _, ok := body["temperature"]; ok {
			anthropicError(w, 400, "invalid_request_error", "temperature: is deprecated for this model")
			return
		}
		_, _ = w.Write([]byte(nativeText))
	}}
	b := newAnthropicHost(t, "/v1", h)
	req := ChatRequest{Model: "claude-sonnet-4-5", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 20, Effort: "none", Temperature: &zero}
	for i := 0; i < 2; i++ {
		if _, err := b.Chat(context.Background(), req); err != nil {
			t.Fatal(err)
		}
	}
	if len(h.bodies) != 3 || h.bodies[1]["temperature"] != nil || h.bodies[2]["temperature"] != nil {
		t.Fatalf("refused once, learned: %v", h.bodies)
	}
}

// Kept (Sonnet 1): low thinks on Anthropic with the least room, 1024 tokens (the smallest budget).
func TestThinkingRoomLowIsTheLeast(t *testing.T) {
	if thinkingRoom("low") != 1024 || thinkingRoom("none") != 1024 || thinkingRoom("medium") <= thinkingRoom("low") {
		t.Fatalf("low %d none %d medium %d", thinkingRoom("low"), thinkingRoom("none"), thinkingRoom("medium"))
	}
	if !strings.Contains(strings.Join(EffortLevels, ","), "none,minimal,low,medium,high,xhigh,max") {
		t.Fatalf("levels %v", EffortLevels)
	}
}

// Sonnet 11: the API takes a conversation that starts with a user turn. A live history cut to start on
// the bot's own line drops that context-only text (as a batch item does); one that cannot start with a
// user turn is refused here, without a call.
func TestAnthropicChatStartsWithUserTurn(t *testing.T) {
	h := &anthropicHost{}
	a := newAnthropicHost(t, "/v1", h)
	ask := func(msgs ...Message) error {
		_, err := a.Chat(context.Background(), ChatRequest{Model: "claude-a", Messages: msgs, MaxTokens: 20, Effort: "none"})
		return err
	}
	if err := ask(Message{Role: "system", Content: "s"}, Message{Role: "assistant", Content: "the bot's earlier line"}, Message{Role: "user", Content: "q"}); err != nil {
		t.Fatal(err)
	}
	msgs, _ := h.bodies[0]["messages"].([]any)
	if len(msgs) != 1 || msgs[0].(map[string]any)["role"] != "user" {
		t.Fatalf("the first turn is the user's: %v", h.bodies[0]["messages"])
	}
	for _, bad := range [][]Message{
		{{Role: "system", Content: "s"}},
		{{Role: "system", Content: "s"}, {Role: "assistant", Content: "only the bot"}},
		{{Role: "assistant", ToolCalls: []ToolCall{{ID: "c1", Function: ToolCallFunction{Name: "f", Arguments: "{}"}}}}, {Role: "tool", ToolCallID: "c1", Content: "x"}},
	} {
		err := ask(bad...)
		if err == nil || errclass.Of(err) != errclass.BadRequest || !strings.Contains(err.Error(), "user message") {
			t.Fatalf("%v: %v", bad, err)
		}
	}
	if len(h.bodies) != 1 {
		t.Fatalf("a refused request is not sent: %d calls", len(h.bodies))
	}
}

// Sonnet 16: a batch answer's cache writes are counted and priced (1.25x input), its thinking tokens
// recorded, and a declined answer (stop_reason refusal) is ErrRefused with its spend kept.
func TestResultsAnthropicCacheWritesAndRefusal(t *testing.T) {
	f := newFake(t)
	a := f.adapter(APIAnthropic, "/v1")
	a.Price = func(string) (float64, float64, bool) { return 1e-6, 4e-6, true }
	f.reply("GET", "/v1/messages/batches/b1", 200, `{"id":"b1","processing_status":"ended","results_url":"`+f.srv.URL+`/v1/messages/batches/b1/results","request_counts":{"succeeded":2}}`)
	f.on("GET", "/v1/messages/batches/b1/results", func(w http.ResponseWriter, _ fakeReq) {
		row := func(id, stop string) string {
			return `{"custom_id":"` + id + `","result":{"type":"succeeded","message":{"model":"claude-x","content":[{"type":"text","text":"answer"}],"stop_reason":"` + stop + `",` +
				`"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":100,"cache_read_input_tokens":20,"output_tokens_details":{"thinking_tokens":3}}}}}`
		}
		_, _ = w.Write([]byte(row("a", "end_turn") + "\n" + row("r", "refusal") + "\n"))
	})
	outs, err := a.BatchResults(context.Background(), BatchRef{ID: "b1", BaseURL: a.BaseURL, Kind: BatchChat, Model: "claude-x", SubmittedAt: time.Now()})
	if err != nil {
		t.Fatal(err)
	}
	m := outcomeByID(outs)
	c := m["a"].Chat
	// (10×1e-6 + 100×1.25e-6 + 20×1e-6 (no cached price: the input price) + 5×4e-6) × 0.5
	if c == nil || m["a"].Err != nil || c.PromptTokens != 130 || c.CachedTokens != 20 || c.CacheWriteTokens != 100 || c.ReasoningTokens != 3 ||
		c.CostUSD == nil || math.Abs(*c.CostUSD-87.5e-6) > 1e-12 {
		t.Fatalf("a %+v cost %v", c, c.CostUSD)
	}
	if r := m["r"]; !errors.Is(r.Err, ErrRefused) || r.Chat == nil || r.Chat.CostUSD == nil || math.Abs(*r.Chat.CostUSD-87.5e-6) > 1e-12 {
		t.Fatalf("a declined item is ErrRefused and billed: %+v %v", r.Chat, r.Err)
	}
}
