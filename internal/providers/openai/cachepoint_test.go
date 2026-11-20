package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"masamong/internal/providers"
)

// flaggedHost clones the listed host id onto the test server's address, so a call to it counts as a
// call to that family.
func flaggedHost(t *testing.T, id, baseURL string, breakpoints bool) {
	t.Helper()
	h, ok := HostByID(id)
	if !ok {
		t.Fatalf("hosts.json has no %s", id)
	}
	h.ID, h.BaseURL, h.CacheBreakpoints = id+"-test", baseURL, breakpoints
	old := hostsData.Hosts
	hostsData.Hosts = append(append([]Host(nil), old...), h)
	t.Cleanup(func() { hostsData.Hosts = old })
}

func chatOKServer(t *testing.T, body *map[string]any) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, body)
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","index":0,"message":{"content":"ok","role":"assistant"}}],` +
			`"usage":{"completion_tokens":2,"prompt_tokens":10,"total_tokens":12,"prompt_tokens_details":{"cached_tokens":8,"cache_write_tokens":2}}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func cacheMarks(body map[string]any) (marked int) {
	for _, m := range body["messages"].([]any) {
		parts, _ := m.(map[string]any)["content"].([]any) // unmarked messages have string content
		for _, p := range parts {
			if _, ok := p.(map[string]any)["cache_control"]; ok {
				marked++
			}
		}
	}
	return
}

// A flagged OpenRouter route writes one breakpoint on the marked message; the same message on an
// unflagged host, or to a family that caches implicitly, carries none.
func TestCachePointFollowsTheHostFlag(t *testing.T) {
	req := ChatRequest{Model: "google/gemini-3.8-flash", Messages: []Message{
		{Role: "system", Content: "persona"},
		{Role: "user", Content: "철수 (14:05): 밥 먹었어?"},
		{Role: "assistant", Content: "먹었어!"},
		{Role: "user", Content: "지금 시각: 2026-10-31 15:04 (토요일, Asia/Seoul)\n민수: 나도!"},
	}, MaxTokens: 50}
	req.Messages[2].CachePoint = true // the end of the stable prefix

	var body map[string]any
	url := chatOKServer(t, &body)
	flaggedHost(t, "openrouter", url+"/api/v1", true)
	c := providers.NewClient(5*time.Second, nil)
	c.MaxRetries = 0
	t.Setenv("K", "test-key")
	cred, _ := providers.Resolve("env:K")
	a := New(c, url+"/api/v1", cred)
	a.API = APIOpenRouter
	a.Price = func(string) (float64, float64, bool) { return 1, 2, true }
	a.CachedPrice = func(string) (float64, bool) { return 0.1, true }
	res, err := a.Chat(context.Background(), req)
	if err != nil || res.Content != "ok" {
		t.Fatalf("chat: %v %+v", err, res)
	}
	if cacheMarks(body) != 1 {
		t.Fatalf("one breakpoint on the marked message: %v", body["messages"])
	}
	marked := body["messages"].([]any)[2].(map[string]any)
	if parts := marked["content"].([]any); len(parts) != 1 || parts[0].(map[string]any)["cache_control"] == nil {
		t.Fatalf("the breakpoint is the text block of the marked message: %v", marked)
	}
	if res.CachedTokens != 8 || res.CacheWriteTokens != 2 {
		t.Fatalf("cached and written tokens read: %+v", res)
	}
	// cost = 0 plain inputs + 8 cached at 0.1 + 2 written at 1.25x + 2 out: 0.8 + 2.5 + 4 = 7.3
	if res.CostUSD == nil || !res.CostEstimated || *res.CostUSD != 7.3 {
		t.Fatalf("cache reads and writes priced: %+v", res)
	}

	for _, tc := range []struct{ name, base, model string }{
		{"unflagged host", url + "/other/v1", "google/gemini-3.8-flash"},
		{"implicit family", url + "/api/v1", "openai/gpt-6.2"},
	} {
		body = nil
		if tc.name == "unflagged host" {
			flaggedHost(t, "openrouter", tc.base, false)
		}
		other := New(c, tc.base, cred)
		other.API = APIOpenRouter
		got := req
		got.Model = tc.model
		if _, err = other.Chat(context.Background(), got); err != nil {
			t.Fatalf("%s chat: %v", tc.name, err)
		}
		if cacheMarks(body) != 0 {
			t.Fatalf("%s: no breakpoint may leave the process: %v", tc.name, body["messages"])
		}
	}
}

// A flagged Anthropic host gets the breakpoint on the last content block of the turn before the final
// user turn — the merged history tail — and on the system prompt when there is no history.
func TestAnthropicCachePointEndsThePrefix(t *testing.T) {
	h := &anthropicHost{chat: func(w http.ResponseWriter, body map[string]any) { _, _ = w.Write([]byte(nativeText)) }}
	srv := httptest.NewServer(http.HandlerFunc(h.serve))
	t.Cleanup(srv.Close)
	flaggedHost(t, "anthropic", srv.URL+"/v1", true)
	c := providers.NewClient(5*time.Second, nil)
	c.MaxRetries = 0
	t.Setenv("K", "test-key")
	cred, _ := providers.Resolve("env:K")
	a := New(c, srv.URL+"/v1", cred)
	a.API = APIAnthropic

	req := ChatRequest{Model: "claude-haiku-5-5", Messages: []Message{
		{Role: "system", Content: "persona"},
		{Role: "user", Content: "철수 (14:05): 밥 먹었어?"},
		{Role: "assistant", Content: "먹었어!", CachePoint: true},
		{Role: "user", Content: "지금 시각: 2026-10-31 15:04\n민수: 나도!"},
	}, MaxTokens: 50}
	if _, err := a.Chat(context.Background(), req); err != nil {
		t.Fatalf("chat: %v", err)
	}
	var err error
	body := h.bodies[len(h.bodies)-1]
	msgs := body["messages"].([]any)
	blocks := msgs[len(msgs)-2].(map[string]any)["content"].([]any)
	last := blocks[len(blocks)-1].(map[string]any)
	if last["cache_control"] == nil {
		t.Fatalf("the last history turn's last block carries the breakpoint: %v", msgs)
	}
	if s, ok := body["system"].(string); !ok || s != "persona" {
		t.Fatalf("an unmarked system stays text: %v", body["system"])
	}

	// with no history the breakpoint is the system prompt, which becomes a marked content block
	req.Messages = []Message{
		{Role: "system", Content: "persona", CachePoint: true},
		{Role: "user", Content: "민수: 나도!"},
	}
	if _, err = a.Chat(context.Background(), req); err != nil {
		t.Fatalf("chat without history: %v", err)
	}
	body = h.bodies[len(h.bodies)-1]
	sys, ok := body["system"].([]any)
	if !ok || len(sys) != 1 || sys[0].(map[string]any)["cache_control"] == nil {
		t.Fatalf("the system block carries the breakpoint: %v", body["system"])
	}
}
