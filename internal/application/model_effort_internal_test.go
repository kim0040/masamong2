package application

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/providers/openai"
)

// effortHost records the reasoning_effort and output limit of every chat request.
type effortHost struct {
	mu   sync.Mutex
	sent []map[string]any
}

func (h *effortHost) server(t *testing.T) string {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/chat/completions") {
			http.NotFound(w, r)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		if body["model"] == "fail-primary" { // a primary that is down: its fallback answers
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error":{"message":"upstream unavailable"}}`))
			return
		}
		h.mu.Lock()
		h.sent = append(h.sent, body)
		h.mu.Unlock()
		_, _ = w.Write([]byte(`{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":3,"completion_tokens":1,"cost":0}}`))
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

func (h *effortHost) last() map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.sent[len(h.sent)-1]
}

func effortService(t *testing.T, url string, edit func(d *config.Document)) *Service {
	t.Helper()
	t.Setenv("FAKE_EFFORT_KEY", "fixture-only")
	s := privacyService(t)
	r := s.mutateConfig(action.Request{}, func(d *config.Document) error {
		d.Providers["host"] = config.Provider{Kind: config.KindOpenAI, API: "compatible", BaseURL: url, CredentialRef: "env:FAKE_EFFORT_KEY"}
		d.Providers["spare"] = config.Provider{Kind: config.KindOpenAI, API: "compatible", BaseURL: url + "/spare", CredentialRef: "env:FAKE_EFFORT_KEY"}
		for _, role := range []string{"response", "planner", "summary"} {
			d.Models[role] = config.Model{Provider: "host", Model: "m-" + role}
		}
		edit(d)
		return nil
	})
	if !r.OK {
		t.Fatal(r.Error)
	}
	return s
}

// The precedence of models.<role>.effort: a role's calls ask for its level (empty: low); a quick
// classifier asks for none unless the level is set, and then for that level capped at low, with room
// to think.
func TestRoleEffortPrecedenceOnChatCalls(t *testing.T) {
	h := &effortHost{}
	url := h.server(t)
	for _, c := range []struct {
		effort           string
		plain, quick     string
		quickRoomAddedTo float64
	}{{"", "low", "none", 0}, {"low", "low", "low", 1024}, {"medium", "medium", "low", 1024}, {"none", "none", "none", 0}, {"minimal", "minimal", "minimal", 0},
		{"xhigh", "xhigh", "low", 1024}, {"max", "max", "low", 1024}} {
		s := effortService(t, url, func(d *config.Document) {
			m := d.Models["planner"]
			m.Effort = c.effort
			d.Models["planner"] = m
		})
		routes, err := s.chatRoutes(s.Doc())
		if err != nil {
			t.Fatal(err)
		}
		llm := chatLLM{s: s, routes: routes}
		if _, err := llm.ask(context.Background(), routes["planner"], "planner", []chat.Msg{{Role: "user", Content: "x"}}, 60, 0, false); err != nil {
			t.Fatal(err)
		}
		if got := h.last()["reasoning_effort"]; got != c.plain {
			t.Fatalf("effort %q: a planner call asks %v want %s", c.effort, got, c.plain)
		}
		if _, err := llm.ask(withQuickAnswer(context.Background()), routes["planner"], "planner", []chat.Msg{{Role: "user", Content: "x"}}, 60, 0, false); err != nil {
			t.Fatal(err)
		}
		if got, limit := h.last()["reasoning_effort"], h.last()["max_tokens"]; got != c.quick || limit != 60+c.quickRoomAddedTo {
			t.Fatalf("effort %q: a quick call asks %v (limit %v) want %s (%v)", c.effort, got, limit, c.quick, 60+c.quickRoomAddedTo)
		}
	}
}

// A fallback's own effort replaces the role's on that model; without one it keeps the role's. Quick
// calls (RoleEffort) take either capped at low; the summary role's own level is the job's and stays.
func TestFallbackEffortIsWired(t *testing.T) {
	h := &effortHost{}
	url := h.server(t)
	s := effortService(t, url, func(d *config.Document) {
		m := d.Models["response"]
		m.Effort = "medium"
		m.Fallbacks = []config.ModelFallback{{Provider: "spare", Model: "fb-1", Effort: "high"}, {Provider: "spare", Model: "fb-2"}}
		d.Models["response"] = m
	})
	a, err := s.llmAdapterFor(s.Doc(), "response")
	if err != nil {
		t.Fatal(err)
	}
	if a.RoleEffort != "low" || a.Failover == nil || len(a.Failover.Fallbacks) != 2 {
		t.Fatalf("adapter %q %+v", a.RoleEffort, a.Failover)
	}
	fb := a.Failover.Fallbacks
	if fb[0].Effort != "high" || fb[0].A.RoleEffort != "low" || fb[1].Effort != "" || fb[1].A.RoleEffort != "low" {
		t.Fatalf("fallback efforts: %q/%q %q/%q", fb[0].Effort, fb[0].A.RoleEffort, fb[1].Effort, fb[1].A.RoleEffort)
	}
	s2 := effortService(t, url, func(d *config.Document) {
		m := d.Models["summary"]
		m.Effort = "high"
		m.Fallbacks = []config.ModelFallback{{Provider: "spare", Model: "fb-s"}}
		d.Models["summary"] = m
	})
	if e, err := s2.llmAdapterFor(s2.Doc(), "summary"); err != nil || e.RoleEffort != "high" || e.Failover.Fallbacks[0].A.RoleEffort != "high" {
		t.Fatalf("the summary role's own level is the job's: %v %v", e, err)
	}
	if a.Caps == nil || a.Caps != s.modelCaps("host", s.Doc().Providers["host"]) || fb[0].A.Caps != s.modelCaps("spare", s.Doc().Providers["spare"]) {
		t.Fatal("adapters share their provider's capability cache")
	}
	// a role without an effort leaves its Quick calls thinking-free
	if e, err := s.llmAdapterFor(s.Doc(), "summary"); err != nil || e.RoleEffort != "" {
		t.Fatalf("summary without effort: %q %v", e.RoleEffort, err)
	}
}

// Feature write-ups follow the role's effort; the summary job and fortunes are Quick (none unless the
// summary role's own effort is set), also when the summary falls back to the response model.
func TestFeatureTextEffort(t *testing.T) {
	h := &effortHost{}
	url := h.server(t)
	s := effortService(t, url, func(d *config.Document) {
		delete(d.Models, "summary")
		m := d.Models["response"]
		m.Effort = "high"
		d.Models["response"] = m
	})
	if _, _, err := s.featureText(context.Background(), "response", "notice", "", "x", 100, 0); err != nil {
		t.Fatal(err)
	}
	if h.last()["reasoning_effort"] != "high" {
		t.Fatalf("a feature on the response role: %v", h.last())
	}
	if _, _, err := s.featureText(context.Background(), "summary", "summary", "", "x", 100, 0); err != nil {
		t.Fatal(err)
	}
	if h.last()["reasoning_effort"] != "none" || h.last()["max_tokens"] != float64(100) {
		t.Fatalf("the summary job on the response model stays thinking-free: %v", h.last())
	}
	s = effortService(t, url, func(d *config.Document) {
		m := d.Models["summary"]
		m.Effort = "low"
		d.Models["summary"] = m
	})
	if _, _, err := s.featureText(context.Background(), "summary", "summary", "", "x", 100, 0); err != nil {
		t.Fatal(err)
	}
	if h.last()["reasoning_effort"] != "low" || h.last()["max_tokens"] != float64(100+1024) {
		t.Fatalf("models.summary.effort set: that level with room: %v", h.last())
	}
	// the response model fails and its fallback answers: a fallback without an effort of its own asks
	// what the job asks (the summary role sets none: thinking-free; a fortune: the response role's level
	// capped at low), not the response role's high; one with its own effort keeps it
	s = effortService(t, url, func(d *config.Document) {
		delete(d.Models, "summary")
		m := d.Models["response"]
		m.Model, m.Effort = "fail-primary", "high"
		m.Fallbacks = []config.ModelFallback{{Provider: "spare", Model: "fb-inherit"}}
		d.Models["response"] = m
	})
	for _, c := range []struct{ role, kind, want string }{{"summary", "summary", "none"}, {"response", "fortune", "low"}} {
		if _, _, err := s.featureText(context.Background(), c.role, c.kind, "", "x", 100, 0); err != nil {
			t.Fatal(err)
		}
		if got := h.last(); got["model"] != "fb-inherit" || got["reasoning_effort"] != c.want {
			t.Fatalf("%s on the fallback: %v", c.kind, got)
		}
	}
	s = effortService(t, url, func(d *config.Document) {
		delete(d.Models, "summary")
		m := d.Models["response"]
		m.Model, m.Effort = "fail-primary", "high"
		m.Fallbacks = []config.ModelFallback{{Provider: "spare", Model: "fb-own", Effort: "minimal"}}
		d.Models["response"] = m
	})
	if _, _, err := s.featureText(context.Background(), "summary", "summary", "", "x", 100, 0); err != nil {
		t.Fatal(err)
	}
	if got := h.last(); got["model"] != "fb-own" || got["reasoning_effort"] != "minimal" {
		t.Fatalf("a fallback's own effort stays: %v", got)
	}
}

// F: the memory engine writes summaries on the response model when no summary model is set. Like a
// summary through featureText, a response fallback without an effort of its own writes them at the
// summary job's level (none while models.summary.effort is empty), not at the response role's high; one
// with its own effort uses it capped at low (a Quick call of the response role).
func TestEngineSummaryFallbackEffort(t *testing.T) {
	h := &effortHost{}
	url := h.server(t)
	s := effortService(t, url, func(d *config.Document) {
		delete(d.Models, "summary")
		m := d.Models["response"]
		m.Effort = "high"
		m.Fallbacks = []config.ModelFallback{{Provider: "spare", Model: "fb-inherit"}, {Provider: "spare", Model: "fb-own", Effort: "max"}}
		d.Models["response"] = m
		if e := d.Models["embed"]; e.Provider == "host" {
			t.Fatal("the summary adapter is the response model's only when embed is elsewhere")
		}
	})
	eng, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	a := eng.SummaryLLM
	if a == nil || a.Failover == nil || len(a.Failover.Fallbacks) != 2 {
		t.Fatalf("summary adapter %+v", a)
	}
	if fb := a.Failover.Fallbacks; a.RoleEffort != "" || fb[0].A.RoleEffort != "" || fb[1].A.RoleEffort != "low" {
		t.Fatalf("summary efforts: primary %q, inheriting %q, own %q", a.RoleEffort, fb[0].A.RoleEffort, fb[1].A.RoleEffort)
	}
}

// The status action lists what is known about each chat role's model; capabilities a list gave are kept
// with the price list and come back after a restart.
func TestModelReasoningRows(t *testing.T) {
	h := &effortHost{}
	url := h.server(t)
	s := effortService(t, url, func(d *config.Document) {})
	yes := true
	s.storePrices("host", []openai.Model{{ID: "m-planner", Reasoning: &yes, Efforts: []string{"low", "medium", "high"}, Thinking: []string{"adaptive", "disabled"}, PromptPrice: "0.000001", CompletionPrice: "0.000002"}})
	capsStates.Delete(s) // a new process: only the stored list is left
	rows := s.modelReasoning()
	got, _ := rows["host/m-planner"].(map[string]any)
	if got["reasoning"] != true || strings.Join(got["efforts"].([]string), ",") != "none,minimal,low,medium,high" || got["thinking_off"] != "disabled" {
		t.Fatalf("planner row %v", got)
	}
	if r, _ := rows["host/m-response"].(map[string]any); len(r) != 0 {
		t.Fatalf("an unknown model has an empty row: %v", r)
	}
	// outside Anthropic the list decides about none too: a model that must think offers no none
	s.storePrices("host", []openai.Model{{ID: "m-planner", Reasoning: &yes, Efforts: []string{"low", "medium", "high", "xhigh", "max"}}})
	capsStates.Delete(s)
	got, _ = s.modelReasoning()["host/m-planner"].(map[string]any)
	if strings.Join(got["efforts"].([]string), ",") != "low,medium,high,xhigh,max" || got["thinking_off"] != nil {
		t.Fatalf("a model that must think: %v", got)
	}
}

// E: a known-empty list (a Claude model that takes no effort field: Claude Haiku 4.5) survives the stored
// price list, so after a restart the model still reads as "no effort field" (empty), not "unknown" (nil,
// which would send a guessed effort field and need a refused call to learn); an unknown list stays unknown.
func TestStoredCapsKeepKnownEmptyLists(t *testing.T) {
	h := &effortHost{}
	url := h.server(t)
	s := effortService(t, url, func(d *config.Document) {})
	yes := true
	s.storePrices("host", []openai.Model{
		{ID: "m-planner", Reasoning: &yes, Efforts: []string{}, Thinking: []string{"enabled", "disabled"}, PromptPrice: "0.000001", CompletionPrice: "0.000005"},
		{ID: "m-response", Reasoning: &yes, PromptPrice: "0.000001", CompletionPrice: "0.000005"},
	})
	capsStates.Delete(s) // a new process: only the stored list is left
	cache := s.modelCaps("host", s.Doc().Providers["host"])
	c, known := cache.Get("m-planner")
	if !known || c.Efforts == nil || len(c.Efforts) != 0 || strings.Join(c.Thinking, ",") != "enabled,disabled" {
		t.Fatalf("restored capabilities: %+v (efforts nil: %v)", c, c.Efforts == nil)
	}
	if c, _ := cache.Get("m-response"); c.Efforts != nil || c.Thinking != nil || c.Reasoning == nil || !*c.Reasoning {
		t.Fatalf("unknown lists stay unknown: %+v", c)
	}
}

// D: the console offers what the application can send, not only the native effort field. A Claude model
// that thinks with a fixed budget (Claude Haiku 4.5: no effort field, thinking enabled/disabled) gets
// every level, low and up being budgets of their own (anthropic.go thinkingRoom); native_efforts keeps
// what the model's own field takes (none here). A model with the effort field is unchanged.
func TestBudgetModelRowOffersEveryLevel(t *testing.T) {
	yes := true
	row := reasoningRow(openai.Caps{Reasoning: &yes, Efforts: []string{}, Thinking: []string{"enabled", "disabled"}})
	native, hasNative := row["native_efforts"].([]string)
	if strings.Join(row["efforts"].([]string), ",") != "none,minimal,low,medium,high,xhigh,max" || !hasNative || native == nil || len(native) != 0 || row["thinking_off"] != "disabled" {
		t.Fatalf("budget model row %v", row)
	}
	row = reasoningRow(openai.Caps{Reasoning: &yes, Efforts: []string{"low", "medium", "high"}, Thinking: []string{"adaptive", "disabled"}})
	if strings.Join(row["efforts"].([]string), ",") != "none,minimal,low,medium,high" || strings.Join(row["native_efforts"].([]string), ",") != "low,medium,high" {
		t.Fatalf("effort-field model row %v", row)
	}
}

// GPT-6 Sol on OpenAI's own host: no list says anything, the docs say it takes no none.
func TestReasoningRowFromHostDocs(t *testing.T) {
	c := openai.Caps{Efforts: openai.HostEfforts("https://api.openai.com/v1", "gpt-6.1-sol")}
	row := reasoningRow(c)
	if strings.Join(row["efforts"].([]string), ",") != "low,medium,high,xhigh" {
		t.Fatalf("row %v", row)
	}
	if openai.HostEfforts("https://api.example.com/v1", "gpt-6.1-sol") != nil {
		t.Fatal("the docs are about OpenAI's own host")
	}
}

// An Anthropic-format host of another company that answers /messages only: the provider test passes
// (the list is "unavailable"), models.list says so instead of failing, and the key is proven by a
// one-token answer once a role uses the host.
func TestAnthropicFormatHostWithoutModelList(t *testing.T) {
	var mu sync.Mutex
	var chats int
	var headers []http.Header
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		headers = append(headers, r.Header.Clone())
		mu.Unlock()
		if r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/anthropic/v1/messages") {
			mu.Lock()
			chats++
			mu.Unlock()
			_, _ = w.Write([]byte(`{"id":"m","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"max_tokens","usage":{"input_tokens":3,"output_tokens":1}}`))
			return
		}
		w.WriteHeader(404)
		_, _ = w.Write([]byte(`<html>not here</html>`))
	}))
	t.Cleanup(srv.Close)
	t.Setenv("FAKE_VENDOR_KEY", "fixture-only")
	s := privacyService(t)
	if r := s.mutateConfig(action.Request{}, func(d *config.Document) error {
		d.Providers["vendor"] = config.Provider{Kind: config.KindOpenAI, API: "anthropic", BaseURL: srv.URL + "/anthropic", CredentialRef: "env:FAKE_VENDOR_KEY"}
		return nil
	}); !r.OK {
		t.Fatal(r.Error)
	}
	r := widthExecute(s, "providers.test", map[string]any{"id": "vendor"})
	if !r.OK || r.Output["model_list"] != "unavailable" || r.Output["key_check"] != "unverified" {
		t.Fatalf("test of a host without a list: %+v", r)
	}
	r = widthExecute(s, "models.list", map[string]any{"provider": "vendor"})
	if !r.OK || r.Output["status"] != "no_list" {
		t.Fatalf("models.list: %+v", r)
	}
	if r := s.mutateConfig(action.Request{}, func(d *config.Document) error {
		d.Models["response"] = config.Model{Provider: "vendor", Model: "vendor-chat"}
		return nil
	}); !r.OK {
		t.Fatal(r.Error)
	}
	r = widthExecute(s, "providers.test", map[string]any{"id": "vendor"})
	if !r.OK || r.Output["key_check"] != "chat" || chats != 1 {
		t.Fatalf("the key proven by one answer: %+v (%d chats)", r, chats)
	}
	for _, h := range headers {
		if h.Get("x-api-key") != "fixture-only" || h.Get("Authorization") != "Bearer fixture-only" {
			t.Fatalf("a third-party host gets both headers: %v", h)
		}
	}
}
