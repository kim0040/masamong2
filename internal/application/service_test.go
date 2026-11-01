package application_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/jobs"
)

func setupSvc(t *testing.T) *application.Service {
	t.Helper()
	return setupSvcLog(t, nil)
}

// setupSvcLog installs the logger before setup starts background workers (they read it, so swapping it
// afterwards races them).
func setupSvcLog(t *testing.T, logger func(*application.Service) *slog.Logger) *application.Service {
	t.Helper()
	dir := t.TempDir()
	svc, err := application.Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	if logger != nil {
		svc.Log = logger(svc)
	}
	raw, _ := json.Marshal(map[string]any{"instance": "gate"})
	res := svc.Execute(context.Background(), action.Request{Action: "setup.resume", Input: raw, Locale: "en"})
	if !res.OK {
		t.Fatalf("setup: %+v", res)
	}
	return svc
}

// serveAnyServer switches server approval off for tests about something else that run Discord
// requests in made-up servers (approval has its own tests).
func serveAnyServer(t *testing.T, svc *application.Service) {
	t.Helper()
	if r := exec(svc, "access.policy", map[string]any{"server_approval": false}); !r.OK {
		t.Fatalf("server approval: %+v", r)
	}
}

func exec(svc *application.Service, act string, in map[string]any) action.Result {
	var raw json.RawMessage
	if in != nil {
		raw, _ = json.Marshal(in)
	}
	return svc.Execute(context.Background(), action.Request{Action: act, Input: raw, Locale: "en", BaseRevision: intVal(in, "base_revision")})
}

func intVal(in map[string]any, k string) int {
	if in == nil {
		return 0
	}
	if v, ok := in[k]; ok {
		switch n := v.(type) {
		case int:
			return n
		case float64:
			return int(n)
		}
	}
	return 0
}

func TestSetupIdempotent(t *testing.T) {
	svc := setupSvc(t)
	res := exec(svc, "setup.resume", map[string]any{"instance": "gate"})
	if !res.OK {
		t.Fatal(res.Error)
	}
	if res.Revision != 1 && res.Output["revision"] != 1 {
		t.Fatalf("revision %+v result.rev=%d", res.Output["revision"], res.Revision)
	}
	if res.Output["legacy_env_loaded"] != false {
		t.Fatal("must not load legacy env")
	}
}

func TestDoctorNotSingleReady(t *testing.T) {
	svc := setupSvc(t)
	res := exec(svc, "runtime.doctor", nil)
	if !res.OK {
		t.Fatal(res.Error)
	}
	if res.Output["single_ready"] != false || res.Output["memory_complete"] != false {
		t.Fatalf("%+v", res.Output)
	}
	feats := res.Output["features"].(map[string]any)
	mem := feats["memory"].(map[string]any)
	// No embedding credential and no data in the test environment: memory must not claim readiness.
	if mem["status"] == "ready" {
		t.Fatalf("memory %+v", mem)
	}
	// The built-in keyless Open-Meteo source makes worldwide weather ready without a KMA key.
	if feats["weather"].(map[string]any)["status"] != "ready" {
		t.Fatalf("weather %+v", feats["weather"])
	}
	if feats["disasters"].(map[string]any)["status"] != "unconfigured" {
		t.Fatalf("disasters %+v", feats["disasters"])
	}
	if feats["polls"].(map[string]any)["status"] != "ready" {
		t.Fatal("polls")
	}
	if feats["eval"].(map[string]any)["status"] != "ready" {
		t.Fatalf("eval %+v", feats["eval"])
	}
	if res.Output["instance"] != "gate" {
		t.Fatalf("instance %v", res.Output["instance"])
	}
	if res.Output["profile"] != "lite" {
		t.Fatalf("profile %v", res.Output["profile"])
	}
}

func TestDeferredActionUnavailable(t *testing.T) {
	svc := setupSvc(t)
	// storage.migrate is switched off with the TiDB backend (catalog OperatorDisabled)
	res := exec(svc, "storage.migrate", nil)
	if res.OK || res.Error == nil || res.Error.Code != errcode.ActionUnavailable {
		t.Fatalf("%+v", res)
	}
	if res := exec(svc, "messages.search", nil); res.OK || res.Error == nil || res.Error.Code != errcode.InvalidInput {
		t.Fatalf("promoted message search should validate its input: %+v", res)
	}
}

func TestConfigCASStaleRejected(t *testing.T) {
	svc := setupSvc(t)
	path := filepath.Join(svc.DataRoot, "candidate.yaml")
	doc := config.Default("gate", svc.DataRoot)
	doc.I18n.DefaultLocale = "ko"
	raw, _ := config.Marshal(doc)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	stale := exec(svc, "config.apply", map[string]any{"file": path, "base_revision": 0})
	if stale.OK || stale.Error.Code != errcode.ConfigConflict {
		t.Fatalf("stale %+v", stale)
	}
	ok := exec(svc, "config.apply", map[string]any{"file": path, "base_revision": 1})
	if !ok.OK {
		t.Fatalf("apply %+v", ok)
	}
	if ok.Revision != 2 {
		t.Fatalf("revision %d", ok.Revision)
	}
}

func TestJobStaleWorkerAndIdempotency(t *testing.T) {
	svc := setupSvc(t)
	j, err := svc.Enqueue("demo", "idem-1", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	j2, err := svc.Enqueue("demo", "idem-1", `{}`)
	if err != nil || j2.ID != j.ID {
		t.Fatalf("idempotency %v %v", j2, err)
	}
	now := time.Now()
	leased, err := jobs.Lease(j, "tok-a", now, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	// persist leased so CompleteWithToken sees it
	if _, err := svc.TakeLease(j.ID, "tok-b", time.Minute); err != nil {
		t.Fatal(err)
	}
	_, err = svc.CompleteWithToken(j.ID, "tok-a", `{"stale":true}`)
	if err == nil {
		t.Fatal("stale token must fail")
	}
	got, err := svc.CompleteWithToken(j.ID, "tok-b", `{"ok":true}`)
	if err != nil {
		t.Fatal(err)
	}
	if got.Result != `{"ok":true}` {
		t.Fatalf("%+v", got)
	}
	_ = leased
}

func TestJobProgressShowsOnList(t *testing.T) {
	svc := setupSvc(t)
	j, err := svc.Enqueue("demo", "progress-1", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Unix(0, 0)
	now := started.Add(10 * time.Second)
	p := jobs.Snapshot("ingest", 25, 100, started, now)
	if _, err := svc.SetJobProgress(j.ID, "", p); err != nil {
		t.Fatal(err)
	}
	res := exec(svc, "jobs.list", nil)
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	list, _ := res.Output["jobs"].([]map[string]any)
	if len(list) != 1 {
		t.Fatalf("jobs %d", len(list))
	}
	if list[0]["progress_pct"] != 25.0 {
		t.Fatalf("progress %#v", list[0]["progress_pct"])
	}
	label, _ := list[0]["progress_label"].(string)
	if label == "" {
		t.Fatalf("missing label %#v", list[0])
	}
}

func TestStalePersistDoesNotOverwriteNewerLease(t *testing.T) {
	svc := setupSvc(t)
	j, err := svc.Enqueue("demo", "idem-cas", `{}`)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.TakeLease(j.ID, "tok-new", time.Minute); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CompleteWithToken(j.ID, "tok-old", `{"hijack":true}`); err == nil {
		t.Fatal("CAS must reject old lease")
	}
	got := exec(svc, "jobs.get", map[string]any{"id": j.ID})
	if !got.OK {
		t.Fatalf("%+v", got)
	}
	if got.Output["result"] == `{"hijack":true}` {
		t.Fatal("stale worker wrote result")
	}
}

func TestWeatherNoticesFortunePolls(t *testing.T) {
	svc := setupSvc(t)
	// with the worldwide source switched off and no KMA key, weather has no live source
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"providers", "openmeteo", "disabled"}, "value": true}}}); !r.OK {
		t.Fatal(r.Error)
	}
	w := exec(svc, "weather.query", map[string]any{"region": "seoul"})
	if !w.OK || w.Output["live"] != false || w.Output["status"] != "unconfigured" {
		t.Fatalf("unconfigured weather must not look live: %+v", w)
	}
	fx := exec(svc, "weather.query", map[string]any{"region": "seoul", "fixture": "ok"})
	if !fx.OK || fx.Output["source"] != "fixture" || fx.Output["live"] != false {
		t.Fatalf("fixture must be labeled: %+v", fx)
	}
	if bad := exec(svc, "weather.query", map[string]any{"region": "아틀란티스"}); bad.OK || bad.Error.Code != errcode.InvalidInput {
		t.Fatalf("unknown region must not default silently: %+v", bad)
	}
	p := exec(svc, "polls.create", map[string]any{
		"id": "p1", "scope": "room-a", "question": "lunch",
		"option_ids": []string{"a", "b"}, "max_select": 1,
	})
	if !p.OK {
		t.Fatalf("create %+v", p)
	}
	v := exec(svc, "polls.vote", map[string]any{
		"id": "p1", "scope": "room-a", "base_revision": 1,
		"participant": "u1", "option_ids": []string{"a"},
	})
	if !v.OK {
		t.Fatalf("vote %+v", v)
	}
	r := exec(svc, "polls.result", map[string]any{"id": "p1"})
	if r.Output["total_selections"] != 1 {
		t.Fatalf("%+v", r.Output)
	}
}

func TestEmptyAgentPlanRejectedBeforeCost(t *testing.T) {
	svc := setupSvc(t)
	res := exec(svc, "agent.simulate", map[string]any{"plan": map[string]any{"nodes": []any{}}})
	if res.OK || res.Error == nil || res.Error.Code != errcode.PlanRejected {
		t.Fatalf("empty plan %+v", res)
	}
	if res.Error.Args["before_cost"] != true {
		t.Fatalf("%+v", res.Error)
	}
}

func TestAgentPlanReject(t *testing.T) {
	svc := setupSvc(t)
	res := exec(svc, "agent.simulate", map[string]any{
		"plan": map[string]any{"nodes": []any{
			map[string]any{"id": "a", "depends_on": []any{"b"}},
			map[string]any{"id": "b", "depends_on": []any{"a"}},
		}},
	})
	if res.OK || res.Error == nil || res.Error.Code != errcode.PlanRejected {
		t.Fatalf("cycle %+v", res)
	}
	if res.Error.Args["before_cost"] != true {
		t.Fatalf("must reject before cost %+v", res.Error)
	}
}

func TestOperatorConfigForm(t *testing.T) {
	svc := setupSvc(t)
	res := exec(svc, "config.apply", map[string]any{
		"form":               "operator",
		"monthly_budget_usd": "25.00",
		"response_provider":  "openrouter",
		"response_model":     "z-ai/glm-5.3-flash",
		"feature_polls":      "off",
		"base_revision":      1,
	})
	if !res.OK {
		t.Fatalf("%+v", res)
	}
	show := exec(svc, "config.show", nil)
	if show.Output["monthly_budget_usd"] != "25.00" {
		t.Fatalf("budget %+v", show.Output)
	}
	if show.Output["response_model"] != "z-ai/glm-5.3-flash" {
		t.Fatalf("model %+v", show.Output)
	}
	switch feats := show.Output["features"].(type) {
	case map[string]bool:
		if feats["polls"] {
			t.Fatal("polls should be off")
		}
	case map[string]any:
		if on, _ := feats["polls"].(bool); on {
			t.Fatal("polls should be off")
		}
	default:
		t.Fatalf("features type %T", show.Output["features"])
	}
}

func TestBackupDryRun(t *testing.T) {
	svc := setupSvc(t)
	c := exec(svc, "backup.create", nil)
	if !c.OK {
		t.Fatal(c.Error)
	}
	name, _ := c.Output["name"].(string)
	d := exec(svc, "backup.restoreDryRun", map[string]any{"name": name})
	if !d.OK || d.Output["applied"] != false {
		t.Fatalf("%+v", d)
	}
	if v := exec(svc, "backup.verify", map[string]any{"name": name}); !v.OK {
		t.Fatalf("verify %+v", v)
	}
	// Path traversal must be refused: verify reads only inside the backups directory.
	outside := filepath.Join(svc.DataRoot, "config.yaml")
	if v := exec(svc, "backup.verify", map[string]any{"path": outside}); v.OK || v.Error.Code == errcode.IO {
		t.Fatalf("outside path accepted: %+v", v)
	}
	if v := exec(svc, "backup.verify", map[string]any{"name": "../config.yaml"}); v.OK {
		t.Fatalf("traversal accepted: %+v", v)
	}
	// Tampered database must fail verification.
	dbPath := filepath.Join(svc.DataRoot, "backups", name, "masamong.db")
	if err := os.WriteFile(dbPath, []byte("corrupt"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v := exec(svc, "backup.verify", map[string]any{"name": name}); v.OK || v.Error.Code != errcode.BackupVerifyFailed {
		t.Fatalf("tamper not detected: %+v", v)
	}
}

// Every capability that ships reports its real state: none is "not implemented", and each non-ready
// reason has text a person can read.
func TestDoctorReportsShippedFeatures(t *testing.T) {
	svc := setupSvc(t)
	res := exec(svc, "runtime.doctor", map[string]any{})
	feats := res.Output["features"].(map[string]any)
	for name, v := range feats {
		m := v.(map[string]any)
		if m["reason"] == "not_implemented" || m["reason"] == "rag_dependency_deferred" || m["status"] == "unavailable" {
			t.Errorf("%s still reported as not built: %v", name, m)
		}
		if r, _ := m["reason"].(string); r != "" && m["reason_text"] == nil && m["status"] != "error" {
			t.Errorf("%s reason %q has no text", name, r)
		}
	}
	if feats["notices"].(map[string]any)["status"] != "ready" {
		t.Errorf("notices use the built-in catalog: %v", feats["notices"])
	}
	if s := feats["fortune"].(map[string]any)["status"]; s != "ready" && s != "unconfigured" {
		t.Errorf("fortune %v", feats["fortune"])
	}
}
