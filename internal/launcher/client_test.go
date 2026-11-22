package launcher

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// ActionPoll sends the poll header, Action does not.
func TestClientPollHeader(t *testing.T) {
	var polls []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/login":
			_, _ = w.Write([]byte(`{"ok":true,"csrf":"c"}`))
		case "/api/action":
			polls = append(polls, r.Header.Get(PollHeader))
			_, _ = w.Write([]byte(`{"ok":true,"output":{}}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()
	c := &Client{Base: srv.URL, token: "t", Via: "tray", http: srv.Client()}
	if _, err := c.Action(context.Background(), "runtime.status", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ActionPoll(context.Background(), "runtime.status", nil); err != nil {
		t.Fatal(err)
	}
	if len(polls) != 2 || polls[0] != "" || polls[1] != "1" {
		t.Fatalf("poll headers %v, want [\"\" \"1\"]", polls)
	}
}
