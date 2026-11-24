package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A rendered diagram is fresh only while its stamps match the source and the style it names. Each way
// it can go wrong is reported, and a file that is no diagram is left alone.
func TestDiagramStamps(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		p := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	stamp := func(rel string) string {
		t.Helper()
		s, err := stampOf(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	svg := func(source, style string) string {
		return "<svg></svg>\n<!-- d2-source-sha256: " + source + " -->\n<!-- d2-style-sha256: " + style + " -->\n"
	}
	render := func(name string) {
		t.Helper()
		for _, mode := range diagramModes {
			write("docs/d/"+name+"-"+mode+".svg", svg(stamp("docs/d/"+name+".d2"), stamp("docs/d/_style-"+mode+".d2")))
		}
	}
	write("docs/d/_style-light.d2", "classes: {a: {style.fill: white}}\n")
	write("docs/d/_style-dark.d2", "classes: {a: {style.fill: black}}\n")
	write("docs/d/flow.d2", "a -> b\n")
	write("docs/d/logo.svg", "<svg></svg>\n") // an image that is no rendering
	render("flow")
	if got := checkDiagrams(root); len(got) != 0 {
		t.Fatalf("fresh diagrams reported: %q", got)
	}

	// a checkout with CRLF line endings is the same source
	write("docs/d/flow.d2", "a -> b\r\n")
	if got := checkDiagrams(root); len(got) != 0 {
		t.Fatalf("CRLF source reported: %q", got)
	}

	expect := func(what string, got []string, want ...string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: %d problems %q, want %d", what, len(got), got, len(want))
		}
		for i, w := range want {
			if !strings.Contains(got[i], w) {
				t.Fatalf("%s: problem %d is %q, want it to mention %q", what, i, got[i], w)
			}
		}
	}

	write("docs/d/flow.d2", "a -> b\nb -> c\n")
	expect("edited source", checkDiagrams(root),
		"flow-light.svg: rendered from an older docs/d/flow.d2", "flow-dark.svg: rendered from an older docs/d/flow.d2")
	render("flow")

	write("docs/d/_style-dark.d2", "classes: {a: {style.fill: navy}}\n")
	expect("edited style", checkDiagrams(root), "flow-dark.svg: rendered with an older docs/d/_style-dark.d2")
	render("flow")

	if err := os.Remove(filepath.Join(root, "docs", "d", "flow-dark.svg")); err != nil {
		t.Fatal(err)
	}
	expect("missing rendering", checkDiagrams(root), "docs/d/flow.d2: no docs/d/flow-dark.svg")
	render("flow")

	write("docs/d/flow-light.svg", "<svg></svg>\n")
	expect("no stamp", checkDiagrams(root), "flow-light.svg: no d2-source-sha256 stamp", "flow-light.svg: no d2-style-sha256 stamp")
	render("flow")

	write("docs/d/gone-light.svg", svg(strings.Repeat("0", 64), strings.Repeat("0", 64)))
	expect("rendering without a source", checkDiagrams(root), "docs/d/gone-light.svg: its .d2 source is gone")
	if err := os.Remove(filepath.Join(root, "docs", "d", "gone-light.svg")); err != nil {
		t.Fatal(err)
	}

	if err := os.Remove(filepath.Join(root, "docs", "d", "_style-light.d2")); err != nil {
		t.Fatal(err)
	}
	expect("missing style", checkDiagrams(root), "docs/d/flow.d2: no docs/d/_style-light.d2")
}
