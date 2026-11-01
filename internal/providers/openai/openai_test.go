package openai

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

func TestParseModelsVariants(t *testing.T) {
	openrouter := `{"data":[{"id":"openai/gpt-5","name":"GPT-5","context_length":400000,"pricing":{"prompt":"0.00000125","completion":"0.00001"}},{"id":"text-embedding-3-small","name":"E","pricing":{"prompt":"0.00000002","completion":"0"}}]}`
	ms, err := ParseModels("application/json", []byte(openrouter))
	if err != nil || len(ms) != 2 || ms[0].ContextLength != 400000 || ms[0].PromptPrice != "0.00000125" || ms[1].Kind != "embedding" {
		t.Fatalf("openrouter: %v %+v", err, ms)
	}
	img := `{"data":[{"id":"google/gemini-2.5-flash-image","context_length":32000,"architecture":{"input_modalities":["text","image"],"output_modalities":["image","text"]}}]}`
	if ms, err := ParseModels("application/json", []byte(img)); err != nil || !ms[0].ImageOutput || ms[0].Kind != "chat" {
		t.Fatalf("image output: %v %+v", err, ms)
	}
	openai := `{"object":"list","data":[{"id":"gpt-4o-mini","object":"model","created":1,"owned_by":"openai"}]}`
	ms, err = ParseModels("application/json", []byte(openai))
	if err != nil || len(ms) != 1 || ms[0].PromptPrice != "" || ms[0].Kind != "chat" {
		t.Fatalf("openai: unknown price must stay empty, not 0: %v %+v", err, ms)
	}
	bare := `[{"id":"local-model"}]`
	ms, err = ParseModels("text/plain", []byte(bare))
	if err != nil || len(ms) != 1 {
		t.Fatalf("bare list: %v", err)
	}
	if _, err := ParseModels("application/json", []byte(`{"error":{"message":"bad key","type":"auth"}}`)); err == nil {
		t.Fatal("error envelope")
	}
}

func TestParseOpenRouterConditionalPricingOverrides(t *testing.T) {
	raw := `{"data":[{"id":"tier-model","pricing":{"prompt":"0.000001","completion":"0.000002","input_cache_read":"0.0000001","overrides":[` +
		`{"min_prompt_tokens":200000,"prompt":"0.000002","completion":"0.000004","input_cache_read":"0.0000002"},` +
		`{"min_prompt_tokens":200000,"completion":"0.000005"},` +
		`{"future_condition":true,"prompt":"0.5"},` +
		`{"utc_start":1630,"utc_end":30,"utc_days":["monday"],"prompt":"0.000003"}]}}]}`
	models, err := ParseModels("application/json", []byte(raw))
	if err != nil || len(models) != 1 || len(models[0].Overrides) != 3 {
		t.Fatalf("pricing overrides: %+v %v", models, err)
	}
	minute := time.Date(2026, 10, 5, 20, 0, 0, 0, time.UTC) // Monday within the wrapping UTC window.
	if models[0].Overrides[0].Applies(200000, minute) || !models[0].Overrides[0].Applies(200001, minute) {
		t.Fatal("min_prompt_tokens must be a strict greater-than condition")
	}
	if !models[0].Overrides[2].Applies(1, minute) || models[0].Overrides[2].Applies(1, minute.Add(-8*time.Hour)) {
		t.Fatal("UTC window and weekday conditions were not applied")
	}
}

func TestParseEmbeddingsRejectsMalformedBatches(t *testing.T) {
	for name, body := range map[string]string{
		"duplicate index":     `{"data":[{"index":1,"embedding":[1,0]},{"index":1,"embedding":[0,1]}]}`,
		"out of range":        `{"data":[{"index":0,"embedding":[1,0]},{"index":2,"embedding":[0,1]}]}`,
		"mixed missing index": `{"data":[{"index":0,"embedding":[1,0]},{"embedding":[0,1]}]}`,
		"missing item":        `{"data":[{"index":0,"embedding":[1,0]}]}`,
		"dimensions":          `{"data":[{"index":0,"embedding":[1,0]},{"index":1,"embedding":[0,1,0]}]}`,
		"empty vector":        `{"data":[{"index":0,"embedding":[1,0]},{"index":1,"embedding":[]}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			result, err := ParseEmbeddings("application/json", []byte(body), 2)
			if !errors.Is(err, decode.ErrUnexpected) || len(result.Vectors) != 0 {
				t.Fatalf("malformed response must return no vectors: %+v %v", result, err)
			}
		})
	}
	result, err := ParseEmbeddings("application/json", []byte(`{"data":[{"index":1,"embedding":[0,1]},{"index":0,"embedding":[1,0]}]}`), 2)
	if err != nil || len(result.Vectors) != 2 || result.Vectors[0][0] != 1 || result.Vectors[1][1] != 1 {
		t.Fatalf("valid shuffled batch: %+v %v", result, err)
	}
	result, err = ParseEmbeddings("application/json", []byte(`{"data":[{"embedding":[1,0]},{"embedding":[0,1]}]}`), 2)
	if err != nil || len(result.Vectors) != 2 || result.Vectors[0][0] != 1 || result.Vectors[1][1] != 1 {
		t.Fatalf("whole positional batch without indices should be accepted: %+v %v", result, err)
	}
}

func TestParseChatVariants(t *testing.T) {
	str := `{"id":"x","model":"m","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":"12","completion_tokens":3}}`
	r, err := ParseChat("application/json", []byte(str))
	if err != nil || r.Content != "hello" || r.PromptTokens != 12 || r.CompletionTokens != 3 {
		t.Fatalf("string content: %v %+v", err, r)
	}
	parts := `{"choices":[{"message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}]}`
	r, err = ParseChat("application/json", []byte(parts))
	if err != nil || r.Content != "ab" {
		t.Fatalf("parts content: %v %+v", err, r)
	}
	if _, err := ParseChat("application/json", []byte(`{"choices":[]}`)); err == nil {
		t.Fatal("empty choices")
	}
	err200 := `{"error":{"message":"insufficient credits","code":402}}`
	var ae *APIError
	if _, err := ParseChat("application/json", []byte(err200)); !errors.As(err, &ae) || ae.Code != "402" {
		t.Fatalf("200 error envelope: %v", err)
	}
}

func TestChatAuthMapping(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer k" {
			t.Errorf("bearer")
		}
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"error":{"message":"No auth credentials found","code":401}}`))
	}))
	defer srv.Close()
	t.Setenv("OA", "k")
	cred, _ := providers.Resolve("env:OA")
	c := providers.NewClient(time.Second, nil)
	c.MaxRetries = 0
	a := New(c, srv.URL, cred)
	_, err := a.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "hi"}}})
	if !errors.Is(err, ErrAuth) {
		t.Fatalf("401 must map to ErrAuth: %v", err)
	}
}

// Hosts other than OpenRouter do not report usage.cost: the adapter prices tokens itself, and a
// reported cost always wins over the list price.
func TestCostFromTokensWhenHostReportsNone(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/chat/completions":
			_, _ = w.Write([]byte(`{"choices":[{"message":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1000,"completion_tokens":500}}`))
		case "/embeddings":
			_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[0.1,0.2]}],"usage":{"total_tokens":2000}}`))
		case "/rerank":
			_, _ = w.Write([]byte(`{"results":[{"index":0,"relevance_score":0.9}],"usage":{"total_tokens":300,"cost":0.5}}`))
		}
	}))
	defer srv.Close()
	t.Setenv("OAI_TEST_KEY", "k")
	cred, _ := providers.Resolve("env:OAI_TEST_KEY")
	a := New(providers.NewClient(2*time.Second, nil), srv.URL, cred)
	a.Price = func(model string) (float64, float64, bool) {
		if model == "unknown" {
			return 0, 0, false
		}
		return 1e-6, 2e-6, true
	}
	res, err := a.Chat(context.Background(), ChatRequest{Model: "m", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil || res.CostUSD == nil || !res.CostEstimated || math.Abs(*res.CostUSD-0.002) > 1e-12 {
		t.Fatalf("chat cost %v %v %v", res.CostUSD, res.CostEstimated, err)
	}
	emb, err := a.Embed(context.Background(), "e", []string{"x"})
	if err != nil || emb.CostUSD == nil || math.Abs(*emb.CostUSD-0.002) > 1e-12 || emb.PromptTokens != 2000 {
		t.Fatalf("embed cost %v tokens %d %v", emb.CostUSD, emb.PromptTokens, err)
	}
	rr, err := a.Rerank(context.Background(), "r", "q", []string{"d"}, 1)
	if err != nil || rr.CostUSD == nil || *rr.CostUSD != 0.5 || rr.CostEstimated {
		t.Fatalf("a reported cost must win: %v %v %v", rr.CostUSD, rr.CostEstimated, err)
	}
	unk, err := a.Chat(context.Background(), ChatRequest{Model: "unknown", Messages: []Message{{Role: "user", Content: "x"}}})
	if err != nil || unk.CostUSD != nil {
		t.Fatalf("an unknown price stays unknown: %v %v", unk.CostUSD, err)
	}
}

// OpenRouter lists models without a key, so a wrong key must be caught by asking for the key itself.
func TestCheckKeyCatchesAWrongOpenRouterKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/models"):
			_, _ = w.Write([]byte(`{"data":[{"id":"m"}]}`)) // served to anyone
		case strings.HasSuffix(r.URL.Path, "/key") && r.Header.Get("Authorization") == "Bearer good":
			_, _ = w.Write([]byte(`{"data":{"label":"k"}}`))
		default:
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"error":{"message":"User not found.","code":401}}`))
		}
	}))
	defer srv.Close()
	c := providers.NewClient(time.Second, nil)
	c.MaxRetries = 0
	for key, wantOK := range map[string]bool{"good": true, "abc": false} {
		t.Setenv("OR", key)
		cred, _ := providers.Resolve("env:OR")
		a := New(c, srv.URL+"/api/v1", cred)
		a.API = APIOpenRouter // the test server is not openrouter.ai; config names the family
		if _, err := a.ListModels(context.Background()); err != nil {
			t.Fatalf("models are public: %v", err)
		}
		err := a.CheckKey(context.Background())
		if wantOK && err != nil {
			t.Errorf("%s: %v", key, err)
		}
		if !wantOK && !errors.Is(err, ErrAuth) {
			t.Errorf("%s: want ErrAuth, got %v", key, err)
		}
	}
}

// Input served from the prompt cache is read in both reported forms and priced at the cache price; a
// model without one is priced as ordinary input.
func TestCachedInputIsPricedAtTheCachePrice(t *testing.T) {
	openaiForm := []byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1000,"completion_tokens":100,"prompt_tokens_details":{"cached_tokens":800}}}`)
	deepseekForm := []byte(`{"choices":[{"message":{"content":"hi"}}],"usage":{"prompt_tokens":1000,"completion_tokens":100,"prompt_cache_hit_tokens":800,"prompt_cache_miss_tokens":200}}`)
	for _, body := range [][]byte{openaiForm, deepseekForm} {
		out, err := ParseChat("application/json", body)
		if err != nil || out.CachedTokens != 800 {
			t.Fatalf("cached tokens: %+v %v", out, err)
		}
	}
	a := &Adapter{
		Price: func(string) (float64, float64, bool) { return 1e-6, 4e-6, true },
		CachedPrice: func(model string) (float64, bool) {
			return 0.1e-6, model == "cached-model"
		},
	}
	var cost *float64
	var est bool
	a.estimateCached(&cost, &est, "cached-model", 1000, 800, 100)
	// 200 × $1/M + 800 × $0.1/M + 100 × $4/M
	if cost == nil || !est || fmt.Sprintf("%.6f", *cost) != "0.000680" {
		t.Fatalf("cached price: %v", cost)
	}
	cost = nil
	a.estimateCached(&cost, &est, "other-model", 1000, 800, 100)
	if cost == nil || fmt.Sprintf("%.6f", *cost) != "0.001400" {
		t.Fatalf("no cache price: input price for all: %v", cost)
	}
	if pub, ok := Published(APIOpenAI, "gpt-5.4"); !ok || pub.Cached*1e6 != 0.25 || pub.In*1e6 != 2.5 {
		t.Fatalf("published cache price: %+v", pub)
	}
}

func TestEstimateUsesContextualLongPromptAndCacheRates(t *testing.T) {
	at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
	seenTokens := 0
	var seenAt time.Time
	a := &Adapter{ContextPrice: func(_ string, promptTokens int, requestAt time.Time) (float64, float64, float64, bool) {
		seenTokens, seenAt = promptTokens, requestAt
		if promptTokens > 200000 {
			return 2e-6, 5e-6, 0.2e-6, true
		}
		return 1e-6, 2e-6, 0.1e-6, true
	}}
	var cost *float64
	var estimated bool
	a.estimateCachedAt(&cost, &estimated, "tier-model", 200001, 50, 100, at)
	want := float64(199951)*2e-6 + float64(50)*0.2e-6 + float64(100)*5e-6
	if cost == nil || !estimated || math.Abs(*cost-want) > 1e-12 || seenTokens != 200001 || !seenAt.Equal(at) {
		t.Fatalf("contextual estimate=%v want=%v tokens=%d at=%v", cost, want, seenTokens, seenAt)
	}
}
