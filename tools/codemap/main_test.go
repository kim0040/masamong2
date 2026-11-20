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
		if strings.ReplaceAll(string(got), "\r\n", "\n") != want {
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
	src := strings.ReplaceAll(string(raw), "\r\n", "\n") // a Windows checkout has CRLF line ends
	shifted := strings.Replace(src, "package application\n", "package application\n\n", 1)
	if shifted == src {
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

// TestActionsIndexPinsHandlers keeps the handler column honest: a wrong row regenerates
// "successfully" forever, so handlers that were wrong once (or could drift into helpers) are
// pinned against the source here. The second part asserts the names that are never handlers —
// guards, audits, predicates, helper calls and package variables — appear in no row.
func TestActionsIndexPinsHandlers(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	out, err := actionsIndex(root)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string]string{}
	for _, l := range strings.Split(out, "\n") {
		cells := strings.Split(l, " | ")
		if len(cells) < 2 || !strings.HasPrefix(cells[0], "| ") {
			continue
		}
		rows[strings.TrimPrefix(cells[0], "| ")] = strings.TrimSuffix(cells[len(cells)-1], " |")
	}
	want := map[string]string{
		"people.merge":            "memory_review.go · peopleAction",
		"personas.apply":          "channels.go · personas",
		"models.select":           "providers_persona.go · providers",
		"providers.remove":        "providers_persona.go · providers",
		"models.list":             "providers_persona.go · providers",
		"providers.test":          "providers_persona.go · providers",
		"agent.simulate":          "agent_backup.go · agent",
		"imports.start":           "memory.go · memoryActions",
		"lab.review":              "admin.go · labReviews",
		"config.snapshot.restore": "ops_extra.go · snapshotsAction",
		"finance.fx":              "fx.go · financeFX",
		"images.generate":         "images.go · images",
	}
	for id, h := range want {
		file, _, _ := strings.Cut(h, " ")
		link := "[" + h + "](../../internal/application/" + file + ")"
		if rows[id] != link {
			t.Errorf("%s: handler = %s, want %s", id, rows[id], link)
		}
	}
	for _, n := range []string{"readForm", "audit", "gateDiscord", "practiceBlocked", "boolVal",
		"withFeeBounds", "providerFail", "evidenceWriteError", "mutateConfig", "applyCandidate"} {
		if strings.Contains(out, " · "+n+"](") {
			t.Errorf("actions index still names %s as a handler", n)
		}
	}
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
