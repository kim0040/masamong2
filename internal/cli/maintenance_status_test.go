package cli

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

	"masamong/internal/action"
	"masamong/internal/launcher"
)

func TestStatusCommandsReportMaintenance(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("CI", "")

	root := t.TempDir()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/health" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true,"maintenance":true,"ready":false}`))
	}))
	defer srv.Close()
	if err := launcher.WriteInfo(root, launcher.DaemonInfo{Bind: strings.TrimPrefix(srv.URL, "http://")}); err != nil {
		t.Fatal(err)
	}

	var jsonOut strings.Builder
	if code := run(context.Background(), []string{"--data-root", root, "--output", "json", "status"}, &jsonOut, ioDiscard(), false, false); code != 0 {
		t.Fatalf("status exit %d: %s", code, jsonOut.String())
	}
	var status action.Result
	if err := json.Unmarshal([]byte(jsonOut.String()), &status); err != nil {
		t.Fatal(err)
	}
	if status.Output["maintenance"] != true || status.Output["message"] != "masamong is stopped for maintenance" {
		t.Fatalf("status did not expose maintenance: %+v", status.Output)
	}

	var human strings.Builder
	if code := run(context.Background(), []string{"--data-root", root, "status"}, &human, ioDiscard(), false, true); code != 0 {
		t.Fatalf("human status exit %d: %s", code, human.String())
	}
	if got := strings.TrimSpace(human.String()); got != "masamong is stopped for maintenance" {
		t.Fatalf("human status should be one line, got %q", got)
	}

	root = t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "maintenance-operation.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	jsonOut.Reset()
	if code := run(context.Background(), []string{"--data-root", root, "--output", "json", "runtime", "status"}, &jsonOut, ioDiscard(), false, false); code != 0 {
		t.Fatalf("runtime status exit %d: %s", code, jsonOut.String())
	}
	var runtimeStatus action.Result
	if err := json.Unmarshal([]byte(jsonOut.String()), &runtimeStatus); err != nil {
		t.Fatal(err)
	}
	if runtimeStatus.Output["maintenance"] != true || runtimeStatus.Output["message"] != "masamong is stopped for maintenance" {
		t.Fatalf("runtime status did not expose maintenance: %+v", runtimeStatus.Output)
	}

	human.Reset()
	if code := run(context.Background(), []string{"--data-root", root, "runtime", "status"}, &human, ioDiscard(), false, true); code != 0 {
		t.Fatalf("human runtime status exit %d: %s", code, human.String())
	}
	if got := strings.TrimSpace(human.String()); got != "masamong is stopped for maintenance" {
		t.Fatalf("human runtime status should be one line, got %q", got)
	}
}

// `masamong open` and `start` do not build a sign-in link for a recorded address that is not this data
// folder's console: a stale daemon.json that names another program gets nothing but the health check, and
// the command says the service did not become ready.
func TestOpenAndStartSendNoLinkToAnUnprovenRecordedAddress(t *testing.T) {
	t.Setenv("MASAMONG_NO_BROWSER", "1")
	t.Setenv("PATH", t.TempDir()) // no masamongd to start
	for _, args := range [][]string{{"open", "--no-browser", "--no-tray"}, {"start", "--no-tray"}} {
		root := t.TempDir()
		var others atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/api/health" {
				others.Add(1)
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"ok":true,"ready":true}`))
		}))
		if err := launcher.WriteInfo(root, launcher.DaemonInfo{PID: 1, Bind: strings.TrimPrefix(srv.URL, "http://")}); err != nil {
			t.Fatal(err)
		}
		var out strings.Builder
		code := run(context.Background(), append([]string{"--data-root", root, "--output", "json"}, args...), &out, ioDiscard(), false, false)
		srv.Close()
		if code == 0 || strings.Contains(out.String(), "once=") {
			t.Fatalf("%v: exit %d, out %s", args, code, out.String())
		}
		if others.Load() != 0 {
			t.Fatalf("%v: %d requests other than the health check reached the stale address", args, others.Load())
		}
	}
}
