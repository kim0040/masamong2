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

// A docs page in two languages is x.md (Korean) with x.en.md beside it, each starting with a switch to
// the other; a D2 source pairs the same way. The pair is a translation group, so one side edited alone
// is reported, and a twin without its page or without the switch is an error.
func TestTwinPages(t *testing.T) {
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
	write("README.md", "# x\n")
	write("docs/wiki/_Footer.md", "x\n")
	write("docs/a/page.md", "**한국어** | [English](page.en.md)\n\n# 쪽\n\n명령은 /날씨 다.\n")
	write("docs/a/page.en.md", "[한국어](page.md) | **English**\n\n# Page\n\nThe command is /weather here.\n")
	write("docs/a/d/flow.d2", "a: 가\n")
	write("docs/a/d/flow.en.d2", "a: A\n")
	write("docs/a/alone.md", "# 혼자\n")

	groups, err := translationGroups(root)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, g := range groups {
		names = append(names, g.Name+"="+strings.Join(g.Files, "+"))
	}
	if got, want := strings.Join(names, " "), "docs/a/page=docs/a/page.md+docs/a/page.en.md docs/a/d/flow.d2=docs/a/d/flow.d2+docs/a/d/flow.en.d2"; got != want {
		t.Fatalf("groups: %s, want %s", got, want)
	}
	if lang := pageLanguage(root, "docs/a/page.en.md", ""); lang != "en" {
		t.Fatalf("the twin's language: %q", lang)
	}
	if lang := pageLanguage(root, "docs/a/page.md", ""); lang != "ko" {
		t.Fatalf("the Korean page's language: %q", lang)
	}

	// recorded, then one side edited alone
	if err := writeLock(root, groups); err != nil {
		t.Fatal(err)
	}
	if got := checkTranslations(root); len(got) != 0 {
		t.Fatalf("a recorded pair is reported: %q", got)
	}
	write("docs/a/d/flow.d2", "a: 가\nb: 나\n")
	if got := checkTranslations(root); len(got) != 1 || !strings.Contains(got[0], "docs/a/d/flow.d2 changed") || !strings.Contains(got[0], "bring docs/a/d/flow.en.d2 along") {
		t.Fatalf("a diagram edited in one language: %q", got)
	}

	// a twin whose Korean page has no switch, and a twin without a page
	write("docs/a/page.md", "# 쪽\n")
	write("docs/a/lost.en.md", "[한국어](lost.md) | **English**\n")
	_, err = translationGroups(root)
	if err == nil || !strings.Contains(err.Error(), "docs/a/page.md (wants the line **한국어** | [English](page.en.md))") || !strings.Contains(err.Error(), "docs/a/lost.en.md (no docs/a/lost.md)") {
		t.Fatalf("unpaired twins: %v", err)
	}
}
