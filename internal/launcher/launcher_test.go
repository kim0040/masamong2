package launcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"masamong/internal/fsutil"
)

// A second click must reuse the running console instead of starting another daemon.
func TestOpenReusesRunningDaemon(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"ready":true}`))
	}))
	defer srv.Close()
	dir := t.TempDir()
	bind := strings.TrimPrefix(srv.URL, "http://")
	if err := WriteInfo(dir, DaemonInfo{PID: 1, Bind: bind}); err != nil {
		t.Fatal(err)
	}
	res, err := Open(context.Background(), Options{DataRoot: dir, NoBrowser: true, Daemon: "/nonexistent/masamongd",
		OnceURL: func(b string) (string, error) { return "http://" + b + "/login?once=x", nil }})
	if err != nil {
		t.Fatal(err)
	}
	if res.Started || res.Bind != bind || !strings.Contains(res.URL, "once=") {
		t.Fatalf("reuse: %+v", res)
	}
}

func TestWriteInfoUsesPrivateStorage(t *testing.T) {
	dir := t.TempDir()
	if err := WriteInfo(dir, DaemonInfo{PID: 1, Bind: "127.0.0.1:8787"}); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.VerifyPrivateDir(dir); err != nil {
		t.Fatalf("daemon root is not private: %v", err)
	}
	if err := fsutil.VerifyPrivateFile(infoPath(dir)); err != nil {
		t.Fatalf("daemon info is not private: %v", err)
	}
}

func TestWriteCrashNoteUsesPrivateStorage(t *testing.T) {
	dir := t.TempDir()
	if err := WriteCrashNote(dir, CrashNote{Reason: "test"}); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.VerifyPrivateDir(dir); err != nil {
		t.Fatalf("crash-note root is not private: %v", err)
	}
	if err := fsutil.VerifyPrivateDir(filepath.Join(dir, "logs")); err != nil {
		t.Fatalf("crash-note folder is not private: %v", err)
	}
	if err := fsutil.VerifyPrivateFile(crashPath(dir)); err != nil {
		t.Fatalf("crash note is not private: %v", err)
	}
}

// A stale daemon.json (process gone) must not be trusted: Open tries to start a daemon.
func TestOpenIgnoresStaleInfo(t *testing.T) {
	dir := t.TempDir()
	_ = WriteInfo(dir, DaemonInfo{PID: 1, Bind: "127.0.0.1:1"})
	_, err := Open(context.Background(), Options{DataRoot: dir, NoBrowser: true, Daemon: "/nonexistent/masamongd"})
	if err == nil || !strings.Contains(err.Error(), "start") {
		t.Fatalf("expected a start attempt, got %v", err)
	}
}

func TestHostPortForWildcardBind(t *testing.T) {
	if got := hostPort("0.0.0.0:8787"); got != "127.0.0.1:8787" {
		t.Fatal(got)
	}
}

func TestConnectRefusesAnOptionAsHost(t *testing.T) {
	for _, h := range []string{"-oProxyCommand=touch /tmp/x", "a b"} {
		if err := Connect(context.Background(), ConnectOptions{Host: h}); err == nil || !strings.Contains(err.Error(), "not a host") {
			t.Fatalf("%q: %v", h, err)
		}
	}
}
