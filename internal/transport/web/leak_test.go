//go:build !noweb

package web

import (
	"bufio"
	"context"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"
)

// openStream opens the console's event stream and waits for its hello.
func openStream(t *testing.T, c *http.Client, url string) (cancel func(), lines *bufio.Reader) {
	t.Helper()
	ctx, stop := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	res, err := c.Do(req)
	if err != nil {
		stop()
		t.Fatal(err)
	}
	if res.StatusCode != 200 {
		stop()
		t.Fatalf("events %d", res.StatusCode)
	}
	br := bufio.NewReader(res.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			stop()
			t.Fatalf("no hello: %v", err)
		}
		if strings.HasPrefix(line, "event: hello") {
			break
		}
	}
	return func() { stop(); _ = res.Body.Close() }, br
}

func settledGoroutines(limit int) int {
	n := runtime.NumGoroutine()
	for deadline := time.Now().Add(5 * time.Second); n > limit && time.Now().Before(deadline); n = runtime.NumGoroutine() {
		time.Sleep(50 * time.Millisecond)
	}
	return n
}

// A console that is opened and closed all day (tabs, sign-ins, the event stream, practice mode, CLI
// commands) must come back to where it started: no goroutine per closed stream, no session per sign-in.
func TestConsoleUseLeavesNothingBehind(t *testing.T) {
	s, ts := testServer(t)
	cycle := func() {
		c := client()
		csrf := signIn(t, s, ts, c)
		closeStream, _ := openStream(t, c, ts.URL+"/api/events")
		for _, id := range []string{"help.version", "dashboard.overview", "config.show", "runtime.status"} {
			if code, out := act(t, c, ts, csrf, id, nil); code != 200 {
				t.Fatalf("%s %d %v", id, code, out)
			}
		}
		do(t, c, "GET", ts.URL+"/api/session", nil, nil)
		closeStream()
		if code, _, _ := do(t, c, "POST", ts.URL+"/api/logout", nil, map[string]string{"X-CSRF-Token": csrf}); code != 200 {
			t.Fatalf("logout %d", code)
		}
		c.CloseIdleConnections()
	}
	// warm up once (practice instance, lazily built state), then measure
	c := client()
	csrf := signIn(t, s, ts, c)
	if code, _, _ := do(t, c, "POST", ts.URL+"/api/practice", map[string]any{"do": "open"}, map[string]string{"X-CSRF-Token": csrf}); code != 200 {
		t.Fatalf("practice %d", code)
	}
	closePractice, _ := openStream(t, c, ts.URL+"/api/events?practice=1")
	closePractice()
	cycle()
	ts.CloseClientConnections()
	time.Sleep(300 * time.Millisecond)
	base := runtime.NumGoroutine()

	for range 150 {
		cycle()
	}
	ts.CloseClientConnections()
	if n := settledGoroutines(base + 3); n > base+3 {
		buf := make([]byte, 1<<20)
		t.Fatalf("goroutines %d after 150 console sessions (baseline %d)\n%s", n, base, buf[:runtime.Stack(buf, true)])
	}
	if n := s.sessions.len(); n != 1 { // the warm-up client never signed out
		t.Fatalf("%d sessions left after signing out", n)
	}
}

// Sign-ins that never sign out (scripts, lost cookies) are capped instead of kept for a week.
func TestSessionsAreCapped(t *testing.T) {
	st := newSessionStore()
	now := time.Now()
	first, _ := st.create(now, "")
	for i := range maxSessions * 3 {
		st.create(now.Add(time.Duration(i+1)*time.Second), "")
	}
	if n := st.len(); n != maxSessions {
		t.Fatalf("%d sessions, want %d", n, maxSessions)
	}
	if _, ok := st.get(first, now.Add(time.Hour)); ok {
		t.Fatal("the oldest sign-in should have gone first")
	}
}

// Signing out ends that browser's event stream instead of streaming changes to a signed-out tab.
func TestEventStreamEndsWithItsSession(t *testing.T) {
	s, ts := testServer(t)
	s.ping = 50 * time.Millisecond
	c := client()
	csrf := signIn(t, s, ts, c)
	closeStream, br := openStream(t, c, ts.URL+"/api/events")
	defer closeStream()
	if code, _, _ := do(t, c, "POST", ts.URL+"/api/logout", nil, map[string]string{"X-CSRF-Token": csrf}); code != 200 {
		t.Fatalf("logout %d", code)
	}
	// the check runs with the keep-alive ping (shortened here)
	ended := make(chan error, 1)
	go func() {
		for {
			if _, err := br.ReadString('\n'); err != nil {
				ended <- err
				return
			}
		}
	}()
	select {
	case <-ended:
	case <-time.After(5 * time.Second):
		t.Fatal("the event stream stayed open after sign-out")
	}
}
