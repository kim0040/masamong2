package openai

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/errclass"
	"masamong/internal/providers"
)

// Regression tests for the provider format audit of 2026-11-03 (one per finding; the numbers are the
// audit's).

// #1 OpenRouter embeds a provider's mid-answer failure in a 200 answer: finish_reason "error" and a
// choice-level error. That is the provider failing, not a cut answer: an error the fallback takes, its
// tokens still billed.
func TestChoiceErrorIsAProviderFailure(t *testing.T) {
	body := `{"choices":[{"finish_reason":"error","message":{"role":"assistant","content":"Partial ans"},"error":{"code":502,"message":"Provider disconnected mid-stream"}}],` +
		`"usage":{"prompt_tokens":40,"completion_tokens":3,"cost":0.00002}}`
	out, err := ParseChat("application/json", []byte(body))
	var ae *APIError
	if !errors.As(err, &ae) || errclass.Of(err) != errclass.Overloaded || !failoverWorthy(err) || out.PromptTokens != 40 || out.CostUSD == nil {
		t.Fatalf("choice error: %+v %v", out, err)
	}
	bare := `{"choices":[{"finish_reason":"error","message":{"role":"assistant","content":""}}],"usage":{"prompt_tokens":4,"completion_tokens":0}}`
	if _, err := ParseChat("application/json", []byte(bare)); err == nil || !failoverWorthy(err) {
		t.Fatalf("finish_reason error alone: %v", err)
	}
	for _, empty := range []string{`null`, `{}`} {
		ok := `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"hi"},"error":` + empty + `}]}`
		if out, err := ParseChat("application/json", []byte(ok)); err != nil || out.Content != "hi" {
			t.Fatalf("an empty choice error is no error: %v", err)
		}
	}
	// through a chain: the fallback answers, the failed call's cost is still reported to the caller
	var calls atomic.Int32
	primary := testAdapter(t, APIOpenRouter, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_, _ = w.Write([]byte(body))
	})
	fb := testAdapter(t, APICompatible, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1}}`))
	})
	primary.Failover = &Failover{Provider: "p", Model: "m", Fallbacks: []Fallback{{A: fb, Provider: "f", Model: "m2"}}}
	res, err := primary.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 20})
	if err != nil || res.Content != "ok" || calls.Load() != 1 || res.CostUSD == nil || *res.CostUSD < 0.00002 {
		t.Fatalf("the fallback answers once, the failed call's cost counted: %+v %v (%d)", res, err, calls.Load())
	}
}

// #2 An override with another price key (OpenRouter added input_cache_write_1h, audio,
// input_audio_cache) still applies: Haiku 5.5's >100K tier is 5x. A condition this code does not know
// still drops the entry.
func TestPricingOverridesIgnoreUnknownPrices(t *testing.T) {
	raw := json.RawMessage(`[{"min_prompt_tokens":100000,"prompt":"0.0000005","completion":"0.0000025","input_cache_read":"0.00000005","input_cache_write_1h":"0.000001","audio":"0.000002","input_audio_cache":"0.0000001"},` +
		`{"max_prompt_tokens":5000,"prompt":"0.00000001"},{"region":"eu","prompt":"0.00000001"},{"min_prompt_tokens":1,"audio":{"rate":1}}]`)
	got := parsePricingOverrides(raw)
	if len(got) != 1 || got[0].MinPromptTokens == nil || *got[0].MinPromptTokens != 100000 || got[0].PromptPrice == nil || *got[0].PromptPrice != 0.0000005 ||
		got[0].CompletionPrice == nil || *got[0].CompletionPrice != 0.0000025 || !got[0].Applies(150000, time.Now()) || got[0].Applies(90000, time.Now()) {
		t.Fatalf("overrides %+v", got)
	}
}

// #3 OpenRouter's 403 is not always the key: a spent budget (error.metadata.limit_source) is Quota, a
// content filter's block a per-request failure the fallback may answer. Neither is a refused key.
func TestForbiddenIsNotAlwaysTheKey(t *testing.T) {
	for _, c := range []struct {
		body    string
		class   errclass.Class
		authErr bool
	}{
		{`{"error":{"code":403,"message":"Key limit exceeded","metadata":{"limit_source":"openrouter_key_limit"}}}`, errclass.Quota, false},
		{`{"error":{"code":403,"message":"Workspace budget reached","metadata":{"limit_source":"openrouter_workspace_budget"}}}`, errclass.Quota, false},
		{`{"error":{"code":"content_policy_violation","message":"The input was flagged"}}`, errclass.Empty, false},
		{`{"error":{"code":403,"message":"flagged","metadata":{"error_type":"refusal"}}}`, errclass.Empty, false},
		{`{"error":{"code":403,"message":"Forbidden"}}`, errclass.Auth, true},
	} {
		a := testAdapter(t, APIOpenRouter, func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(c.body))
		})
		_, err := a.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 10})
		if errclass.Of(err) != c.class || errors.Is(err, ErrAuth) != c.authErr {
			t.Errorf("%s: %s auth=%v (%v)", c.body, errclass.Of(err), errors.Is(err, ErrAuth), err)
		}
		if c.class == errclass.Empty && !failoverWorthy(err) {
			t.Errorf("a filtered request goes to the fallback: %v", err)
		}
	}
}

// #6 #7 #13 Balance and spending caps come with 429 but reset in hours to months: Quota, not a rate
// limit. NanoGPT's authentication_rate_limited waits like a refused key.
func TestQuotaCodesWinOverTheRateLimit(t *testing.T) {
	for _, code := range []string{"credit_balance_exhausted", "organization_spend_limit_exceeded", "project_spend_limit_exceeded",
		"organization_usage_limit_exceeded", "budget_exceeded", "api_key_usd_limit", "api_key_weekly_usd_limit", "api_account_monthly_usd_limit",
		"api_key_daily_subscription_token_limit"} {
		e := &APIError{Status: 429, Type: "rate_limit_error", Code: code, Message: "limit reached"}
		if got := classify(e); got != errclass.Quota {
			t.Errorf("%s: %s", code, got)
		}
	}
	if got := classify(&APIError{Status: 429, Type: "rate_limit_error", Code: "rate_limit_exceeded", Message: "slow down"}); got != errclass.RateLimited {
		t.Errorf("a plain rate limit: %s", got)
	}
	if got := classify(&APIError{Status: 429, Type: "authentication_error", Code: "authentication_rate_limited"}); got != errclass.Auth {
		t.Errorf("authentication_rate_limited: %s", got)
	}
}

// #8 #12 A model the host cannot route is NotFound at any status (CometAPI answers 503), and neither it
// nor a used-up balance is retried by the client; a real rate limit still is.
func TestLastingAnswersAreNotRetried(t *testing.T) {
	for _, c := range []struct {
		status int
		body   string
		class  errclass.Class
		calls  int32
	}{
		{503, `{"error":{"code":"model_not_found","message":"no available channel for group default and model x (distributor)","type":"cometapi_error"}}`, errclass.NotFound, 1},
		{429, `{"error":{"code":"credit_balance_exhausted","message":"no prepaid credits remaining"}}`, errclass.Quota, 1},
		{429, `{"error":{"type":"rate_limit_error","code":"api_key_usd_limit","message":"daily USD limit reached"}}`, errclass.Quota, 1},
		{429, `{"error":{"code":"rate_limit_exceeded","message":"Rate limit reached"}}`, errclass.RateLimited, 3},
		// Gemini's per-minute limit says "exceeded your current quota" too, and is still retried
		{429, `{"error":{"code":429,"message":"You exceeded your current quota, please check your plan and billing details.\n* Quota exceeded for metric: generate_content_free_tier_requests, limit: 10\nPlease retry in 41.6s.","status":"RESOURCE_EXHAUSTED",` +
			`"details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerMinutePerProjectPerModel-FreeTier"}]},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"41s"}]}}`, errclass.Quota, 3},
		// its daily limit is not: the wait is hours
		{429, `{"error":{"code":429,"message":"You exceeded your current quota, please check your plan and billing details.","status":"RESOURCE_EXHAUSTED",` +
			`"details":[{"@type":"type.googleapis.com/google.rpc.QuotaFailure","violations":[{"quotaId":"GenerateRequestsPerDayPerProjectPerModel-FreeTier"}]},{"@type":"type.googleapis.com/google.rpc.RetryInfo","retryDelay":"36000s"}]}}`, errclass.Quota, 1},
	} {
		var calls atomic.Int32
		a := testAdapter(t, APICompatible, func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(c.status)
			_, _ = w.Write([]byte(c.body))
		})
		a.Client.MaxRetries = 2
		a.Client.Sleep = func(context.Context, time.Duration) error { return nil }
		_, err := a.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 10})
		if errclass.Of(err) != c.class || calls.Load() != c.calls {
			t.Errorf("%d %s: %s after %d calls, want %s after %d", c.status, c.body, errclass.Of(err), calls.Load(), c.class, c.calls)
		}
	}
}

// #9 NanoGPT reranks (POST /api/v1/rerank) with up to 1,024 documents per call and raw logit scores,
// which may be negative: more documents go in parts, the scores come back as 0..1 in the same order.
func TestNanoGPTRerankPartsAndScores(t *testing.T) {
	var sizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Documents []string `json:"documents"`
			TopN      int      `json:"top_n"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		sizes = append(sizes, len(req.Documents))
		// the document's number is its logit, minus 1000: early documents score negative
		type row struct {
			Index int     `json:"index"`
			Score float64 `json:"relevance_score"`
		}
		var rows []row
		for i, d := range req.Documents {
			var n float64
			_ = json.Unmarshal([]byte(strings.TrimPrefix(d, "doc")), &n)
			rows = append(rows, row{i, (n - 1000) / 100})
		}
		for i := 0; i < len(rows); i++ { // best first, top_n of them
			for j := i + 1; j < len(rows); j++ {
				if rows[j].Score > rows[i].Score {
					rows[i], rows[j] = rows[j], rows[i]
				}
			}
		}
		b, _ := json.Marshal(map[string]any{"results": rows[:req.TopN], "usage": map[string]int{"total_tokens": len(req.Documents)}})
		_, _ = w.Write(b)
	}))
	t.Cleanup(srv.Close)
	nano, ok := HostByID("nanogpt")
	if !ok || nano.Rerank == nil || !*nano.Rerank || nano.RerankMaxDocs != 1024 || nano.RerankScores != "logit" {
		t.Fatalf("hosts.json nanogpt: %+v", nano)
	}
	h := nano // NanoGPT's settings at the test server's address
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
	if !a.CanRerank() {
		t.Fatal("NanoGPT reranks")
	}
	docs := make([]string, 1500)
	for i := range docs {
		docs[i] = "doc" + strconv.Itoa(i)
	}
	res, err := a.Rerank(context.Background(), "nvidia/Nemotron-3.5-Rerank-8B-BF16", "q", docs, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(sizes) != 2 || sizes[0] != 1024 || sizes[1] != 476 {
		t.Fatalf("parts %v", sizes)
	}
	if len(res.Indices) != 3 || res.Indices[0] != 1499 || res.Indices[1] != 1498 || res.Indices[2] != 1497 || res.TotalTokens != 1500 {
		t.Fatalf("best of all parts: %v %+v", res.Indices, res)
	}
	want := 1 / (1 + math.Exp(-4.99))
	if math.Abs(res.Scores[0]-want) > 1e-9 || res.Scores[0] <= res.Scores[1] {
		t.Fatalf("logits mapped to 0..1, order kept: %v", res.Scores)
	}
	low, err := a.Rerank(context.Background(), "m", "q", docs[:5], 5)
	if err != nil || low.Scores[0] <= 0 || low.Scores[0] >= 0.5 {
		t.Fatalf("a negative logit is a low score in 0..1: %v %v", low.Scores, err)
	}
}

// #10 OpenGateway's list: context_window and modalities.input, and models it no longer serves left out.
func TestOpenGatewayModelList(t *testing.T) {
	body := `{"object":"list","data":[` +
		`{"id":"deepseek/deepseek-v4.1-flash","object":"model","status":"active","context_window":1000000,"modalities":{"input":["text","image"],"output":["text"]},"pricing":{"prompt":"0.0000001","completion":"0.0000004","until":"2026-11-10T00:00:00Z","next":{"at":"2026-11-10T00:00:00Z","prompt":"0.00000014"}}},` +
		`{"id":"old/model","object":"model","status":"retired","context_window":8000},{"id":"paused/model","object":"model","status":"inactive"},` +
		`{"id":"vendor/painter","object":"model","status":"deprecated","modalities":{"input":["text"],"output":["image"]}}]}`
	ms, err := ParseModels("application/json", []byte(body))
	if err != nil || len(ms) != 2 {
		t.Fatalf("%v %+v", err, ms)
	}
	if ms[0].ContextLength != 1000000 || ms[0].ImageInput == nil || !*ms[0].ImageInput || ms[1].ID != "vendor/painter" || !ms[1].ImageOutput || *ms[1].ImageInput {
		t.Fatalf("models %+v", ms)
	}
	if !ms[0].PriceUntil.Equal(time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)) || !ms[1].PriceUntil.IsZero() {
		t.Fatalf("price end %v %v", ms[0].PriceUntil, ms[1].PriceUntil)
	}
	if got := listedTime(json.RawMessage(`1793577600`)); !got.Equal(time.Unix(1793577600, 0)) {
		t.Fatalf("unix seconds %v", got)
	}
}

// #11 OpenRouter's reasoning object and NanoGPT's reasoning_efforts give the levels a model takes: a
// level outside them is clamped up front, max goes out where the list has it, and a model that cannot
// stop thinking gets its lowest level for none.
func TestListedEffortsShapeTheRequest(t *testing.T) {
	list := `{"data":[` +
		`{"id":"openai/gpt-6.1-terra","supported_parameters":["reasoning","tools"],"reasoning":{"mandatory":false,"default_enabled":true,"supported_efforts":["max","xhigh","high","medium","low"],"default_effort":"medium"}},` +
		`{"id":"openai/gpt-6.1-terra-pro","supported_parameters":["reasoning"],"reasoning":{"mandatory":true,"supported_efforts":["max","xhigh","high","medium","low"]}},` +
		`{"id":"recursive/flint","supported_parameters":["reasoning"],"reasoning":{"mandatory":true}},` +
		`{"id":"plain/model","supported_parameters":["tools"],"reasoning":null}]}`
	ms, err := ParseModels("application/json", []byte(list))
	if err != nil {
		t.Fatal(err)
	}
	a := &Adapter{API: APIOpenRouter}
	for _, m := range ms {
		a.caps().Put(m.ID, m.Caps())
	}
	effortOf := func(model, effort string) any {
		p := a.payload(ChatRequest{Model: model, Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 10, Effort: effort}, shape{})
		r, _ := p["reasoning"].(map[string]any)
		if r == nil {
			return nil
		}
		return r["effort"]
	}
	for _, c := range []struct {
		model, effort string
		want          any
	}{
		{"openai/gpt-6.1-terra", "max", "max"}, {"openai/gpt-6.1-terra", "none", "none"},
		{"openai/gpt-6.1-terra-pro", "none", "low"}, {"openai/gpt-6.1-terra-pro", "minimal", "low"},
		{"recursive/flint", "max", "xhigh"}, {"plain/model", "high", nil},
	} {
		if got := effortOf(c.model, c.effort); got != c.want {
			t.Errorf("%s %s → %v want %v", c.model, c.effort, got, c.want)
		}
	}
	nano := `{"data":[{"id":"xingchen/xing4","capabilities":{"reasoning":true},"reasoning_efforts":["none","minimal","low","medium","high","xhigh","max"]},` +
		`{"id":"prism/bonsai","capabilities":{"reasoning":true},"reasoning_efforts":null},{"id":"plain","capabilities":{"reasoning":false}}]}`
	ms, err = ParseModels("application/json", []byte(nano))
	if err != nil || len(ms[0].Efforts) != 7 || ms[1].Efforts != nil || !*ms[1].Reasoning || *ms[2].Reasoning {
		t.Fatalf("nanogpt %+v %v", ms, err)
	}
}

// #4 #5 #17 Levels and tools the hosts' docs give where the list does not: GPT-6 Astra and 6.1 Sol take
// no none (low instead) and, like 6.1 Terra, no tools on Chat Completions; Gemini 3.x/4 list no none
// either (their lowest listed level instead).
func TestDocumentedModelLimits(t *testing.T) {
	oa := &Adapter{API: APIOpenAI, BaseURL: "https://api.openai.com/v1"}
	gm := &Adapter{API: APIGemini, BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai"}
	for _, c := range []struct {
		a             *Adapter
		model, effort string
		want          any
	}{
		{oa, "gpt-6.1-sol", "none", "low"}, {oa, "gpt-6-astra", "none", "low"}, {oa, "gpt-6.1-sol-2026-09-01", "none", "low"},
		{oa, "gpt-6.1-terra", "none", "none"}, {oa, "gpt-6.1-sol", "high", "high"},
		{gm, "gemini-3.8-flash", "none", "low"}, {gm, "models/gemini-4-argon", "minimal", "low"}, {gm, "gemini-3.6-flash", "none", "minimal"},
		{gm, "gemini-3.8-flash-lite", "none", "none"},
	} {
		p := c.a.payload(ChatRequest{Model: c.model, Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 10, Effort: c.effort}, shape{})
		if got := p["reasoning_effort"]; got != c.want {
			t.Errorf("%s %s → %v want %v", c.model, c.effort, got, c.want)
		}
	}
	// a Quick call asking no thinking of a model that always thinks leaves room for the thinking
	p := oa.payload(oa.resolveQuick(ChatRequest{Model: "gpt-6.1-sol", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 120, Quick: true}), shape{})
	if p["reasoning_effort"] != "low" || p["max_completion_tokens"] != 120+thinkingHeadroom {
		t.Errorf("quick call on gpt-6.1-sol: %v %v", p["reasoning_effort"], p["max_completion_tokens"])
	}
	p = oa.payload(oa.resolveQuick(ChatRequest{Model: "gpt-6.1-terra", Messages: []Message{{Role: "user", Content: "x"}}, MaxTokens: 120, Quick: true}), shape{})
	if p["reasoning_effort"] != "none" || p["max_completion_tokens"] != 120 {
		t.Errorf("quick call on gpt-6.1-terra: %v %v", p["reasoning_effort"], p["max_completion_tokens"])
	}
	for model, want := range map[string]bool{"gpt-6-astra": true, "gpt-6.1-sol": true, "gpt-6.1-terra": true, "gpt-6.1-terra-2026-10-01": true, "gpt-6-sol": false, "gpt-6.1-luna": false} {
		if got := oa.ToolsUnsupported(model); got != want {
			t.Errorf("tools on chat completions for %s: unsupported=%v want %v", model, got, want)
		}
	}
	if (&Adapter{API: APICompatible, BaseURL: "https://api.example.com/v1"}).ToolsUnsupported("gpt-6.1-sol") {
		t.Error("the OpenAI table is about OpenAI's own host")
	}
	// through Chat, with OpenAI's settings at a test address: no request goes out
	var calls atomic.Int32
	a := testAdapter(t, APIOpenAI, func(w http.ResponseWriter, r *http.Request) { calls.Add(1) })
	h, _ := HostByID("openai")
	h.ID, h.BaseURL = "openai-test", a.BaseURL
	old := hostsData.Hosts
	hostsData.Hosts = append(append([]Host(nil), old...), h)
	t.Cleanup(func() { hostsData.Hosts = old })
	_, err := a.Chat(context.Background(), ChatRequest{Model: "gpt-6.1-sol", Messages: []Message{{Role: "user", Content: "x"}}, Tools: []Tool{weatherTool}})
	if !errors.Is(err, ErrToolsUnsupported) || calls.Load() != 0 {
		t.Fatalf("no request for tools that would be refused: %v (%d)", err, calls.Load())
	}
}
