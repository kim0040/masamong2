package launcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
)

func TestChildStopRefusesAnotherPIDPortAndDataFolder(t *testing.T) {
	root, other := t.TempDir(), t.TempDir()
	var actions atomic.Int32
	servedRoot := root
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/health":
			fmtJSON(w, map[string]any{"ok": true})
		case "/api/login":
			fmtJSON(w, map[string]any{"ok": true, "csrf": "test"})
		case "/api/session":
			fmtJSON(w, map[string]any{"authenticated": true, "data_root": servedRoot})
		case "/api/action":
			if r.Header.Get("X-CSRF-Token") != "test" {
				t.Error("missing CSRF")
			}
			actions.Add(1)
			fmtJSON(w, map[string]any{"ok": true})
		default:
			fmtJSON(w, map[string]any{"ok": true})
		}
	}))
	defer srv.Close()
	bind := strings.TrimPrefix(srv.URL, "http://")
	t.Setenv("MASAMONG_DATA_ROOT", other)
	t.Setenv("APPDATA", t.TempDir())
	if err := WriteInfo(root, DaemonInfo{PID: 123, Bind: bind}); err != nil {
		t.Fatal(err)
	}
	if err := RequestStopChild(context.Background(), root, 456, "test"); err == nil {
		t.Fatal("followed another PID")
	}
	servedRoot = other
	if err := RequestStopChild(context.Background(), root, 123, "test"); err == nil {
		t.Fatal("stopped another data folder at a stale port")
	}
	if actions.Load() != 0 {
		t.Fatal("sent mutation to another instance")
	}
	servedRoot = root
	if err := RequestStopChild(context.Background(), root, 123, "test"); err != nil {
		t.Fatal(err)
	}
	if actions.Load() != 1 {
		t.Fatal("did not stop its own instance exactly once")
	}
	if _, err := os.Stat(infoPath(root)); err != nil {
		t.Fatal("request removed the child's record")
	}
	if err := WriteInfo(root, DaemonInfo{PID: 123, Bind: "192.0.2.1:8787"}); err != nil {
		t.Fatal(err)
	}
	if err := RequestStopChild(context.Background(), root, 123, "test"); err == nil {
		t.Fatal("accepted nonlocal address")
	}
}

func fmtJSON(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(v) }

func TestExplicitDataFoldersKeepLocksPortsAndAutostartSeparate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "명시 폴더")
	other := filepath.Join(t.TempDir(), "environment")
	t.Setenv("MASAMONG_DATA_ROOT", other)
	t.Setenv("APPDATA", t.TempDir())
	if DefaultDataRoot() != other {
		t.Fatal("environment default not honored")
	}
	for _, name := range []string{"daemon.lock", "tray.lock"} {
		first, err := AcquireNamedLock(root, name)
		if err != nil {
			t.Fatal(err)
		}
		second, err := AcquireNamedLock(other, name)
		if err != nil {
			first.Release()
			t.Fatal("another root must have its own lock")
		}
		first.Release()
		if !Running(other, name) {
			t.Fatal("releasing one instance affected another")
		}
		second.Release()
	}
	for i, dir := range []string{root, other} {
		if err := WriteInfo(dir, DaemonInfo{PID: i + 1, Bind: []string{"127.0.0.1:9001", "127.0.0.1:9002"}[i]}); err != nil {
			t.Fatal(err)
		}
	}
	RemoveInfo(root)
	if info, ok := ReadInfo(other); !ok || info.PID != 2 {
		t.Fatal("port records collided")
	}
	path, _ := autostartPath(root)
	otherPath, _ := autostartPath(other)
	if path == otherPath {
		t.Fatal("login autostart entries collided")
	}
	// Build registration arguments without registering, querying or modifying the OS.
	bin := t.TempDir()
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	if err := os.WriteFile(filepath.Join(bin, "masamong"+ext), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	old := executable
	executable = func() (string, error) { return filepath.Join(bin, "masamongd"+ext), nil }
	defer func() { executable = old }()
	cmd, err := autostartCommand(root, true)
	if err != nil || len(cmd) != 4 || cmd[2] != root {
		t.Fatalf("registration must pin explicit folder: %v", err)
	}
}

func TestStopChildRechecksFolderAfterReauthentication(t *testing.T) {
	testStopChildReauth(t, true)
}

func TestStopChildRetriesWhenTheFolderStillMatches(t *testing.T) {
	testStopChildReauth(t, false)
}

func testStopChildReauth(t *testing.T, diverge bool) {
	t.Helper()
	root, other := t.TempDir(), t.TempDir()
	t.Setenv("MASAMONG_DATA_ROOT", t.TempDir())
	t.Setenv("APPDATA", t.TempDir())
	var sessions, attempts, accepted atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/health":
			fmtJSON(w, map[string]any{"ok": true})
		case "/api/login":
			fmtJSON(w, map[string]any{"ok": true, "csrf": "test"})
		case "/api/session":
			folder := root
			if sessions.Add(1) > 1 && diverge {
				folder = other
			}
			fmtJSON(w, map[string]any{"authenticated": true, "data_root": folder})
		case "/api/action":
			if attempts.Add(1) == 1 {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			accepted.Add(1)
			fmtJSON(w, map[string]any{"ok": true})
		default:
			fmtJSON(w, map[string]any{"ok": true})
		}
	}))
	defer srv.Close()
	bind := strings.TrimPrefix(srv.URL, "http://")
	if err := WriteInfo(root, DaemonInfo{PID: 123, Bind: bind}); err != nil {
		t.Fatal(err)
	}
	err := RequestStopChild(context.Background(), root, 123, "test")
	wantAttempts := int32(2)
	if diverge {
		wantAttempts = 1
		if err == nil || !strings.Contains(err.Error(), "another data folder") {
			t.Fatalf("retried against a different folder: %v", err)
		}
		if accepted.Load() != 0 {
			t.Fatal("sent the stop to the other folder")
		}
	} else if err != nil || accepted.Load() != 1 {
		t.Fatalf("retry err %v accepted %d", err, accepted.Load())
	}
	if attempts.Load() != wantAttempts || sessions.Load() < 2 {
		t.Fatalf("attempts %d sessions %d", attempts.Load(), sessions.Load())
	}
}
