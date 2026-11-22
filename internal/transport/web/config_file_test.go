//go:build !noweb

package web

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"masamong/internal/action"
)

// The console cannot make the service read a file: config.diff, config.validate and config.apply take
// the document as yaml only, and a path is refused without being opened.
func TestConsoleConfigActionsDoNotReadFiles(t *testing.T) {
	s, ts := testServer(t)
	c := client()
	csrf := signIn(t, s, ts, c)
	// a valid document, so a service that read it would show it in a diff or apply it
	current := s.svc.Execute(t.Context(), action.Request{Action: "config.show", Locale: "en"})
	yaml, _ := current.Output["yaml"].(string)
	const marker = "synthetic-outside"
	if !strings.Contains(yaml, "id: web") {
		t.Fatalf("unexpected settings:\n%s", yaml)
	}
	outside := filepath.Join(t.TempDir(), "elsewhere.yaml")
	if err := os.WriteFile(outside, []byte(strings.Replace(yaml, "id: web", "id: "+marker, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"config.diff", "config.validate", "config.apply"} {
		code, out := act(t, c, ts, csrf, id, map[string]any{"file": outside, "base_revision": s.svc.Revision()})
		raw, _ := json.Marshal(out)
		if code == 200 || out["ok"] != false || strings.Contains(string(raw), marker) {
			t.Fatalf("%s with a file: %d %s", id, code, raw)
		}
	}
	if s.svc.Doc().Instance.ID == marker {
		t.Fatal("the outside file was applied")
	}
	// the document itself is still accepted
	if code, out := act(t, c, ts, csrf, "config.diff", map[string]any{"yaml": yaml}); code != 200 || out["ok"] != true {
		t.Fatalf("diff with yaml: %d %v", code, out)
	}
}
