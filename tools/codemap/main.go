// codemap writes the generated code indexes under docs/code-map so a reviewer (or an agent) can
// find where a feature lives without searching. The index can never go stale: the package test
// regenerates the output and fails when a committed file differs, and -check does the same for a
// manual or CI gate.
//
//	go run ./tools/codemap          write docs/code-map/*.md
//	go run ./tools/codemap -check   fail when a committed file differs from fresh output
package main

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// outDir is the generated files' home, relative to the repository root.
const outDir = "docs/code-map"

func main() {
	check := flag.Bool("check", false, "fail when a committed file differs from what the generator produces")
	flag.Parse()
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "codemap:", err)
		os.Exit(2)
	}
	files, err := generate(root)
	if err != nil {
		fmt.Fprintln(os.Stderr, "codemap:", err)
		os.Exit(1)
	}
	if *check {
		var stale []string
		for name, want := range files {
			raw, err := os.ReadFile(filepath.Join(root, name))
			if err != nil || string(raw) != want {
				stale = append(stale, name)
			}
		}
		sort.Strings(stale)
		if len(stale) > 0 {
			fmt.Println("codemap: stale generated file(s):")
			for _, s := range stale {
				fmt.Println("  " + s)
			}
			fmt.Println("regenerate with: go run ./tools/codemap")
			os.Exit(1)
		}
		fmt.Println("codemap: generated files are current")
		return
	}
	dir := filepath.Join(root, outDir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, "codemap:", err)
		os.Exit(1)
	}
	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := os.WriteFile(filepath.Join(root, name), []byte(files[name]), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "codemap:", err)
			os.Exit(1)
		}
	}
	fmt.Printf("codemap: wrote %s\n", strings.Join(names, ", "))
}

// repoRoot finds the directory holding go.mod above the working directory.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod above the working directory")
		}
		dir = parent
	}
}

// generate renders every index file, keyed by its root-relative path. Output is deterministic:
// rows and multi-value cells are sorted and no dates, times or SHAs appear, so a diff only ever
// shows a real change.
func generate(root string) (map[string]string, error) {
	actions, err := actionsIndex(root)
	if err != nil {
		return nil, err
	}
	return map[string]string{
		outDir + "/actions.md":  actions,
		outDir + "/commands.md": commandsIndex(),
		outDir + "/tools.md":    toolsIndex(),
		outDir + "/locales.md":  localesIndex(),
	}, nil
}

// table renders a Markdown table; one row per rows entry, every cell escaped.
func table(header []string, rows [][]string) string {
	var b strings.Builder
	b.WriteString("| " + strings.Join(header, " | ") + " |\n")
	b.WriteString("|" + strings.Repeat("---|", len(header)) + "\n")
	for _, r := range rows {
		b.WriteString("| " + strings.Join(r, " | ") + " |\n")
	}
	return b.String()
}

// cell keeps one value inside a table cell: | would split the row and a newline would end it.
func cell(s string) string {
	s = strings.ReplaceAll(s, "|", "\\|")
	return strings.Join(strings.Fields(s), " ")
}

func orDash(s string) string {
	if s = cell(s); s == "" {
		return "—"
	}
	return s
}
