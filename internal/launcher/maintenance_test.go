package launcher

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
)

// fakeMaintenanceDaemon answers like the daemon stopped for maintenance: a health reply that says so,
// no sign-in, and a stop endpoint that needs the admin token.
func fakeMaintenanceDaemon(t *testing.T, token string, stopped *atomic.Bool) (bind string, srv *httptest.Server) {
	t.Helper()
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/health":
			_, _ = w.Write([]byte(`{"ok":true,"maintenance":true,"ready":false}`))
		case "/api/maintenance/stop":
			var in struct{ Token string }
			_ = json.NewDecoder(r.Body).Decode(&in)
			if in.Token != token {
				http.Error(w, "no", http.StatusUnauthorized)
				return
			}
			stopped.Store(true)
			_, _ = w.Write([]byte(`{"ok":true}`))
			w.(http.Flusher).Flush() // the answer must reach the client before its connection is closed
			go srv.CloseClientConnections()
			go srv.Close()
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), srv
}

// The launcher, the icon and `masamong open` must treat a daemon stopped for maintenance as running, in
// a state of its own, and open its instructions page: no sign-in link, no second daemon.
func TestOpenShowsTheMaintenancePageOfAStoppedDaemon(t *testing.T) {
	var stopped atomic.Bool
	bind, _ := fakeMaintenanceDaemon(t, "tok", &stopped)
	dir := t.TempDir()
	if err := WriteInfo(dir, DaemonInfo{PID: 1, Bind: bind}); err != nil {
		t.Fatal(err)
	}
	h, err := Health(context.Background(), bind)
	if err != nil || !InMaintenance(h) || h["ready"] != false {
		t.Fatalf("health must say maintenance, not 'not ready': %v %v", h, err)
	}
	signIn := false
	res, err := Open(context.Background(), Options{DataRoot: dir, NoBrowser: true, Daemon: "/nonexistent/masamongd",
		OnceURL: func(string) (string, error) { signIn = true; return "http://x/login?once=1", nil }})
	if err != nil {
		t.Fatalf("open must not start another daemon or fail: %v", err)
	}
	if res.Started || !res.Maintenance || res.URL != "http://"+bind+"/" || signIn {
		t.Fatalf("%+v signIn=%v", res, signIn)
	}
}

// `masamong stop` (and the icon's Quit) must stop the maintenance daemon and must not delete its
// daemon.json as a stale record while the process lives.
func TestStopEndsTheMaintenanceDaemonAndKeepsItsRecord(t *testing.T) {
	var stopped atomic.Bool
	bind, _ := fakeMaintenanceDaemon(t, "tok", &stopped)
	dir := t.TempDir()
	if err := WriteInfo(dir, DaemonInfo{PID: 1, Bind: bind}); err != nil {
		t.Fatal(err)
	}
	if ok, err := Stop(context.Background(), dir, "wrong", "cli"); ok || err == nil {
		t.Fatalf("a wrong token must be refused: %v %v", ok, err)
	}
	if _, err := os.Stat(infoPath(dir)); err != nil {
		t.Fatal("daemon.json deleted although the daemon is alive")
	}
	if ok, err := Stop(context.Background(), dir, "tok", "cli"); !ok || err != nil || !stopped.Load() {
		t.Fatalf("stop: %v %v stopped=%v", ok, err, stopped.Load())
	}
}

// A daemon that is busy (no answer within the health timeout) holds daemon.lock; its record is not stale.
func TestStopDoesNotDeleteTheRecordOfADaemonThatHoldsTheLock(t *testing.T) {
	dir := t.TempDir()
	if err := WriteInfo(dir, DaemonInfo{PID: 1, Bind: "127.0.0.1:1"}); err != nil { // nothing listens
		t.Fatal(err)
	}
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	ok, stopErr := Stop(context.Background(), dir, "tok", "cli")
	lock.Release()
	if ok || stopErr == nil {
		t.Fatalf("a daemon that holds the lock is running: %v %v", ok, stopErr)
	}
	if _, err := os.Stat(infoPath(dir)); err != nil {
		t.Fatal("daemon.json of a running daemon was deleted")
	}
	// without the lock the record is stale and goes
	if ok, err := Stop(context.Background(), dir, "tok", "cli"); ok || err != nil {
		t.Fatalf("%v %v", ok, err)
	}
	if _, err := os.Stat(infoPath(dir)); !os.IsNotExist(err) {
		t.Fatal("a stale record must be removed")
	}
}
