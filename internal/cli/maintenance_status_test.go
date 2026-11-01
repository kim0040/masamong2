package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
