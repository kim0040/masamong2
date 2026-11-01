package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/fsutil"
)

func TestPracticeRawMessageSearchAndBlockPrepare(t *testing.T) {
	root := t.TempDir()
	p, err := application.OpenPractice(context.Background(), root, "ko", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	search := exec(p, "messages.search", map[string]any{"query": "텐트", "rooms": []string{"kakao:practice"}, "top_k": 5})
	if !search.OK || search.Output["mode"] != "raw_lexical" || search.Output["cost_usd"] != 0 {
		t.Fatalf("raw message search: %+v", search)
	}
	raw, _ := json.Marshal(search.Output)
	if !strings.Contains(string(raw), "텐트") || strings.Contains(string(raw), "actor_account") {
		t.Fatalf("raw message result/provenance: %s", raw)
	}
	dry := exec(p, "memory.blocks.prepare", map[string]any{"source_room": "kakao:practice", "dry_run": true})
	if !dry.OK || dry.Output["dry_run"] != true || dry.Output["report"] == nil {
		t.Fatalf("block estimate: %+v", dry)
	}
	prepared := exec(p, "memory.blocks.prepare", map[string]any{"source_room": "kakao:practice", "budget_usd": "0"})
	preparedJSON, _ := json.Marshal(prepared.Output["block"])
	if !prepared.OK || !strings.Contains(string(preparedJSON), `"status":"ready"`) {
		t.Fatalf("block prepare: %+v", prepared)
	}
	path := filepath.Join(application.PracticeRoot(root), "memory", "blocks", "index.json")
	if err := fsutil.VerifyPrivateFile(path); err != nil {
		t.Fatalf("block manifest permissions: %v", err)
	}
}

func TestPracticeProviderToolsStayOfflineWithoutKeyWarnings(t *testing.T) {
	p := practiceSvc(t)
	weather := exec(p, "weather.query", nil)
	if !weather.OK || weather.Output["source"] != "fixture" {
		t.Fatalf("practice weather must use its offline fixture: %+v", weather)
	}
	disasters := exec(p, "disasters.query", nil)
	if !disasters.OK || disasters.Output["source"] != "fixture" {
		t.Fatalf("practice disasters must use its offline fixture: %+v", disasters)
	}
	dash := exec(p, "dashboard.overview", nil)
	if !dash.OK {
		t.Fatal(dash.Error)
	}
	raw, _ := json.Marshal(dash.Output["attention"])
	var items []map[string]any
	_ = json.Unmarshal(raw, &items)
	for _, item := range items {
		if item["key"] == "tool_unconfigured" {
			t.Fatalf("practice cannot act on live provider key warnings: %+v", item)
		}
	}
	// the tools page says the same: sample-backed tools are ready, the rest rest in practice (no key
	// or model can be set there), never "missing key"
	state := map[string]string{}
	for _, raw := range exec(p, "tools.list", nil).Output["tools"].([]any) {
		tl := raw.(map[string]any)
		why, _ := tl["ready_reason"].(string)
		if tl["ready"] == true {
			why = "ready"
		}
		state[tl["id"].(string)] = why
	}
	if state["weather.query"] != "ready" || state["disasters.query"] != "ready" || state["search.web"] != "practice_unavailable" ||
		state["finance.quote"] != "practice_unavailable" || state["images.generate"] != "practice_unavailable" || state["chat.respond"] != "ready" {
		t.Fatalf("practice tool readiness: %v", state)
	}
	if r := exec(p, "search.web", map[string]any{"query": "x"}); r.Output["status"] != "unconfigured" {
		t.Fatalf("practice has no search sample, so it answers unconfigured: %+v", r)
	}
}

// Practice mode runs the whole pipeline offline: the sample is imported and indexed, search finds it,
// and a chat turn answers from it without any network call or cost.
func TestPracticeInstanceAnswersOfflineFromTheSample(t *testing.T) {
	root := t.TempDir()
	ctx := context.Background()
	p, err := application.OpenPractice(ctx, root, "ko", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	if !p.Practice() || !p.Ready() {
		t.Fatal("practice instance must be ready")
	}
	run := func(id string, in map[string]any) action.Result {
		b, _ := json.Marshal(in)
		return p.Execute(ctx, action.Request{Action: id, Input: b, Locale: "ko", RequestID: "web"})
	}
	s := run("memory.search", map[string]any{"query": "캠핑 텐트 누가 챙겨", "guild_id": application.PracticeGuild, "channel_id": application.PracticeChannel})
	if !s.OK {
		t.Fatalf("search: %+v", s.Error)
	}
	raw, _ := json.Marshal(s.Output["hits"])
	if !strings.Contains(string(raw), "텐트") {
		t.Fatalf("sample memory not found: %s", raw)
	}
	c := run("chat.respond", map[string]any{"guild_id": application.PracticeGuild, "channel_id": application.PracticeChannel, "message": "캠핑 갈 때 텐트 누가 챙기기로 했지?"})
	if !c.OK {
		t.Fatalf("chat: %+v", c.Error)
	}
	ans, _ := c.Output["answer"].(string)
	if !strings.Contains(ans, "연습") || !strings.Contains(ans, "텐트") || c.Output["cost_usd"].(float64) != 0 {
		t.Fatalf("practice answer: %q cost %v", ans, c.Output["cost_usd"])
	}
	for _, id := range []string{"backup.restore", "storage.migrate", "runtime.autostart.set"} {
		if r := run(id, map[string]any{"name": "x", "confirm": "x", "to": "sqlite", "enabled": true}); r.OK {
			t.Fatalf("%s must be refused in practice mode", id)
		}
	}
	// a real provider added to the practice config is never called
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	t.Setenv("PRACTICE_TEST_KEY", "sk-test")
	ops := []any{
		map[string]any{"path": []any{"providers", "real"}, "value": map[string]any{"kind": "openai_compat", "base_url": srv.URL, "credential_ref": "env:PRACTICE_TEST_KEY"}},
		map[string]any{"path": []any{"models", "response"}, "value": map[string]any{"provider": "real", "model": "gpt-real"}},
	}
	if r := run("config.patch", map[string]any{"ops": ops}); !r.OK {
		t.Fatalf("patch: %+v", r.Error)
	}
	run("providers.test", map[string]any{"id": "real"})
	run("chat.respond", map[string]any{"guild_id": application.PracticeGuild, "channel_id": application.PracticeChannel, "message": "안녕"})
	if n := hits.Load(); n != 0 {
		t.Fatalf("practice mode reached a real provider %d times", n)
	}
	p.Close()
	// reopening does not import twice
	p2, err := application.OpenPractice(ctx, root, "ko", nil)
	if err != nil {
		t.Fatal(err)
	}
	st := p2.Execute(ctx, action.Request{Action: "memory.blocks.list", Locale: "ko"})
	b, _ := json.Marshal(st.Output)
	if strings.Count(string(b), "kakao:practice") == 0 {
		t.Fatalf("practice block missing: %s", b)
	}
	if err := p2.Close(); err != nil {
		t.Fatal(err)
	}
	if err := application.ResetPractice(root); err != nil {
		t.Fatal(err)
	}
}
