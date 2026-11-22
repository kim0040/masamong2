//go:build !noweb

package web

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/launcher"
)

// The console proves it knows the token for the address the request arrived on, and only for a
// challenge of the form clients send.
func TestHealthProvesTheTokenForTheAddressItListensOn(t *testing.T) {
	s, ts := testServer(t)
	addr := strings.TrimPrefix(ts.URL, "http://")
	challenge := strings.Repeat("ab", 16)
	_, out, _ := do(t, client(), "GET", ts.URL+"/api/health?challenge="+challenge, nil, nil)
	if out["proof"] != launcher.EndpointProof(s.currentToken(), addr, challenge) {
		t.Fatalf("proof %v", out["proof"])
	}
	for _, q := range []string{"", "?challenge=short", "?challenge=" + strings.Repeat("Z", 32)} {
		if _, out, _ := do(t, client(), "GET", ts.URL+"/api/health"+q, nil, nil); out["proof"] != nil {
			t.Fatalf("%q answered with a proof", q)
		}
	}
	// the launcher's client signs in to the real console after checking it
	c := launcher.NewClient(addr, s.currentToken())
	defer c.Close(context.Background())
	if _, err := c.Action(context.Background(), "help.version", nil); err != nil {
		t.Fatal(err)
	}
}

// A re-issue makes the old token stop working everywhere at once: sign-in, the long-lived link, pending
// one-time codes and every other session. The tab that asked stays signed in; the new token works.
func TestRotateTokenEndsEverythingMadeWithTheOldToken(t *testing.T) {
	s, ts := testServer(t)
	old := s.currentToken()
	tab, other := client(), client()
	csrf := signIn(t, s, ts, tab)
	otherCSRF := signIn(t, s, ts, other)
	code, err := CreateOnce(s.svc.DataRoot)
	if err != nil {
		t.Fatal(err)
	}
	oldLink := strings.Replace(LoginURL("127.0.0.1:1", old), "http://127.0.0.1:1", ts.URL, 1)

	if st, _, _ := do(t, other, "POST", ts.URL+launcher.RotateTokenPath, nil, nil); st != http.StatusForbidden {
		t.Fatalf("re-issue without the CSRF header: %d", st)
	}
	if st, out, _ := do(t, tab, "POST", ts.URL+launcher.RotateTokenPath, nil, map[string]string{"X-CSRF-Token": csrf}); st != 200 || out["ok"] != true || out["token"] != nil {
		t.Fatalf("re-issue %d %v", st, out)
	}
	fresh, err := ReadToken(s.svc.DataRoot)
	if err != nil || fresh == old || fresh != s.currentToken() {
		t.Fatalf("token file %v", err)
	}
	if st, _ := act(t, tab, ts, csrf, "help.version", nil); st != 200 {
		t.Fatalf("the tab that asked was signed out: %d", st)
	}
	if st, _ := act(t, other, ts, otherCSRF, "help.version", nil); st != http.StatusUnauthorized {
		t.Fatalf("another session survived: %d", st)
	}
	if st, _, _ := do(t, client(), "POST", ts.URL+"/api/login", map[string]any{"token": old}, nil); st != http.StatusUnauthorized {
		t.Fatalf("old token signed in: %d", st)
	}
	res, err := client().Get(oldLink)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if !strings.Contains(res.Header.Get("Location"), "error=token") {
		t.Fatalf("old link: %s", res.Header.Get("Location"))
	}
	res, err = client().Get(ts.URL + "/login?once=" + code)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if !strings.Contains(res.Header.Get("Location"), "error=token") {
		t.Fatalf("a one-time code made before the re-issue still works: %s", res.Header.Get("Location"))
	}
	newLink := strings.Replace(LoginURL("127.0.0.1:1", fresh), "http://127.0.0.1:1", ts.URL, 1)
	res, err = client().Get(newLink)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.Header.Get("Location") != "/" {
		t.Fatalf("new link: %s", res.Header.Get("Location"))
	}
}

// A re-issue asked by the CLI or the tray ends that client's session too: it signs in again with the
// new token, which a client with Reload reads from the file.
func TestRotateTokenFromTheCLIEndsItsOwnSessionAndClientsFollow(t *testing.T) {
	s, ts := testServer(t)
	addr := strings.TrimPrefix(ts.URL, "http://")
	root := s.svc.DataRoot
	tray := launcher.NewClient(addr, s.currentToken())
	tray.Via = "tray"
	tray.Reload = func() (string, error) { return ReadToken(root) }
	if _, err := tray.Action(context.Background(), "help.version", nil); err != nil {
		t.Fatal(err)
	}
	cli := launcher.NewClient(addr, s.currentToken())
	if err := cli.RotateToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	if n := s.sessions.len(); n != 0 {
		t.Fatalf("%d sessions survived a re-issue from the CLI", n)
	}
	if _, err := cli.Action(context.Background(), "help.version", nil); err == nil {
		t.Fatal("a client without Reload kept working with the old token")
	}
	if _, err := tray.Action(context.Background(), "help.version", nil); err != nil {
		t.Fatalf("the tray did not pick up the new token: %v", err)
	}
}

// Re-issues and sign-ins can run at the same time (run with -race).
func TestRotateTokenWhileSigningIn(t *testing.T) {
	s, ts := testServer(t)
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			_, _ = s.rotateToken()
		}()
		go func() {
			defer wg.Done()
			res, err := http.PostForm(ts.URL+"/lite", url.Values{"token": {s.currentToken()}})
			if err == nil {
				res.Body.Close()
			}
		}()
	}
	wg.Wait()
	if got, err := ReadToken(s.svc.DataRoot); err != nil || got != s.currentToken() {
		t.Fatalf("file and console disagree: %v", err)
	}
}

// Failed sign-ins through an admin.web.hosts name (another device) count against that bucket only, for
// the one-click link as for the API: they cannot lock this computer out.
func TestRemoteLinkFailuresDoNotLockOutThisComputer(t *testing.T) {
	s, ts := testServer(t)
	c := client()
	csrf := signIn(t, s, ts, c)
	if st, out := act(t, c, ts, csrf, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"admin", "web", "hosts"}, "value": []any{"desk.example.ts.net"}}}}); st != 200 {
		t.Fatalf("list the name: %d %v", st, out)
	}
	link := func(host, token string) string {
		req, _ := http.NewRequest("GET", ts.URL+"/login?token="+token, nil)
		req.Host = host
		res, err := client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.Header.Get("Location")
	}
	for i := 0; i < 12; i++ {
		link("desk.example.ts.net:8787", "wrong")
	}
	if got := link("desk.example.ts.net:8787", s.currentToken()); !strings.Contains(got, "error=rate") {
		t.Fatalf("the remote bucket is not limited: %s", got)
	}
	if got := link(strings.TrimPrefix(ts.URL, "http://"), s.currentToken()); got != "/" {
		t.Fatalf("remote failures locked this computer out: %s", got)
	}
}

// The limit holds when attempts arrive together: the check and the count are one step.
func TestLoginLimiterCountsConcurrentAttempts(t *testing.T) {
	l := &loginLimiter{}
	now := time.Now()
	var mu sync.Mutex
	passed := 0
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, ok := l.attempt(now); ok {
				mu.Lock()
				passed++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if passed != 10 {
		t.Fatalf("%d attempts passed, want 10", passed)
	}
	// a success gives its try back
	l = &loginLimiter{}
	for i := 0; i < 20; i++ {
		succeeded, ok := l.attempt(now)
		if !ok {
			t.Fatalf("attempt %d refused although every one succeeded", i)
		}
		succeeded()
	}
}

// A restart keeps the browser's address only when it is on loopback; Listen checks again.
func TestRestartBindMustBeLoopback(t *testing.T) {
	s, _ := testServer(t)
	s.UseRestartBind("192.0.2.10:8787")
	if s.addr != "" || !isLoopback(s.Bind()) {
		t.Fatalf("kept a non-loopback restart address: %q", s.Bind())
	}
	s.UseRestartBind("127.0.0.1:9123")
	if s.Bind() != "127.0.0.1:9123" {
		t.Fatalf("loopback restart address dropped: %q", s.Bind())
	}
	s.addr = "192.0.2.10:0" // as if set some other way: Listen must still not open it
	ctx, cancel := context.WithCancel(context.Background())
	s.Force = true
	got := make(chan string, 1)
	s.OnListen = func(addr string) { got <- addr; cancel() }
	if err := s.Listen(ctx); err != nil {
		t.Fatal(err)
	}
	if addr := <-got; !isLoopback(addr) {
		t.Fatalf("listened on %s", addr)
	}
}
