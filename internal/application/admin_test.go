package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/application"
)

func out(t *testing.T, svc *application.Service, act string, in map[string]any) map[string]any {
	t.Helper()
	r := exec(svc, act, in)
	if !r.OK {
		t.Fatalf("%s: %+v", act, r.Error)
	}
	m, err := jsonRoundTrip(r.Output)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestConfigPatchEditsByPathWithCAS(t *testing.T) {
	svc := setupSvc(t)
	doc := out(t, svc, "config.document", nil)
	rev := int(doc["revision"].(float64))
	if doc["schema"] == nil || doc["defaults"] == nil {
		t.Fatal("schema and defaults are part of the document view")
	}
	// tool ids contain dots; guild ids are numeric map keys: paths are segment lists
	r := exec(svc, "config.patch", map[string]any{"base_revision": rev, "ops": []any{
		map[string]any{"path": []any{"tools", "finance.fx", "limits", "per_day"}, "value": 5},
		map[string]any{"path": []any{"guilds", "1234", "channels", "5678", "respond"}, "value": "mention"},
	}})
	if !r.OK {
		t.Fatalf("patch: %+v", r.Error)
	}
	if svc.Doc().Tools["finance.fx"].Limits.PerDay != 5 {
		t.Fatalf("limit not applied: %+v", svc.Doc().Tools["finance.fx"])
	}
	if svc.Doc().Guilds["1234"].Channels["5678"].Respond != "mention" {
		t.Fatal("channel policy not applied")
	}
	if r := exec(svc, "config.patch", map[string]any{"base_revision": rev, "ops": []any{
		map[string]any{"path": []any{"limits", "monthly_budget_usd"}, "value": "3.00"}}}); r.OK || r.Error.Code != "CONFIG_CONFLICT" {
		t.Fatalf("stale base must conflict: %+v", r)
	}
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{
		map[string]any{"path": []any{"limits", "no_such_field"}, "value": 1}}}); r.OK {
		t.Fatal("unknown fields must be rejected like the CLI does")
	}
	r = exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"tools", "finance.fx"}, "op": "unset"}}})
	if !r.OK || svc.Doc().Tools["finance.fx"].Limits.PerDay != 0 {
		t.Fatalf("unset: %+v", r)
	}
	audit := out(t, svc, "audit.list", nil)["entries"].([]any)
	found := false
	for _, e := range audit {
		found = found || strings.Contains(e.(map[string]any)["summary"].(string), "tools.finance.fx.limits.per_day")
	}
	if len(audit) < 3 || !found {
		t.Fatalf("change log must record patch paths: %v", audit)
	}
	for _, e := range audit {
		if strings.Contains(e.(map[string]any)["summary"].(string), "mention") {
			t.Fatal("patch values stay out of the change log")
		}
	}
}

func TestPauseStopsRepliesAndCommandsButNotTheConsole(t *testing.T) {
	svc, _ := chatSvc(t, `{"tool_plan":[]}`)
	if r := exec(svc, "runtime.pause", map[string]any{"scope": "all"}); !r.OK {
		t.Fatalf("pause: %+v", r)
	}
	in := application.ChatInput{GuildID: "1", ChannelID: "2", Message: "hi", Speaker: "a"}
	if _, err := svc.ChatTurn(context.Background(), in); !errors.Is(err, application.ErrChatPaused) {
		t.Fatalf("paused reply: %v", err)
	}
	if r := as(svc, "finance.fx", map[string]any{"base": "USD"}, &action.Invoker{Via: "command", GuildID: "1", ChannelID: "2", UserID: "u"}); r.OK || r.Error.Code != "FEATURE_DISABLED" {
		t.Fatalf("paused command: %+v", r)
	}
	if r := exec(svc, "dashboard.overview", nil); !r.OK {
		t.Fatalf("console keeps working: %+v", r)
	}
	turns := out(t, svc, "turns.list", nil)["turns"].([]any)
	if len(turns) != 1 || turns[0].(map[string]any)["outcome"] != "paused" {
		t.Fatalf("refused turns are logged: %v", turns)
	}
	exec(svc, "runtime.pause", map[string]any{"scope": "all", "paused": "false"})
	exec(svc, "runtime.pause", map[string]any{"scope": "channel", "guild_id": "1", "channel_id": "2"})
	if !svc.Paused("1", "2") || svc.Paused("1", "3") {
		t.Fatal("channel pause scope")
	}
}

func TestRuntimeDrainUsesDurableGlobalPause(t *testing.T) {
	svc := setupSvc(t)
	if r := exec(svc, "setup.resume", map[string]any{"instance": "drain-test"}); !r.OK {
		t.Fatalf("setup: %+v", r)
	}
	drained := exec(svc, "runtime.drain", map[string]any{"enabled": true})
	if !drained.OK || drained.Output["draining"] != true || !svc.Paused("any", "room") {
		t.Fatalf("drain: %+v", drained)
	}
	resumed := exec(svc, "runtime.drain", map[string]any{"draining": false})
	if !resumed.OK || resumed.Output["draining"] != false || svc.Paused("any", "room") {
		t.Fatalf("resume: %+v", resumed)
	}
}

func TestTurnLogDashboardAndSeries(t *testing.T) {
	svc, _ := chatSvc(t, `{"tool_plan":[]}`)
	for _, msg := range []string{"안녕", "뭐해"} {
		if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "1", ChannelID: "2", Message: msg, Speaker: "철수"}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "1", ChannelID: "2", Message: "lab", Speaker: "t", DryRun: true}); err != nil {
		t.Fatal(err)
	}
	list := out(t, svc, "turns.list", nil)["turns"].([]any)
	if len(list) != 2 {
		t.Fatalf("lab turns are separate: %d", len(list))
	}
	first := list[0].(map[string]any)
	got := out(t, svc, "turns.get", map[string]any{"id": first["id"]})["turn"].(map[string]any)
	if got["body"].(map[string]any)["answer"] == "" {
		t.Fatal("turn detail carries the answer")
	}
	if lab := out(t, svc, "turns.list", map[string]any{"lab": "only"})["turns"].([]any); len(lab) != 1 {
		t.Fatalf("lab filter: %d", len(lab))
	}
	dash := out(t, svc, "dashboard.overview", nil)
	if dash["today"].(map[string]any)["turns"].(map[string]any)["answered"].(float64) != 2 {
		t.Fatalf("today counts: %v", dash["today"])
	}
	for _, c := range dash["checklist"].([]any) {
		if c.(map[string]any)["id"] == "test_chat" && c.(map[string]any)["done"] != true {
			t.Fatal("checklist is computed from real turns")
		}
	}
	// stats.query reads the all-time totals the console's Stats page now shows (from the read pool)
	if totals := out(t, svc, "stats.query", nil); totals["window"] != "all_time" || totals["denominator"] != "stored_rows" {
		t.Fatalf("stats.query: %v", totals)
	}
	series := out(t, svc, "stats.series", map[string]any{"days": 7})
	answered := series["turns"].(map[string]any)["answered"].([]any)
	if len(answered) != 7 || answered[6].(float64) != 2 {
		t.Fatalf("series: %v", answered)
	}
}

// The dashboard alerts only what stops the bot: a tool that lacks its optional key (disasters without
// KMA, stock quotes without Finnhub) is marked on the tools page, not raised as an alert.
func TestDashboardDoesNotAlertOptionalToolKeys(t *testing.T) {
	svc := setupSvc(t)
	dash := out(t, svc, "dashboard.overview", nil)
	for _, raw := range dash["attention"].([]any) {
		if item := raw.(map[string]any); item["key"] == "tool_unconfigured" {
			t.Fatalf("optional tool keys are not alerts: %+v", item)
		}
	}
	tl := map[string]map[string]any{}
	for _, raw := range out(t, svc, "tools.list", nil)["tools"].([]any) {
		tl[raw.(map[string]any)["id"].(string)] = raw.(map[string]any)
	}
	if tl["weather.query"]["ready"] != true {
		t.Fatalf("keyless Open-Meteo keeps weather ready: %+v", tl["weather.query"])
	}
	if tl["disasters.query"]["ready"] != false || tl["disasters.query"]["ready_reason"] != "no_key" {
		t.Fatalf("disasters without a KMA key: %+v", tl["disasters.query"])
	}
}

func TestPrefsAndLabReviews(t *testing.T) {
	svc := setupSvc(t)
	out(t, svc, "admin.prefs.set", map[string]any{"prefs": map[string]any{"theme": "dark", "lang": "ko"}})
	out(t, svc, "admin.prefs.set", map[string]any{"prefs": map[string]any{"lang": nil, "density": "compact"}})
	p := out(t, svc, "admin.prefs", nil)["prefs"].(map[string]any)
	if p["theme"] != "dark" || p["density"] != "compact" || p["lang"] != nil {
		t.Fatalf("prefs merge: %v", p)
	}
	id := out(t, svc, "lab.review", map[string]any{"question": "작년 여름?", "answer": "부산", "verdict": "good", "tags": []any{"time"}})["id"]
	rv := out(t, svc, "lab.reviews", nil)
	if rv["counts"].(map[string]any)["good"].(float64) != 1 {
		t.Fatalf("reviews: %v", rv)
	}
	out(t, svc, "lab.review", map[string]any{"delete": id})
	if n := len(out(t, svc, "lab.reviews", nil)["reviews"].([]any)); n != 0 {
		t.Fatalf("delete: %d", n)
	}
	if r := exec(svc, "lab.review", map[string]any{"question": "x", "verdict": "meh"}); r.OK {
		t.Fatal("verdict is validated")
	}
}

func TestLogsRingRedacts(t *testing.T) {
	svc := setupSvc(t)
	log := svc.CaptureLogs(nil)
	log.Info("provider call", "token", "sk-live-123", "provider", "openrouter")
	log.Warn("slow", "ms", 900)
	e := out(t, svc, "runtime.logs", map[string]any{"level": "info"})["entries"].([]any)
	raw, _ := json.Marshal(e)
	if strings.Contains(string(raw), "sk-live") || len(e) != 2 {
		t.Fatalf("ring: %s", raw)
	}
	if w := out(t, svc, "runtime.logs", map[string]any{"level": "warn"})["entries"].([]any); len(w) != 1 {
		t.Fatalf("level filter: %d", len(w))
	}
	meta := out(t, svc, "runtime.logs", map[string]any{"after_id": 999})
	if meta["source"] != "memory" || meta["buffer"].(float64) != 3000 || meta["oldest_id"].(float64) != 1 {
		t.Fatalf("ring metadata: %v", meta)
	}
}

func TestConsoleReadsAreNotToolUse(t *testing.T) {
	svc := setupSvc(t)
	for i := 0; i < 3; i++ {
		raw, _ := json.Marshal(map[string]any{})
		if r := svc.Execute(context.Background(), action.Request{Action: "polls.result", Input: raw, RequestID: "web"}); !r.OK {
			t.Fatalf("polls.result: %+v", r.Error)
		}
	}
	tl := out(t, svc, "tools.list", nil)["tools"].([]any)
	for _, x := range tl {
		m := x.(map[string]any)
		if m["id"] == "polls.result" && m["used_today"].(float64) != 0 {
			t.Fatalf("console reads counted as tool use: %v", m["used_today"])
		}
	}
}
