package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
)

// Regression tests for the effort-control review (2026-11-03, round 3), at the service level.

// noThinkingHost is an Anthropic-format host whose route takes no thinking field; it has no model list
// and looks no model up, so nothing but a refusal or a reset changes what is learned. It counts chats.
type noThinkingHost struct {
	mu     sync.Mutex
	bodies []map[string]any
}

func (h *noThinkingHost) start(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v1/messages" {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		h.mu.Lock()
		h.bodies = append(h.bodies, body)
		h.mu.Unlock()
		if body["thinking"] != nil {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"invalid_request_error","message":"thinking: Extra inputs are not permitted"}}`))
			return
		}
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// chats is the number of chat requests one call made, and whether its first one carried thinking.
func (h *noThinkingHost) chats(t *testing.T, a *openai.Adapter) (int, bool) {
	t.Helper()
	h.mu.Lock()
	h.bodies = nil
	h.mu.Unlock()
	req := openai.ChatRequest{Model: "claude-x", Messages: []openai.Message{{Role: "user", Content: "x"}}, MaxTokens: 40, Effort: "low"}
	if _, err := a.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.bodies), len(h.bodies) > 0 && h.bodies[0]["thinking"] != nil
}

// P2-1: what a provider's route refused is learned again after the operator tests the provider, saves
// its key (even under the same reference) or edits any of its settings; an edit of something else keeps it.
func TestProviderEditsDropLearnedShapes(t *testing.T) {
	h := &noThinkingHost{}
	srv := h.start(t)
	svc := setupSvc(t)
	t.Setenv("FAKE_CLAUDE_KEY", "k")
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["claude"] = config.Provider{Kind: config.KindOpenAI, API: "anthropic", BaseURL: srv.URL + "/v1", CredentialRef: "env:FAKE_CLAUDE_KEY"}
		d.Models["planner"] = config.Model{Provider: "claude", Model: "claude-x"}
	})
	cred, _ := providers.Resolve("env:FAKE_CLAUDE_KEY")
	client := providers.NewClient(5*time.Second, nil)
	client.MaxRetries = 0
	adapter := func() *openai.Adapter { // a turn's adapter: the provider's shared cache
		a := openai.New(client, srv.URL+"/v1", cred)
		a.API, a.Caps = openai.APIAnthropic, application.CapsCacheForTest(svc, "claude")
		return a
	}
	learn := func(step string) {
		t.Helper()
		a := adapter()
		if n, _ := h.chats(t, a); n > 2 {
			t.Fatalf("%s: %d chats", step, n)
		}
		if n, thinking := h.chats(t, adapter()); n != 1 || thinking {
			t.Fatalf("%s: the refusal is learned: %d chats, thinking %v", step, n, thinking)
		}
	}
	relearns := func(step string) {
		t.Helper()
		if n, thinking := h.chats(t, adapter()); n != 2 || !thinking {
			t.Fatalf("%s: the shape is learned again: %d chats, thinking %v", step, n, thinking)
		}
	}

	learn("first")
	applyDoc(t, svc, func(d *config.Document) { d.Models["response"] = config.Model{Provider: "claude", Model: "claude-y"} })
	if n, _ := h.chats(t, adapter()); n != 1 {
		t.Fatalf("an edit of another setting keeps the shape: %d chats", n)
	}

	if r := exec(svc, "providers.test", map[string]any{"id": "claude"}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	relearns("after providers.test")

	learn("before the key")
	if r := exec(svc, "providers.credentials", map[string]any{"id": "claude", "credential_ref": "env:FAKE_CLAUDE_KEY"}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	relearns("after providers.credentials")

	learn("before the edit")
	applyDoc(t, svc, func(d *config.Document) {
		p := d.Providers["claude"]
		p.DailyLimit = 500
		d.Providers["claude"] = p
	})
	relearns("after a provider edit")
}

// P3-7: a provider test of an Anthropic-format host says its role models' capabilities are still being
// looked up, so the console reads the status again.
func TestProviderTestSaysCapsPending(t *testing.T) {
	h := &capsHost{}
	srv := h.start(t)
	svc := setupSvc(t)
	t.Setenv("FAKE_CLAUDE_KEY", "k")
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["claude"] = config.Provider{Kind: config.KindOpenAI, API: "anthropic", BaseURL: srv.URL + "/v1", CredentialRef: "env:FAKE_CLAUDE_KEY"}
		d.Models["planner"] = config.Model{Provider: "claude", Model: "claude-x"}
	})
	r := exec(svc, "providers.test", map[string]any{"id": "claude"})
	if !r.OK || r.Output["caps_pending"] != true {
		t.Fatalf("caps_pending after the test: %+v", r)
	}
	waitLookups(t, h, 1)
}
