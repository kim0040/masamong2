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

// A console of an older version cannot answer the challenge. It is still signed in to while this data
// folder's daemon lock is held (that daemon is alive, so nothing else holds its port), and refused
// otherwise.
func TestOlderConsoleIsAcceptedOnlyWhileTheDaemonLockIsHeld(t *testing.T) {
	bind, bodies := tokenCatcher(t, "")
	dir := t.TempDir()
	if err := verifyEndpoint(context.Background(), bind, "synthetic-token", dir); !errors.Is(err, errUnproven) {
		t.Fatalf("no lock: %v", err)
	}
	lock, err := AcquireLock(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	if !Running(dir, "daemon.lock") {
		t.Skip("this system cannot see its own process's lock as held")
	}
	if err := verifyEndpoint(context.Background(), bind, "synthetic-token", dir); err != nil {
		t.Fatalf("lock held: %v", err)
	}
	if bodies.Load() != 0 {
		t.Fatal("verification sent a body")
	}
}

func TestTokenIsNeverSentOffThisComputer(t *testing.T) {
	for _, bind := range []string{"192.0.2.1:8787", "example.invalid:8787", "[2001:db8::1]:8787"} {
		if err := verifyEndpoint(context.Background(), bind, "synthetic-token", ""); !errors.Is(err, errNotLoopback) {
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
