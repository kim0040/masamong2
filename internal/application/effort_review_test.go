package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
	"masamong/internal/tools"
)

// Regression tests for the effort-control review (2026-11-03, round 1), at the service level.

// anthropicLoopHost is an Anthropic-format host for the tool loop: a request with tools gets the next
// scripted native answer (a signed thinking block and a tool call), any other request a text answer.
type anthropicLoopHost struct {
	mu       sync.Mutex
	script   []string // tool_use inputs, one round each
	toolReqs []map[string]any
	others   int
}

func (f *anthropicLoopHost) start(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/v1/messages") {
			http.NotFound(w, r)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		usage := `"usage":{"input_tokens":10,"output_tokens":5,"cache_creation_input_tokens":0,"cache_read_input_tokens":0}`
		if _, ok := body["tools"]; !ok || len(f.script) == 0 {
			f.others++
			_, _ = w.Write([]byte(`{"id":"msg_t","type":"message","role":"assistant","model":"claude-loop","content":[{"type":"text","text":"알겠어"}],"stop_reason":"end_turn",` + usage + `}`))
			return
		}
		f.toolReqs = append(f.toolReqs, body)
		n := len(f.toolReqs)
		input := f.script[0]
		f.script = f.script[1:]
		sig := "sig-round-" + string(rune('0'+n))
		_, _ = w.Write([]byte(`{"id":"msg_` + sig + `","type":"message","role":"assistant","model":"claude-loop","content":[` +
			`{"type":"thinking","thinking":"round ` + string(rune('0'+n)) + `","signature":"` + sig + `"},` +
			`{"type":"tool_use","id":"toolu_` + string(rune('0'+n)) + `","name":"weather__query","input":` + input + `}],"stop_reason":"tool_use",` + usage + `}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A: on a model that binds its thinking to the tool list (Anthropic's preserved thinking), the loop
// sends the first round's tools and system on every round. A tool closed during the turn (failed twice)
// stays on the wire list, and a call to it is answered with a refusal instead; every earlier thinking
// block goes back unchanged.
func TestAnthropicToolLoopKeepsToolsAndSystem(t *testing.T) {
	f := &anthropicLoopHost{script: []string{`{"region":"부산"}`, `{"region":"서울"}`, `{"region":"대구"}`}}
	srv := f.start(t)
	svc := setupSvc(t)
	t.Setenv("FAKE_CLAUDE_KEY", "k")
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["claude"] = config.Provider{Kind: config.KindOpenAI, API: "anthropic", BaseURL: srv.URL + "/v1", CredentialRef: "env:FAKE_CLAUDE_KEY"}
		d.Providers["openmeteo"] = config.Provider{Kind: config.KindOpenMeteo, Disabled: true} // the weather answers "unconfigured"
		d.Models["planner"] = config.Model{Provider: "claude", Model: "claude-loop"}
		d.Models["response"] = config.Model{Provider: "claude", Model: "claude-loop"}
		d.Memory.Retrieve = "off"
		d.Tools = map[string]tools.Settings{"search.web": {Chat: boolp(false)}}
		d.Chat.ToolMode = "loop"
	})
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "c", "persona_ref": "friendly_ko"}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "부산 날씨 어때?",
		Speaker: "영희", SpeakerID: "u1", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(f.toolReqs) != 3 {
		t.Fatalf("three tool rounds, got %d", len(f.toolReqs))
	}
	first := f.toolReqs[0]
	for i, req := range f.toolReqs[1:] {
		if !reflect.DeepEqual(req["tools"], first["tools"]) {
			t.Fatalf("round %d tools changed:\n%v\nwant\n%v", i+2, req["tools"], first["tools"])
		}
		if !reflect.DeepEqual(req["system"], first["system"]) {
			t.Fatalf("round %d system changed", i+2)
		}
	}
	// round 3 sends rounds 1 and 2 back with their thinking as it came
	raw, _ := json.Marshal(f.toolReqs[2]["messages"])
	for _, sig := range []string{`"signature":"sig-round-1"`, `"signature":"sig-round-2"`} {
		if !strings.Contains(string(raw), sig) {
			t.Fatalf("round 3 lost %s: %s", sig, raw)
		}
	}
	body := turnBody(t, svc, res.TurnID)
	loop, _ := body["tool_loop"].(map[string]any)
	calls, _ := loop["calls"].([]any)
	if len(calls) != 3 || !reflect.DeepEqual(loop["removed"], []any{"weather.query"}) {
		t.Fatalf("trace %v", loop)
	}
	if last := calls[2].(map[string]any); last["state"] != "refused" || last["reason"] != "removed" {
		t.Fatalf("a call to the closed tool is refused, not offered away: %v", last)
	}
}

// B: gemini-embedding-2 joined the MRL catalog after installs had embedded their messages at its full
// width. Such an install keeps that width (its stored space): no new space, no embedding call.
func TestMRLCatalogUpgradeKeepsStoredMessageWidth(t *testing.T) {
	svc := setupSvc(t)
	t.Setenv("MASAMONG_OPENROUTER_KEY", "synthetic-test-key")
	applyDoc(t, svc, func(d *config.Document) {
		m := d.Models["embed"]
		m.Model, m.Dimensions = "gemini-embedding-2", 0
		d.Models["embed"] = m
	})
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		data := make([]map[string]any, len(req.Input))
		for i := range req.Input {
			v := make([]float32, 3072)
			v[i%3072] = 1
			data[i] = map[string]any{"index": i, "embedding": v}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	t.Cleanup(srv.Close)
	t.Setenv("MESSAGE_VECTOR_FAKE_KEY", "synthetic")
	cred, err := providers.Resolve("env:MESSAGE_VECTOR_FAKE_KEY")
	if err != nil {
		t.Fatal(err)
	}
	old, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	old.Models.EmbedTruncatable = false // the catalog before: the model kept its full width everywhere
	old.LLM = openai.New(providers.NewClient(5*time.Second, nil), srv.URL, cred)
	old.Settings.Summaries = false
	for _, room := range []string{"synthetic:a", "synthetic:b"} {
		if _, err := old.Store.InsertObservations([]corpus.Observation{{ObservationID: room + "#0", Platform: "discord", Collection: "synthetic", SourceRoom: room, ActorLabel: "synthetic",
			Content: "synthetic message for the width upgrade", ParsedTime: time.Now().Add(-48 * time.Hour).UTC().Format(time.RFC3339), RecordKind: corpus.HumanText}}, nil); err != nil {
			t.Fatal(err)
		}
	}
	ctx := context.Background()
	if rep, err := old.IndexRooms(ctx, engine.IndexOptions{}, nil); err != nil || rep.MessageVectors != 2 {
		t.Fatalf("the old install embeds its messages: %+v %v", rep, err)
	}
	stored := "gemini-embedding-2:3072:l2"
	if total, in, err := old.Store.MessageVectorCounts(stored); err != nil || total != 2 || in != 2 {
		t.Fatalf("stored at the full width: %d/%d %v", in, total, err)
	}

	// the update: a new engine reads the catalog that names the model as cuttable
	off, on := false, true
	applyDoc(t, svc, func(d *config.Document) { d.Memory.MessageVectors = &off })
	applyDoc(t, svc, func(d *config.Document) { d.Memory.MessageVectors = &on })
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if eng == old || !eng.Models.EmbedTruncatable {
		t.Fatalf("a new engine with the new catalog: %+v", eng.Models)
	}
	eng.LLM = openai.New(providers.NewClient(5*time.Second, nil), srv.URL, cred)
	eng.Settings.Summaries = false
	before := calls.Load()
	if _, err := eng.IndexRooms(ctx, engine.IndexOptions{}, nil); err != nil {
		t.Fatal(err)
	}
	if got := calls.Load() - before; got != 0 || eng.MessageSpace() != stored || eng.MessageVectorDims() != 3072 {
		t.Fatalf("the install keeps its width: %d embedding calls, space %s", got, eng.MessageSpace())
	}
	if total, in, err := eng.Store.MessageVectorCounts(eng.MessageSpace()); err != nil || total != 2 || in != 2 {
		t.Fatalf("search reads every stored vector: %d/%d %v", in, total, err)
	}
	feat, _ := exec(svc, "runtime.doctor", nil).Output["features"].(map[string]any)["message_vectors"].(map[string]any)
	if feat == nil || feat["status"] != "ready" || !strings.Contains(feat["reason"].(string), "message_dims:3072") {
		t.Fatalf("doctor: %v", feat)
	}
	// a new install of the same model starts at the default width
	fresh := setupSvc(t)
	applyDoc(t, fresh, func(d *config.Document) {
		m := d.Models["embed"]
		m.Model, m.Dimensions = "gemini-embedding-2", 0
		d.Models["embed"] = m
	})
	if e, err := fresh.MemoryEngine(); err != nil || e.MessageVectorDims() != 512 {
		t.Fatalf("a new install: %v", err)
	}
}

// capsHost is an Anthropic-format host with a model list (one budget-thinking model, Claude Haiku 4.5's
// capabilities) and a per-model lookup for another; it records every GET.
type capsHost struct {
	mu   sync.Mutex
	gets []string
	hold chan struct{} // when set, the lookup of claude-x answers once it is closed
}

func (h *capsHost) start(t *testing.T) *httptest.Server {
	const budget = `{"type":"model","id":"claude-budget","display_name":"Budget","max_input_tokens":200000,"capabilities":{` +
		`"effort":{"supported":false,"low":{"supported":false},"medium":{"supported":false},"high":{"supported":false},"xhigh":{"supported":false},"max":{"supported":false}},` +
		`"thinking":{"supported":true,"types":{"enabled":{"supported":true},"adaptive":{"supported":false},"disabled":{"supported":true},"between_tools":{"supported":false}}}}}`
	const adaptive = `{"type":"model","id":"claude-x","display_name":"X","max_input_tokens":1000000,"capabilities":{` +
		`"effort":{"supported":true,"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},"xhigh":{"supported":true},"max":{"supported":true}},` +
		`"thinking":{"supported":true,"types":{"enabled":{"supported":false},"adaptive":{"supported":true},"disabled":{"supported":true},"between_tools":{"supported":false}}}}}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet {
			h.mu.Lock()
			h.gets = append(h.gets, r.URL.Path)
			h.mu.Unlock()
		}
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models":
			_, _ = w.Write([]byte(`{"data":[` + budget + `],"has_more":false}`))
		case r.Method == http.MethodGet && r.URL.Path == "/v1/models/claude-x":
			if h.hold != nil {
				<-h.hold
			}
			_, _ = w.Write([]byte(adaptive))
		case r.Method == http.MethodPost && r.URL.Path == "/v1/messages":
			_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","model":"claude-x","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":1}}`))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (h *capsHost) lookups() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []string
	for _, p := range h.gets {
		if strings.HasPrefix(p, "/v1/models/") {
			out = append(out, p)
		}
	}
	return out
}

// G: what a model can do comes from the model list and the operator's provider test, never from a status
// read: providers.list asks the host nothing about a model, the provider test asks once about each role
// model the list did not cover, and a second test asks nothing more.
// D: a model that thinks with a fixed budget (no effort field, thinking enabled/disabled) is offered every
// level by models.list and providers.list, low and up being budgets of their own; its native effort
// field list stays empty.
func TestModelCapsComeFromListsAndTests(t *testing.T) {
	h := &capsHost{}
	srv := h.start(t)
	svc := setupSvc(t)
	t.Setenv("FAKE_CLAUDE_KEY", "k")
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["claude"] = config.Provider{Kind: config.KindOpenAI, API: "anthropic", BaseURL: srv.URL + "/v1", CredentialRef: "env:FAKE_CLAUDE_KEY"}
		d.Models["planner"] = config.Model{Provider: "claude", Model: "claude-x"}
		d.Models["response"] = config.Model{Provider: "claude", Model: "claude-budget"}
	})
	if r := exec(svc, "providers.list", nil); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	time.Sleep(300 * time.Millisecond) // a background lookup would have run by now
	if got := h.lookups(); len(got) != 0 {
		t.Fatalf("a status read asks the host about models: %v", got)
	}
	r := exec(svc, "models.list", map[string]any{"provider": "claude"})
	rows, _ := r.Output["models"].([]any)
	if !r.OK || len(rows) != 1 {
		t.Fatalf("models.list %+v", r)
	}
	row, _ := rows[0].(map[string]any)
	efforts, _ := row["efforts"].([]string)
	native, _ := row["native_efforts"].([]string)
	if strings.Join(efforts, ",") != "none,minimal,low,medium,high,xhigh,max" || native == nil || len(native) != 0 || row["thinking_off"] != "disabled" {
		t.Fatalf("the budget model's row: %v", row)
	}
	for i := 0; i < 2; i++ {
		if r := exec(svc, "providers.test", map[string]any{"id": "claude"}); !r.OK {
			t.Fatalf("%+v", r.Error)
		}
		waitLookups(t, h, 1) // the lookup runs after the test's answer
	}
	// once the first lookup is recorded, the lookup a test starts asks nothing (run here, not waited for)
	var status map[string]any
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(10 * time.Millisecond) {
		status, _ = exec(svc, "providers.list", nil).Output["model_reasoning"].(map[string]any)
		if x, _ := status["claude/claude-x"].(map[string]any); x["thinking_off"] != nil || time.Now().After(deadline) {
			break
		}
	}
	application.LookupModelCapsForTest(svc, context.Background(), "claude")
	if got := h.lookups(); !reflect.DeepEqual(got, []string{"/v1/models/claude-x"}) {
		t.Fatalf("one lookup, for the role model the list did not cover: %v", got)
	}
	x, _ := status["claude/claude-x"].(map[string]any)
	b, _ := status["claude/claude-budget"].(map[string]any)
	if x["thinking_off"] != "disabled" || strings.Join(b["efforts"].([]string), ",") != "none,minimal,low,medium,high,xhigh,max" {
		t.Fatalf("the status shows what the list and the test found: %v %v", x, b)
	}
}

// waitLookups waits until h has answered n model lookups (they run in the background).
func waitLookups(t *testing.T, h *capsHost, n int) {
	t.Helper()
	for deadline := time.Now().Add(5 * time.Second); len(h.lookups()) < n; {
		if time.Now().After(deadline) {
			t.Fatalf("%d lookups, want %d", len(h.lookups()), n)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Effort review round 2, finding 7: providers.test answers before the role models are looked up; the
// lookup runs in the background and its result reaches the status afterwards.
func TestProviderTestDoesNotWaitForTheCapsLookup(t *testing.T) {
	h := &capsHost{hold: make(chan struct{})}
	srv := h.start(t)
	released := false
	release := func() {
		if !released {
			released = true
			close(h.hold)
		}
	}
	t.Cleanup(release) // before the server closes (cleanups run last first)
	svc := setupSvc(t)
	t.Setenv("FAKE_CLAUDE_KEY", "k")
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["claude"] = config.Provider{Kind: config.KindOpenAI, API: "anthropic", BaseURL: srv.URL + "/v1", CredentialRef: "env:FAKE_CLAUDE_KEY"}
		d.Models["planner"] = config.Model{Provider: "claude", Model: "claude-x"}
	})
	done := make(chan bool, 1)
	go func() { done <- exec(svc, "providers.test", map[string]any{"id": "claude"}).OK }()
	select {
	case ok := <-done:
		if !ok {
			t.Fatal("providers.test failed")
		}
	case <-time.After(2 * time.Second):
		release()
		t.Fatal("providers.test waited for the model lookup")
	}
	waitLookups(t, h, 1) // asked, not answered yet
	release()
	for deadline := time.Now().Add(5 * time.Second); ; {
		status, _ := exec(svc, "providers.list", nil).Output["model_reasoning"].(map[string]any)
		if x, _ := status["claude/claude-x"].(map[string]any); x["thinking_off"] == "disabled" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the lookup's answer reaches the status: %v", status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
