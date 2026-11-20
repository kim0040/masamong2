package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"masamong/internal/catalog"
)

// TestGeneratedFilesAreCurrent keeps docs/code-map in step with the code it indexes: when a
// catalog, command, tool or locale change makes fresh output differ, run go run ./tools/codemap.
func TestGeneratedFilesAreCurrent(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	files, err := generate(root)
	if err != nil {
		t.Fatal(err)
	}
	for name, want := range files {
		got, err := os.ReadFile(filepath.Join(root, name))
		if err != nil {
			t.Errorf("%s: %v (regenerate: go run ./tools/codemap)", name, err)
			continue
		}
		if string(got) != want {
			t.Errorf("%s is stale (regenerate: go run ./tools/codemap)", name)
		}
	}
}

// TestHandlerIndexIsLineIndependent pins the property the map is built on: it references files
// and functions, never lines, so inserting a line anywhere above a dispatch changes nothing.
// A blank line is added near the top of service.go and the handler index must come out the same.
func TestHandlerIndexIsLineIndependent(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "internal", "application")
	raw, err := os.ReadFile(filepath.Join(dir, "service.go"))
	if err != nil {
		t.Fatal(err)
	}
	base, err := loadPackage(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	shifted := strings.Replace(string(raw), "package application\n", "package application\n\n", 1)
	if shifted == string(raw) {
		t.Fatal("service.go: package clause not found")
	}
	moved, err := loadPackage(dir, map[string]string{"service.go": shifted})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := renderHandlers(moved), renderHandlers(base); got != want {
		t.Error("handler index changed after inserting a blank line")
	}
}

// renderHandlers is the test's view of handler resolution: every Action's handler and dispatch
// references, exactly as the generated index would list them.
func renderHandlers(idx *pkgIndex) string {
	ids := map[string]bool{}
	for _, d := range catalog.All() {
		ids[d.ID] = true
	}
	sites := collectSites(idx, ids)
	keys := make([]string, 0, len(sites))
	for id := range sites {
		keys = append(keys, id)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, id := range keys {
		b.WriteString(id + "\n")
		for _, s := range sites[id] {
			h := idx.handler(s)
			b.WriteString("  " + h.String() + " <- " + s.file.name + " · " + s.enc + "\n")
		}
	}
	return b.String()
}

// TestActionsIndexReferencesFunctions checks the map points at functions, not lines: finance.fx
// lands on financeFX in fx.go (through the finance relay), and no file:line reference appears.
func TestActionsIndexReferencesFunctions(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	out, err := actionsIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "[fx.go · financeFX](../../internal/application/fx.go)") {
		t.Error("actions index does not resolve finance.fx to fx.go · financeFX")
	}
	if !strings.Contains(out, "[images.go · images](../../internal/application/images.go)") {
		t.Error("actions index does not resolve images.generate to images.go · images")
	}
	if re := regexp.MustCompile(`internal/application/[a-z0-9_]+\.go:\d`); re.MatchString(out) {
		t.Errorf("actions index still carries a line number: %s", re.FindString(out))
	}
}
