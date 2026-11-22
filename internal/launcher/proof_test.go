package launcher

import (
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// writeProvenHealth answers /api/health like a console that knows token: with the proof for the
// challenge and the address the request arrived on.
func writeProvenHealth(w http.ResponseWriter, r *http.Request, token string, extra map[string]any) {
	out := map[string]any{"ok": true}
	for k, v := range extra {
		out[k] = v
	}
	if c := r.URL.Query().Get(ChallengeParam); ValidChallenge(c) {
		addr := r.Context().Value(http.LocalAddrContextKey).(net.Addr).String()
		out["proof"] = EndpointProof(token, addr, c)
	}
	fmtJSON(w, out)
}

// tokenCatcher is a program that took the console's port: it answers health like a console (without a
// proof, or with one for another token) and records every request body that reaches it.
func tokenCatcher(t *testing.T, proofToken string) (bind string, bodies *atomic.Int32) {
	t.Helper()
	bodies = &atomic.Int32{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			if proofToken == "" {
				fmtJSON(w, map[string]any{"ok": true, "ready": true})
			} else {
				writeProvenHealth(w, r, proofToken, nil)
			}
			return
		}
		if b, _ := io.ReadAll(r.Body); len(b) > 0 {
			bodies.Add(1)
		}
		fmtJSON(w, map[string]any{"ok": true, "csrf": "c"})
	}))
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://"), bodies
}

// A program on a stopped service's port receives no token: it cannot prove it knows it, whether it
// sends no proof or one made with another installation's token.
func TestFakeListenerDoesNotReceiveTheToken(t *testing.T) {
	for _, proofToken := range []string{"", "another-installations-token"} {
		bind, bodies := tokenCatcher(t, proofToken)
		dir := t.TempDir()
		if err := WriteInfo(dir, DaemonInfo{PID: 1, Bind: bind}); err != nil {
			t.Fatal(err)
		}
		c := NewClient(bind, "synthetic-token")
		c.DataRoot = dir
		if _, err := c.Action(context.Background(), "runtime.status", nil); !errors.Is(err, errUnproven) {
			t.Fatalf("proof %q: %v", proofToken, err)
		}
		if ok, err := Stop(context.Background(), dir, "synthetic-token", "cli"); ok || !errors.Is(err, errUnproven) {
			t.Fatalf("stop, proof %q: %v %v", proofToken, ok, err)
		}
		if n := bodies.Load(); n != 0 {
			t.Fatalf("proof %q: %d requests with a body reached the fake listener", proofToken, n)
		}
	}
}

// The maintenance stop sends the token too, so it needs the same proof.
func TestMaintenanceStopNeedsTheProof(t *testing.T) {
	bind, bodies := tokenCatcher(t, "")
	if err := stopMaintenance(context.Background(), t.TempDir(), bind, "synthetic-token", "", 1); !errors.Is(err, errUnproven) {
		t.Fatal(err)
	}
	if bodies.Load() != 0 {
		t.Fatal("the token reached a console that did not prove itself")
	}
}

// A held daemon lock proves nothing about the recorded address: a daemon that is still starting holds it
// while daemon.json may still name the port of the copy that crashed, which another program may have
// taken. A console that cannot answer the challenge (another program, or a console of an older version)
// never receives the token, whether the lock is held or not, for the sign-in and the maintenance stop.
func TestUnprovenListenerIsRejectedWithDaemonLockHeld(t *testing.T) {
	shortRecordWait(t)
	bind, bodies := tokenCatcher(t, "")
	dir := t.TempDir()
	if err := WriteInfo(dir, DaemonInfo{PID: 1, Bind: bind}); err != nil {
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
	c := NewClient(bind, "synthetic-token")
	c.DataRoot = dir
	if _, err := c.Action(context.Background(), "runtime.status", nil); !errors.Is(err, errUnproven) {
		t.Fatalf("sign-in: %v", err)
	}
	if ok, err := Stop(context.Background(), dir, "synthetic-token", "cli"); ok || !errors.Is(err, errUnproven) {
		t.Fatalf("stop: %v %v", ok, err)
	}
	if err := stopMaintenance(context.Background(), dir, bind, "synthetic-token", "", 1); !errors.Is(err, errUnproven) {
		t.Fatalf("maintenance stop: %v", err)
	}
	if n := bodies.Load(); n != 0 {
		t.Fatalf("%d requests with a body reached a listener that did not prove itself", n)
	}
}

// While the daemon lock is held, a record that does not prove itself may belong to a daemon that is still
// starting: the client reads the record again and signs in once a fresh record names a console that
// proves itself, and the stale address never receives the token.
func TestSignInWaitsForAFreshRecordWhileTheDaemonStarts(t *testing.T) {
	shortRecordWait(t)
	stale, staleBodies := tokenCatcher(t, "")
	var logins atomic.Int32
	real := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/health":
			writeProvenHealth(w, r, "synthetic-token", nil)
		case "/api/login":
			logins.Add(1)
			fmtJSON(w, map[string]any{"ok": true, "csrf": "c"})
		default:
			fmtJSON(w, map[string]any{"ok": true, "output": map[string]any{}})
		}
	}))
	defer real.Close()
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
	listened := make(chan error, 1)
	go func() { // the starting daemon's console listens and writes its record
		time.Sleep(300 * time.Millisecond)
		listened <- WriteInfo(dir, DaemonInfo{PID: 2, Bind: strings.TrimPrefix(real.URL, "http://")})
	}()
	c := NewClient(stale, "synthetic-token")
	c.DataRoot = dir
	if _, err := c.Action(context.Background(), "runtime.status", nil); err != nil {
		t.Fatalf("sign-in after the fresh record: %v", err)
	}
	if err := <-listened; err != nil {
		t.Fatal(err)
	}
	if logins.Load() != 1 || staleBodies.Load() != 0 {
		t.Fatalf("logins %d, bodies at the stale address %d", logins.Load(), staleBodies.Load())
	}
}

// shortRecordWait keeps a test's wait for a fresh daemon record short.
func shortRecordWait(t *testing.T) {
	t.Helper()
	old := recordWait
	recordWait = time.Second
	t.Cleanup(func() { recordWait = old })
}

func TestTokenIsNeverSentOffThisComputer(t *testing.T) {
	for _, bind := range []string{"192.0.2.1:8787", "example.invalid:8787", "[2001:db8::1]:8787"} {
		if err := verifyEndpoint(context.Background(), bind, "synthetic-token"); !errors.Is(err, errNotLoopback) {
			t.Fatalf("%s: %v", bind, err)
		}
	}
	for _, bind := range []string{"127.0.0.1:1", "[::1]:1", "localhost:1", ":1", "0.0.0.0:1"} {
		if !loopbackAddr(hostPort(bind)) {
			t.Fatalf("%s is this computer", bind)
		}
	}
}

// A proven console that redirects the sign-in elsewhere does not get the token re-sent to the target.
func TestSignInDoesNotFollowRedirects(t *testing.T) {
	var elsewhere atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { elsewhere.Add(1) }))
	defer target.Close()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/health" {
			writeProvenHealth(w, r, "synthetic-token", nil)
			return
		}
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer srv.Close()
	c := NewClient(strings.TrimPrefix(srv.URL, "http://"), "synthetic-token")
	if _, err := c.Action(context.Background(), "runtime.status", nil); err == nil {
		t.Fatal("a redirected sign-in succeeded")
	}
	if elsewhere.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

// A long-lived client (the tray) reads the token again before each sign-in, so it follows a re-issue.
func TestClientReloadsTheTokenAfterARotation(t *testing.T) {
	current := "synthetic-token-one"
	var logins atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/health":
			writeProvenHealth(w, r, current, nil)
		case "/api/login":
			logins.Add(1)
			fmtJSON(w, map[string]any{"ok": true, "csrf": "csrf-" + current})
		case "/api/action":
			if r.Header.Get("X-CSRF-Token") != "csrf-"+current {
				w.WriteHeader(http.StatusUnauthorized)
				return
			}
			fmtJSON(w, map[string]any{"ok": true, "output": map[string]any{}})
		}
	}))
	defer srv.Close()
	file := "synthetic-token-one"
	c := NewClient(strings.TrimPrefix(srv.URL, "http://"), "synthetic-token-one")
	c.Reload = func() (string, error) { return file, nil }
	if _, err := c.Action(context.Background(), "runtime.status", nil); err != nil {
		t.Fatal(err)
	}
	current, file = "synthetic-token-two", "synthetic-token-two" // re-issued: old session and token are gone
	if _, err := c.Action(context.Background(), "runtime.status", nil); err != nil {
		t.Fatalf("after a re-issue: %v", err)
	}
	if logins.Load() != 2 {
		t.Fatalf("logins %d", logins.Load())
	}
}
