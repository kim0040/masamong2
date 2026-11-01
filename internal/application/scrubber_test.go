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
	"masamong/internal/tools"
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

// A credential shorter than 8 characters would also erase ordinary words, so it is not scrubbed, and
// the operator is told in the log.
func TestShortCredentialIsNotScrubbedButWarned(t *testing.T) {
	t.Setenv("TOOL_SHORT_CREDENTIAL", "abc")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("abc is the start of the alphabet"))
	}))
	defer srv.Close()
	var logs bytes.Buffer
	s := &Service{Log: slog.New(slog.NewTextHandler(&logs, nil))}
	h := &tools.HTTP{URL: srv.URL + "?key={credential}", CredentialRef: "env:TOOL_SHORT_CREDENTIAL"}
	r := s.runHTTPTool(context.Background(), action.Request{}, tools.Resolved{ID: "echo", HTTP: h})
	if !r.OK {
		t.Fatal(r.Error)
	}
	if out, _ := r.Output["data_text"].(string); !strings.Contains(out, "abc is the start") {
		t.Fatalf("ordinary text was mangled: %v", r.Output)
	}
	if !strings.Contains(logs.String(), "too short to scrub") {
		t.Fatalf("no warning logged: %s", logs.String())
	}
}
