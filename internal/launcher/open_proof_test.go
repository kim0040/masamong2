package launcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// openToken is the console token the tests below use; it never leaves the test process.
func openToken() (string, error) { return "synthetic-token", nil }

// staleListener is a program on the port a stopped daemon recorded in daemon.json. It answers health like
// a console would, without a proof, and counts every request that is not that health check. linkCalls
// counts the sign-in links Open builds for any address that is not allowed.
func staleListener(t *testing.T) (bind string, others *atomic.Int32) {
	t.Helper()
	others = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			fmtJSON(w, map[string]any{"ok": true, "ready": true})
			return
		}
		others.Add(1)
		fmtJSON(w, map[string]any{"ok": true})
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), others
}

// provenConsole is a console that knows the token and answers the challenge.
func provenConsole(t *testing.T) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeProvenHealth(w, r, "synthetic-token", map[string]any{"ready": true})
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

// linkRecorder is an OnceURL that remembers every address it was asked to build a link for.
type linkRecorder struct{ binds []string }

func (l *linkRecorder) once(bind string) (string, error) {
	l.binds = append(l.binds, bind)
	return "http://" + bind + "/login?once=synthetic-code", nil
}

// exitingDaemon is a stand-in masamongd that exits at once and leaves daemon.json alone.
func exitingDaemon(t *testing.T) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the stand-in daemon is a shell script")
	}
	p := filepath.Join(t.TempDir(), "masamongd")
	if err := os.WriteFile(p, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// A stale daemon.json that names a program which is not this folder's console gets no link: nothing but
// the health check reaches it, and no one-time link is built for it, whether the daemon can be started or
// not.
func TestOpenSendsNoLinkToAnUnprovenRecordedAddress(t *testing.T) {
	for name, daemon := range map[string]string{"cannot start": "/nonexistent/masamongd", "never answers": exitingDaemon(t)} {
		stale, others := staleListener(t)
		dir := t.TempDir()
		if err := WriteInfo(dir, DaemonInfo{PID: 1, Bind: stale}); err != nil {
			t.Fatal(err)
		}
		var links linkRecorder
		res, err := Open(context.Background(), Options{DataRoot: dir, NoBrowser: true, Daemon: daemon, Wait: 800 * time.Millisecond,
			OnceURL: links.once, Token: openToken})
		if err == nil {
			t.Fatalf("%s: Open reported success: %+v", name, res)
		}
		if len(links.binds) != 0 || strings.Contains(res.URL, "once=") {
			t.Fatalf("%s: a sign-in link was built for %v (url %q)", name, links.binds, res.URL)
		}
		if others.Load() != 0 {
			t.Fatalf("%s: %d requests other than the health check reached the stale address", name, others.Load())
		}
	}
}

// When no daemon runs, Open starts it and waits for a fresh record that proves itself. The stale address
// gets no link, and the link goes to the fresh one.
func TestOpenFollowsTheFreshRecordOfTheDaemonItStarts(t *testing.T) {
	stale, others := staleListener(t)
	fresh := provenConsole(t)
	dir := t.TempDir()
	if err := WriteInfo(dir, DaemonInfo{PID: 1, Bind: stale}); err != nil {
		t.Fatal(err)
	}
	written := make(chan error, 1)
	go func() { // the daemon takes the folder, then its console listens and writes its record
		time.Sleep(600 * time.Millisecond)
		written <- WriteInfo(dir, DaemonInfo{PID: 2, Bind: fresh})
	}()
	var links linkRecorder
	res, err := Open(context.Background(), Options{DataRoot: dir, NoBrowser: true, Daemon: exitingDaemon(t), Wait: 5 * time.Second,
		OnceURL: links.once, Token: openToken})
	if err != nil {
		t.Fatal(err)
	}
	if werr := <-written; werr != nil {
		t.Fatal(werr)
	}
	if !res.Started || res.Bind != fresh || len(links.binds) != 1 || links.binds[0] != fresh {
		t.Fatalf("result %+v, links built for %v", res, links.binds)
	}
	if others.Load() != 0 {
		t.Fatalf("%d requests other than the health check reached the stale address", others.Load())
	}
}

// While this folder's daemon holds its lock, a record that does not prove itself may be that of the daemon
// before it: Open does not start another daemon and does not send a link to it; it waits for the record
// the starting daemon writes.
func TestOpenWaitsForTheFreshRecordWhileTheDaemonStarts(t *testing.T) {
	shortRecordWait(t)
	stale, others := staleListener(t)
	fresh := provenConsole(t)
	dir := t.TempDir()
	if err := WriteInfo(dir, DaemonInfo{PID: 1, Bind: stale}); err != nil {
		t.Fatal(err)
	}
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if !Running(dir, "daemon.lock") {
		t.Skip("this system cannot see its own process's lock as held")
	}
	written := make(chan error, 1)
	go func() {
		time.Sleep(300 * time.Millisecond)
		written <- WriteInfo(dir, DaemonInfo{PID: 2, Bind: fresh})
	}()
	var links linkRecorder
	res, err := Open(context.Background(), Options{DataRoot: dir, NoBrowser: true, Daemon: "/nonexistent/masamongd",
		OnceURL: links.once, Token: openToken})
	if err != nil {
		t.Fatal(err)
	}
	if werr := <-written; werr != nil {
		t.Fatal(werr)
	}
	if res.Started || res.Bind != fresh || len(links.binds) != 1 || links.binds[0] != fresh {
		t.Fatalf("result %+v, links built for %v", res, links.binds)
	}
	if others.Load() != 0 {
		t.Fatalf("%d requests other than the health check reached the stale address", others.Load())
	}
}

// A link needs a token to prove the console with: without one, Open builds none.
func TestOpenBuildsNoLinkWithoutAToken(t *testing.T) {
	bind := provenConsole(t)
	dir := t.TempDir()
	if err := WriteInfo(dir, DaemonInfo{PID: 1, Bind: bind}); err != nil {
		t.Fatal(err)
	}
	var links linkRecorder
	if _, err := Open(context.Background(), Options{DataRoot: dir, NoBrowser: true, OnceURL: links.once}); err == nil || len(links.binds) != 0 {
		t.Fatalf("err %v, links built for %v", err, links.binds)
	}
	failing := func() (string, error) { return "", os.ErrNotExist }
	if _, err := Open(context.Background(), Options{DataRoot: dir, NoBrowser: true, OnceURL: links.once, Token: failing}); err == nil || len(links.binds) != 0 {
		t.Fatalf("err %v, links built for %v", err, links.binds)
	}
}

// A daemon stopped for maintenance has a plain instructions page and no sign-in: Open needs no proof for
// it, because no link is built.
func TestOpenMaintenancePageNeedsNoLink(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmtJSON(w, map[string]any{"ok": true, "maintenance": true})
	}))
	defer srv.Close()
	bind := strings.TrimPrefix(srv.URL, "http://")
	dir := t.TempDir()
	if err := WriteInfo(dir, DaemonInfo{PID: 1, Bind: bind}); err != nil {
		t.Fatal(err)
	}
	var links linkRecorder
	res, err := Open(context.Background(), Options{DataRoot: dir, NoBrowser: true, OnceURL: links.once, Token: openToken})
	if err != nil || !res.Maintenance || len(links.binds) != 0 || strings.Contains(res.URL, "once=") {
		t.Fatalf("%+v %v, links %v", res, err, links.binds)
	}
}
