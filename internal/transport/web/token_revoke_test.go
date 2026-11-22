//go:build !noweb

package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/fsutil"
	"masamong/internal/launcher"
)

// gatedClock stands in for Server.now: the call numbered at (from 1) waits until release closes, and
// reached closes when it arrives. Every other call passes straight through.
type gatedClock struct {
	at      int32
	calls   atomic.Int32
	reached chan struct{}
	release chan struct{}
}

func newGatedClock(at int32) *gatedClock {
	return &gatedClock{at: at, reached: make(chan struct{}), release: make(chan struct{})}
}

func (g *gatedClock) now() time.Time {
	if g.calls.Add(1) == g.at {
		close(g.reached)
		<-g.release
	}
	return time.Now()
}

// sessionCookieOf returns the session cookie a response set, or "".
func sessionCookieOf(rec *httptest.ResponseRecorder) string {
	for _, c := range rec.Result().Cookies() {
		if c.Name == sessionCookie && c.MaxAge >= 0 {
			return c.Value
		}
	}
	return ""
}

func (s *Server) cookieSignsIn(id string) bool {
	req := httptest.NewRequest(http.MethodGet, "/api/session", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
	_, _, ok := s.currentSession(req)
	return ok
}

// A sign-in that checked the old token (or a code made with it) and is still on its way when the token is
// re-issued ends with no usable session: JSON, the emergency page, the long-lived link and one-time links.
// The clock call held back is the first one after the credential was checked.
func TestRotationRevokesConcurrentSignIn(t *testing.T) {
	for _, tc := range []struct {
		name   string
		gateAt int32
		req    func(s *Server) (*http.Request, func(http.ResponseWriter, *http.Request))
	}{
		{"json", 2, func(s *Server) (*http.Request, func(http.ResponseWriter, *http.Request)) {
			r := httptest.NewRequest(http.MethodPost, "/api/login", strings.NewReader(`{"token":"`+s.currentToken()+`"}`))
			return r, s.apiLogin
		}},
		{"lite", 2, func(s *Server) (*http.Request, func(http.ResponseWriter, *http.Request)) {
			r := httptest.NewRequest(http.MethodPost, "/lite", strings.NewReader(url.Values{"token": {s.currentToken()}}.Encode()))
			r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			return r, s.liteSignIn
		}},
		{"token link", 2, func(s *Server) (*http.Request, func(http.ResponseWriter, *http.Request)) {
			return httptest.NewRequest(http.MethodGet, "/login?token="+s.currentToken(), nil), s.loginLink
		}},
		{"once link", 3, func(s *Server) (*http.Request, func(http.ResponseWriter, *http.Request)) {
			code, err := CreateOnce(s.svc.DataRoot)
			if err != nil {
				panic(err)
			}
			return httptest.NewRequest(http.MethodGet, "/login?once="+code, nil), s.loginLink
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := testServer(t)
			req, handler := tc.req(s)
			req.Host = "127.0.0.1:8787"
			gate := newGatedClock(tc.gateAt)
			s.now = gate.now
			rec := httptest.NewRecorder()
			signedIn := make(chan struct{})
			go func() {
				defer close(signedIn)
				handler(rec, req)
			}()
			select {
			case <-gate.reached:
			case <-signedIn:
				t.Fatalf("the sign-in finished without reaching the held clock call: %d %v", rec.Code, rec.Header())
			}
			rotated := make(chan error, 1)
			go func() { _, err := s.rotateToken(); rotated <- err }()
			var rotateErr error
			done := false
			select { // an implementation that orders the two waits for the sign-in instead
			case rotateErr = <-rotated:
				done = true
			case <-time.After(500 * time.Millisecond):
			}
			close(gate.release)
			<-signedIn
			if !done {
				rotateErr = <-rotated
			}
			if rotateErr != nil {
				t.Fatal(rotateErr)
			}
			if id := sessionCookieOf(rec); id != "" && s.cookieSignsIn(id) {
				t.Fatal("a sign-in made with the old token survived the re-issue")
			}
		})
	}
}

// A re-issue that stopped after it published the new token, before it removed the one-time codes and the
// saved sessions, leaves nothing usable: the next start refuses both, since they were made with the old
// token.
func TestInterruptedRotationLeavesNoUsableCredential(t *testing.T) {
	s, _ := testServer(t)
	root := s.svc.DataRoot
	id, _ := s.sessions.create(s.now(), "test browser", s.currentTag())
	if err := s.sessions.save(root, s.now()); err != nil {
		t.Fatal(err)
	}
	code, err := CreateOnce(root)
	if err != nil {
		t.Fatal(err)
	}
	if err := fsutil.WriteAtomic(TokenPath(root), []byte(strings.Repeat("5", 48)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := New(s.svc)
	restarted.sessions.load(root, restarted.now(), restarted.currentTag())
	if restarted.cookieSignsIn(id) {
		t.Fatal("a session saved under the old token was restored")
	}
	req := httptest.NewRequest(http.MethodGet, "/login?once="+code, nil)
	req.Host = "127.0.0.1:8787"
	rec := httptest.NewRecorder()
	restarted.loginLink(rec, req)
	if loc := rec.Header().Get("Location"); !strings.Contains(loc, "error=token") || sessionCookieOf(rec) != "" {
		t.Fatalf("a one-time code made with the old token signed in: %q", loc)
	}
}

// Once the new token is in place the console says the re-issue worked, even when a leftover file could
// not be removed (those are refused anyway).
func TestRotationReportsSuccessOnceTheTokenChanged(t *testing.T) {
	s, ts := testServer(t)
	root := s.svc.DataRoot
	old := s.currentToken()
	// a non-empty folder where sessions.json goes cannot be removed
	if err := os.MkdirAll(filepath.Join(sessionsPath(root), "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	c := client()
	csrf := signIn(t, s, ts, c)
	st, out, _ := do(t, c, "POST", ts.URL+launcher.RotateTokenPath, nil, map[string]string{"X-CSRF-Token": csrf})
	if fresh, err := ReadToken(root); err != nil || fresh == old {
		t.Fatalf("the token was not replaced: %v", err)
	}
	if st != http.StatusOK || out["ok"] != true {
		t.Fatalf("the console reported a failure after the token changed: %d %v", st, out)
	}
}

// A re-issue from a browser gives that browser a fresh session: a copy of its old cookie and CSRF value
// (another client holding the same credential) is signed out with everything else, while the browser
// that asked goes on with its new cookie and the CSRF value its open tabs already hold.
func TestBrowserRotationEndsCopiesOfItsSession(t *testing.T) {
	s, ts := testServer(t)
	tab := client()
	csrf := signIn(t, s, ts, tab)
	u, _ := url.Parse(ts.URL)
	oldCookie := tab.Jar.Cookies(u)
	copied := client()
	copied.Jar.SetCookies(u, oldCookie)
	if st, _ := act(t, copied, ts, csrf, "help.version", nil); st != http.StatusOK {
		t.Fatalf("the copy does not work before the re-issue: %d", st)
	}
	if st, out, _ := do(t, tab, "POST", ts.URL+launcher.RotateTokenPath, nil, map[string]string{"X-CSRF-Token": csrf}); st != http.StatusOK || out["ok"] != true {
		t.Fatalf("re-issue %d %v", st, out)
	}
	if st, _ := act(t, copied, ts, csrf, "help.version", nil); st != http.StatusUnauthorized {
		t.Fatalf("a copy of the asking session survived the re-issue: %d", st)
	}
	newCookie := tab.Jar.Cookies(u)
	if len(oldCookie) != 1 || len(newCookie) != 1 || newCookie[0].Value == oldCookie[0].Value {
		t.Fatalf("the asking tab kept its session cookie: %v -> %v", oldCookie, newCookie)
	}
	if st, _ := act(t, tab, ts, csrf, "help.version", nil); st != http.StatusOK {
		t.Fatalf("the asking tab cannot use its fresh session: %d", st)
	}
	if _, sess, _ := do(t, tab, "GET", ts.URL+"/api/session", nil, nil); sess["authenticated"] != true || sess["csrf"] != csrf {
		t.Fatalf("the asking tab's session: %v", sess)
	}
}

// A re-issue closes the event streams of the sessions it ended at once, before they deliver another
// change, instead of at their next keep-alive check.
func TestRotationClosesEventStreamsOfRevokedSessions(t *testing.T) {
	s, ts := testServer(t)
	s.ping = time.Hour // only the re-issue can end the stream in time
	viewer := client()
	signIn(t, s, ts, viewer)
	closeStream, br := openStream(t, viewer, ts.URL+"/api/events")
	defer closeStream()
	tab := client()
	csrf := signIn(t, s, ts, tab)
	if st, _, _ := do(t, tab, "POST", ts.URL+launcher.RotateTokenPath, nil, map[string]string{"X-CSRF-Token": csrf}); st != http.StatusOK {
		t.Fatalf("re-issue %d", st)
	}
	raw := []byte(`{"ops":[{"path":["limits","monthly_budget_usd"],"value":"3.50"}]}`)
	if res := s.svc.Execute(context.Background(), action.Request{Action: "config.patch", Input: raw, RequestID: "test"}); !res.OK {
		t.Fatalf("synthetic change: %+v", res.Error)
	}
	ended := make(chan string, 1)
	go func() {
		for {
			line, err := br.ReadString('\n')
			if strings.HasPrefix(line, "event: change") {
				ended <- "change"
				return
			}
			if err != nil {
				ended <- "closed"
				return
			}
		}
	}()
	select {
	case got := <-ended:
		if got != "closed" {
			t.Fatal("a revoked session's stream delivered a change after the re-issue")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("a revoked session's stream stayed open after the re-issue")
	}
}

// A one-time code file that survived a re-issue (its removal failed) is refused by the running console
// too: it holds the tag of the old token.
func TestLeftoverCodeFromBeforeARotationIsRefused(t *testing.T) {
	s, _ := testServer(t)
	root := s.svc.DataRoot
	code, err := CreateOnce(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(onceDir(root), code)
	held, err := os.ReadFile(path)
	if err != nil || len(held) == 0 {
		t.Fatalf("the code records no token tag: %v", err)
	}
	if _, err := s.rotateToken(); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.PrivateDir(onceDir(root)); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, held, 0o600); err != nil { // as if its removal had failed
		t.Fatal(err)
	}
	if ConsumeOnce(root, code, s.currentTag(), time.Now()) {
		t.Fatal("a code made with the old token signed in after the re-issue")
	}
	if fresh, err := CreateOnce(root); err != nil || !ConsumeOnce(root, fresh, s.currentTag(), time.Now()) {
		t.Fatalf("a code made with the new token was refused: %v", err)
	}
}

// Codes and saved sessions written by an older version record no tag. They still work when they are
// newer than the token file (an update keeps the browser signed in, an older menu bar icon still opens
// it) and are refused when they are older, since then they may be from before a re-issue.
func TestUntaggedCodesAndSessionsCountOnlyAfterTheToken(t *testing.T) {
	s, _ := testServer(t)
	root := s.svc.DataRoot
	now := time.Now()
	tokenAt := now.Add(-30 * time.Second)
	if err := os.Chtimes(TokenPath(root), tokenAt, tokenAt); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.PrivateDir(onceDir(root)); err != nil {
		t.Fatal(err)
	}
	untagged := func(code string, at time.Time) {
		t.Helper()
		p := filepath.Join(onceDir(root), code)
		if err := os.WriteFile(p, nil, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Chtimes(p, at, at); err != nil {
			t.Fatal(err)
		}
	}
	before, after := strings.Repeat("a", 36), strings.Repeat("b", 36)
	untagged(before, now.Add(-time.Minute))
	untagged(after, now)
	if ConsumeOnce(root, before, s.currentTag(), now) {
		t.Fatal("an untagged code older than the token signed in")
	}
	if !ConsumeOnce(root, after, s.currentTag(), now) {
		t.Fatal("an untagged code newer than the token was refused")
	}

	legacy := map[string]map[string]any{
		"before": {"CSRF": "c1", "Created": now.Add(-time.Minute), "Expires": now.Add(time.Hour), "Agent": "browser"},
		"after":  {"CSRF": "c2", "Created": now, "Expires": now.Add(time.Hour), "Agent": "browser"},
	}
	raw, _ := json.Marshal(legacy)
	if err := os.WriteFile(sessionsPath(root), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := New(s.svc)
	restarted.sessions.load(root, now, restarted.currentTag())
	if restarted.cookieSignsIn("before") {
		t.Fatal("an untagged session older than the token was restored")
	}
	if !restarted.cookieSignsIn("after") {
		t.Fatal("an untagged session newer than the token was not restored")
	}
}
