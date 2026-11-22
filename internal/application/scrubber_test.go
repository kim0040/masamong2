package application

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/i18n"
	"masamong/internal/tools"
	"masamong/locales"
)

// The patterns are built once per tool call; scrubbing each of thousands of strings must not compile.
func TestScrubberDoesNotCompilePerString(t *testing.T) {
	sc := newScrubber("synthetic/credential+value")
	if !sc.active() {
		t.Fatal("a 26-character credential must be scrubbed")
	}
	if got := sc.text("key=synthetic%2Fcredential%2bvalue and synthetic/credential+value"); strings.Contains(got, "credential") {
		t.Fatalf("not scrubbed: %q", got)
	}
	if n := testing.AllocsPerRun(200, func() { _ = sc.text("an ordinary line of a tool response with no secret in it") }); n > 8 {
		t.Fatalf("%v allocations per string: the scrubber is rebuilding its patterns", n)
	}
}

// A credential shorter than 8 characters would also erase ordinary words, so it cannot be scrubbed
// from a response: the call is not made, the caller gets a configuration error and the operator a log line.
func TestShortCredentialIsNotSent(t *testing.T) {
	t.Setenv("TOOL_SHORT_CREDENTIAL", "abc")
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte("abc is the start of the alphabet"))
	}))
	defer srv.Close()
	var logs bytes.Buffer
	cat := i18n.New()
	if err := cat.LoadFS(locales.FS); err != nil {
		t.Fatal(err)
	}
	s := &Service{Log: slog.New(slog.NewTextHandler(&logs, nil)), I18n: cat}
	h := &tools.HTTP{URL: srv.URL + "?key={credential}", CredentialRef: "env:TOOL_SHORT_CREDENTIAL"}
	r := s.runHTTPTool(context.Background(), action.Request{Locale: "en"}, tools.Resolved{ID: "echo", HTTP: h})
	if r.OK || r.Error.Code != errcode.ConfigInvalid || r.Error.MessageKey != "tool.key_too_short" || !strings.Contains(r.Error.Message, "echo") {
		t.Fatalf("a key too short to scrub was used: %+v", r)
	}
	if hits != 0 {
		t.Fatalf("the server was called %d times", hits)
	}
	if !strings.Contains(logs.String(), "too short to scrub") {
		t.Fatalf("no warning logged: %s", logs.String())
	}
}

// An operator tool whose stored configuration has a key written into a header (accepted by an earlier
// release) still runs, and each call warns the operator without logging the value.
func TestDirectKeyInAStoredToolWarns(t *testing.T) {
	direct := "synthetic" + "-direct-value"
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()
	var logs bytes.Buffer
	s := &Service{Log: slog.New(slog.NewTextHandler(&logs, nil))}
	h := &tools.HTTP{URL: srv.URL, Headers: map[string]string{"X-API-Key": direct}}
	r := s.runHTTPTool(context.Background(), action.Request{Locale: "en"}, tools.Resolved{ID: "ops.lookup", HTTP: h})
	if !r.OK || hits != 1 {
		t.Fatalf("the stored tool stopped working: %+v hits %d", r, hits)
	}
	if out := logs.String(); !strings.Contains(out, "keeps a key in its configuration") || !strings.Contains(out, "headers.X-API-Key") || strings.Contains(out, direct) {
		t.Fatalf("log: %s", out)
	}
}
