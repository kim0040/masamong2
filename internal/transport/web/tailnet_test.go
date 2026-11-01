//go:build !noweb

package web

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"sync/atomic"
	"testing"
	"time"
)

// tailscale serve forwards the tailnet name to the loopback listener with the Host header unchanged: the
// console answers to it only once the operator lists it in admin.web.hosts, and a write from that page
// passes the same-origin check.
func TestConsoleAnswersToAListedTailnetName(t *testing.T) {
	s, ts := testServer(t)
	c := client()
	csrf := signIn(t, s, ts, c)
	const host = "desk.tail1234.ts.net:8787"
	send := func(method, path string, body any) int {
		t.Helper()
		var raw []byte
		if body != nil {
			raw, _ = json.Marshal(body)
		}
		req, _ := http.NewRequest(method, ts.URL+path, bytes.NewReader(raw))
		req.Host = host
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		req.Header.Set("Origin", "http://"+host)
		res, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
		return res.StatusCode
	}
	if code := send("GET", "/api/session", nil); code != http.StatusMisdirectedRequest {
		t.Fatalf("an unlisted name is refused: %d", code)
	}
	if code, out := act(t, c, ts, csrf, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"admin", "web", "hosts"}, "value": []any{"http://desk.tail1234.ts.net"}}}}); code == 200 {
		t.Fatalf("a URL is not a host name: %v", out)
	}
	if code, out := act(t, c, ts, csrf, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"admin", "web", "hosts"}, "value": []any{"Desk.Tail1234.ts.net."}}}}); code != 200 {
		t.Fatalf("list the name: %d %v", code, out)
	}
	if got := s.svc.Doc().Admin.Web.Hosts; len(got) != 1 || got[0] != "desk.tail1234.ts.net" {
		t.Fatalf("normalized: %v", got)
	}
	if code := send("GET", "/api/session", nil); code != 200 {
		t.Fatalf("the listed name is answered: %d", code)
	}
	if code := send("POST", "/api/action", map[string]any{"action": "help.version"}); code != 200 {
		t.Fatalf("a write from the tailnet page: %d", code)
	}
	if s.listedHost("desk.tail1234.ts.net.evil.example") || s.listedHost("evil.example") {
		t.Fatal("only the exact listed name")
	}
}

func TestTailnetNameIsLookedUpInTheBackground(t *testing.T) {
	var calls atomic.Int32
	tn := tailnetName{look: func(context.Context) string { calls.Add(1); return "desk.tail1234.ts.net" }}
	if tn.get() != "" {
		t.Fatal("the first call does not wait for the look-up")
	}
	deadline := time.Now().Add(2 * time.Second)
	for tn.get() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if tn.get() != "desk.tail1234.ts.net" || calls.Load() != 1 {
		t.Fatalf("name %q after %d look-ups", tn.get(), calls.Load())
	}
}
