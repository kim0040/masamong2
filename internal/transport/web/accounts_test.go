//go:build !noweb

package web

import (
	"context"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"masamong/internal/fsutil"
)

func TestPracticeHeaderRoutesToPracticeInstance(t *testing.T) {
	s, ts := testServer(t)
	c := client()
	csrf := signIn(t, s, ts, c)
	code, out, _ := do(t, c, "POST", ts.URL+"/api/practice", map[string]any{"do": "open"}, map[string]string{"X-CSRF-Token": csrf})
	if code != 200 || out["open"] != true {
		t.Fatalf("open practice %d %v", code, out)
	}
	h := map[string]string{"X-CSRF-Token": csrf, practiceHeader: "1"}
	code, out, _ = do(t, c, "POST", ts.URL+"/api/action", map[string]any{"action": "config.show"}, h)
	if code != 200 || out["output"].(map[string]any)["instance"] != "practice" {
		t.Fatalf("practice config %d %v", code, out["output"])
	}
	code, out, _ = do(t, c, "POST", ts.URL+"/api/action", map[string]any{"action": "config.show"}, map[string]string{"X-CSRF-Token": csrf})
	if code != 200 || out["output"].(map[string]any)["instance"] != "web" {
		t.Fatalf("real config %d %v", code, out["output"])
	}
	// preferences belong to the admin, not the instance
	do(t, c, "POST", ts.URL+"/api/action", map[string]any{"action": "admin.prefs.set", "input": map[string]any{"prefs": map[string]any{"theme": "dark"}}}, h)
	_, out = act(t, c, ts, csrf, "admin.prefs", nil)
	if out["output"].(map[string]any)["prefs"].(map[string]any)["theme"] != "dark" {
		t.Fatal("prefs set in practice mode must land in the real instance")
	}
	if code, _, _ := do(t, c, "POST", ts.URL+"/api/action", map[string]any{"action": "runtime.restart"}, h); code == 200 {
		t.Fatal("restart must be refused in practice mode")
	}
	if code, _, _ := do(t, c, "POST", ts.URL+"/api/practice", map[string]any{"do": "reset"}, map[string]string{"X-CSRF-Token": csrf}); code != 200 {
		t.Fatalf("reset %d", code)
	}
}

func TestSessionsSurviveARestart(t *testing.T) {
	s, ts := testServer(t)
	c := client()
	csrf := signIn(t, s, ts, c)
	if err := s.sessions.save(s.svc.DataRoot, s.now()); err != nil {
		t.Fatal(err)
	}
	// the restarted service: same data folder, new server
	s2 := New(s.svc)
	s2.sessions.load(s.svc.DataRoot, s2.now())
	ts2 := httptest.NewServer(s2.Handler())
	defer ts2.Close()
	if code, _ := act(t, c, ts2, csrf, "dashboard.overview", nil); code != 200 {
		t.Fatalf("session lost across restart: %d", code)
	}
	if _, err := os.Stat(sessionsPath(s.svc.DataRoot)); err == nil {
		t.Fatal("sessions.json must be consumed on start")
	}
}

func TestSessionPersistenceFiltersExpiredAndUsesPrivateMode(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 9, 24, 3, 0, 0, 0, time.UTC)
	st := newSessionStore()
	liveID, _ := st.create(now, "test browser")
	otherID, _ := st.create(now, "other browser")
	st.mu.Lock()
	st.byID["expired-at-boundary"] = session{Expires: now}
	st.byID["already-expired"] = session{Expires: now.Add(-time.Second)}
	st.mu.Unlock()
	revokedID, _ := st.create(now, "test browser")
	st.drop(revokedID)

	path := sessionsPath(dir)
	if err := os.WriteFile(path, []byte("stale"), 0o666); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o666); err != nil {
		t.Fatal(err)
	}
	if err := st.save(dir, now); err != nil {
		t.Fatal(err)
	}
	if err := fsutil.VerifyPrivateFile(path); err != nil {
		t.Fatalf("sessions file permissions: %v", err)
	}

	restored := newSessionStore()
	restored.load(dir, now)
	if _, ok := restored.get(liveID, now); !ok {
		t.Fatal("live session was not restored")
	}
	if sess, ok := restored.get(otherID, now); !ok || sess.CSRF == "" {
		t.Fatalf("second session was not restored: %+v, ok=%v", sess, ok)
	}
	for _, id := range []string{"expired-at-boundary", "already-expired", revokedID} {
		if _, ok := restored.get(id, now); ok {
			t.Fatal("expired or revoked session was restored")
		}
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("sessions file must be consumed on load")
	}

	// Saving an empty store removes any stale handoff instead of reviving it later.
	if err := os.WriteFile(path, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := newSessionStore().save(dir, now); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("empty session handoff must remove stale file")
	}
}

func TestListenPersistsSessionsOnlyForRestart(t *testing.T) {
	for _, tc := range []struct {
		name     string
		restart  bool
		wantFile bool
	}{{"restart", true, true}, {"stop-or-signal", false, false}} {
		t.Run(tc.name, func(t *testing.T) {
			s, ts := testServer(t)
			ts.Close()
			s.addr = "127.0.0.1:0"
			s.sessions.create(s.now(), "test browser")
			if tc.restart {
				s.PreserveSessionsForRestart()
			}
			ready := make(chan struct{})
			s.OnListen = func(string) { close(ready) }
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan error, 1)
			go func() { done <- s.Listen(ctx) }()
			select {
			case <-ready:
			case <-time.After(5 * time.Second):
				t.Fatal("listener did not start")
			}
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("listener did not stop")
			}
			_, err := os.Stat(sessionsPath(s.svc.DataRoot))
			if tc.wantFile && err != nil {
				t.Fatal("restart did not persist sessions")
			}
			if !tc.wantFile && !os.IsNotExist(err) {
				t.Fatal("stop or signal persisted sessions")
			}
		})
	}
}

func TestRestartReusesTheAddressAlreadyOpenInTheBrowser(t *testing.T) {
	s, ts := testServer(t)
	ts.Close()
	s.UseRestartBind("127.0.0.1:0")

	listenOnce := func(server *Server) string {
		t.Helper()
		ready := make(chan string, 1)
		server.OnListen = func(addr string) { ready <- addr }
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- server.Listen(ctx) }()
		var addr string
		select {
		case addr = <-ready:
		case <-time.After(5 * time.Second):
			t.Fatal("listener did not start")
		}
		cancel()
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("listener did not stop")
		}
		return addr
	}

	first := listenOnce(s)
	restarted := New(s.svc)
	restarted.Force = true
	restarted.UseRestartBind(first)
	if second := listenOnce(restarted); second != first {
		t.Fatalf("restart changed browser address: %s -> %s", first, second)
	}
}
