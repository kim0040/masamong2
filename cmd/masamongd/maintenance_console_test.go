package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/config"
	"masamong/internal/launcher"
	"masamong/internal/transport/web"
)

func get(h http.Handler, path string, headers map[string]string) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	r := httptest.NewRequest("GET", "http://localhost"+path, nil)
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	h.ServeHTTP(w, r)
	return w
}

func TestMaintenanceConsoleUsesConfiguredBindWhenTheJournalBlocksStartup(t *testing.T) {
	root := t.TempDir()
	doc := config.Default("test", root)
	doc.Admin.Web.Bind = "127.0.0.1:8801"
	raw, err := config.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if got := maintenanceConsoleBind(root, ""); got != "127.0.0.1:8801" {
		t.Fatalf("configured bind = %q", got)
	}
	if got := maintenanceConsoleBind(root, "127.0.0.1:8795"); got != "127.0.0.1:8795" {
		t.Fatalf("restart bind should win = %q", got)
	}
	if err := os.WriteFile(filepath.Join(root, "config.yaml"), []byte("broken: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := maintenanceConsoleBind(root, ""); got != "127.0.0.1:8787" {
		t.Fatalf("invalid config fallback = %q", got)
	}
}

// The launcher, `masamong status` and the tray read /api/health: a maintenance daemon must answer ok with
// a state of its own, not look like a console that is "not ready".
func TestMaintenanceHealthIsAStateOfItsOwn(t *testing.T) {
	srv := httptest.NewServer(maintenanceHandler(t.TempDir(), nil))
	defer srv.Close()
	h, err := launcher.Health(context.Background(), strings.TrimPrefix(srv.URL, "http://"))
	if err != nil || !launcher.InMaintenance(h) || h["ready"] != false {
		t.Fatalf("%v %v", h, err)
	}
}

// A browser gets a plain page in its language: what happened, that the bot is stopped, and only the
// commands that can work in this state. No script, no raw error.
func TestMaintenancePageSaysWhatToDoInTheVisitorsLanguage(t *testing.T) {
	root := t.TempDir()
	// an unversioned marker from an older version: cancel is the only command that can work
	if err := os.WriteFile(filepath.Join(root, "reset-pending.json"), []byte(`{"secret":"https://user:synthetic-secret@x"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := maintenanceHandler(root, nil)
	for lang, want := range map[string]string{
		"ko-KR,ko;q=0.9": "마사몽이 점검을 위해 멈춰 있어요", "en-US": "Masamong is stopped for maintenance", "ja": "メンテナンスのため停止",
		"zh-CN,zh;q=0.8": "因维护而停止", "es-ES,es;q=0.9": "detenido por mantenimiento", "fr": "Masamong is stopped for maintenance",
	} {
		w := get(h, "/", map[string]string{"Accept": "text/html,application/xhtml+xml", "Accept-Language": lang})
		body := w.Body.String()
		if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "text/html") || !strings.Contains(body, want) {
			t.Fatalf("%s: %d %s", lang, w.Code, body)
		}
		if strings.Contains(body, "<script") || strings.Contains(body, "synthetic-secret") {
			t.Fatalf("%s: script or raw error in the page", lang)
		}
		if !strings.Contains(body, "word-break:keep-all") || !strings.Contains(body, "overflow-wrap:anywhere") {
			t.Fatalf("%s: mobile text must keep Korean words together and wrap long paths", lang)
		}
		if !strings.Contains(body, "--maintenance cancel") || strings.Contains(body, "--maintenance retry") || strings.Contains(body, "--maintenance rollback") {
			t.Fatalf("%s: only cancel can work for an old marker:\n%s", lang, body)
		}
		if !strings.Contains(body, " stop</pre>") { // masamong [--data-root ...] stop
			t.Fatalf("%s: the page must say how to stop this daemon", lang)
		}
	}
	// a client that does not ask for HTML keeps getting JSON
	if w := get(h, "/", nil); !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatal(w.Header())
	}
}

func TestMaintenanceStopNeedsTheAdminTokenAndKeepsTheHostChecks(t *testing.T) {
	root := t.TempDir()
	var stopped atomic.Bool
	h := maintenanceHandler(root, func() { stopped.Store(true) })
	tok, err := web.EnsureToken(root)
	if err != nil {
		t.Fatal(err)
	}
	post := func(body, host, origin string) int {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "http://localhost/api/maintenance/stop", strings.NewReader(body))
		if host != "" {
			r.Host = host
		}
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		h.ServeHTTP(w, r)
		return w.Code
	}
	if c := post(`{"token":"nope"}`, "", ""); c != http.StatusUnauthorized {
		t.Fatalf("wrong token: %d", c)
	}
	if c := post(`{"token":"`+tok+`"}`, "attacker.example", ""); c != http.StatusMisdirectedRequest {
		t.Fatalf("rebinding: %d", c)
	}
	if c := post(`{"token":"`+tok+`"}`, "", "https://attacker.example"); c != http.StatusForbidden {
		t.Fatalf("cross-origin: %d", c)
	}
	for _, identity := range []struct {
		root string
		pid  int
	}{
		{filepath.Join(root, "other"), os.Getpid()}, {root, os.Getpid() + 1},
	} {
		body, _ := json.Marshal(map[string]any{"token": tok, "data_root": identity.root, "pid": identity.pid})
		if c := post(string(body), "", ""); c != http.StatusConflict {
			t.Fatalf("another maintenance instance must be refused: %d", c)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if stopped.Load() {
		t.Fatal("stopped without a valid request")
	}
	var out map[string]any
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "http://localhost/api/maintenance/stop", strings.NewReader(`{"token":"`+tok+`"}`)))
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil || out["stopping"] != true {
		t.Fatalf("%s %v", w.Body.String(), err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for !stopped.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !stopped.Load() {
		t.Fatal("a valid stop request did not stop the daemon")
	}
}

func TestMaintenanceSupervisorStopUsesSameAuthenticatedEndpoint(t *testing.T) {
	root := t.TempDir()
	token, err := web.EnsureToken(root)
	if err != nil {
		t.Fatal(err)
	}
	var stopped atomic.Bool
	srv := httptest.NewServer(maintenanceHandler(root, func() { stopped.Store(true) }))
	defer srv.Close()
	if err := launcher.WriteInfo(root, launcher.DaemonInfo{PID: os.Getpid(), Bind: strings.TrimPrefix(srv.URL, "http://")}); err != nil {
		t.Fatal(err)
	}
	if err := launcher.RequestStopChild(context.Background(), root, os.Getpid(), token); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(time.Second)
	for !stopped.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if !stopped.Load() {
		t.Fatal("supervisor's authenticated stop did not reach maintenance daemon")
	}
}
