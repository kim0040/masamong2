package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The repository's pages state one version, and it is the one of the newest release notes.
func TestRepositoryVersions(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range checkVersions(root) {
		t.Error(p)
	}
}

func TestPageVersion(t *testing.T) {
	for _, c := range []struct {
		page, want string
		ok         bool
	}{
		{"# Title\n\nVersion: `1.5.1`\n", "1.5.1", true},
		{"현재 버전: `1.5.1` · 라이선스: MIT · 저장소: [x](y)", "1.5.1", true},
		{"版本：`10.20.30`", "10.20.30", true},
		{"Versión: `1.5.1`", "1.5.1", true},
		{"Run `masamong open` (needs `1.5.1` or newer)\n", "", false}, // a label is a short phrase, not a sentence
		{"no version here\n", "", false},
		{strings.Repeat("line\n", 40) + "Version: `1.5.1`\n", "", false}, // only the first lines count
	} {
		got, ok := pageVersion(c.page)
		if got != c.want || ok != c.ok {
			t.Errorf("pageVersion(%q) = %q %v, want %q %v", c.page, got, ok, c.want, c.ok)
		}
	}
}

// versionTree writes a minimal tree: every page at version v, release notes for each of notes.
func versionTree(t *testing.T, v string, notes ...string) string {
	t.Helper()
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
	for _, f := range []string{"README.md", "README.ko.md", "README.ja.md", "docs/wiki/Home.md", "docs/wiki/English.md"} {
		write(f, "# Name\n\nVersion: `"+v+"`\n")
	}
	for _, n := range notes {
		write("release-notes/v"+n+".json", "{}")
	}
	return root
}

func TestCheckVersions(t *testing.T) {
	if p := checkVersions(versionTree(t, "1.5.1", "1.4.0", "1.5.1", "1.10.0")); len(p) == 0 {
		t.Fatal("1.5.1 pages with v1.10.0 notes must be reported (1.10.0 is newer than 1.5.1)")
	}
	if p := checkVersions(versionTree(t, "1.10.0", "1.4.0", "1.5.1", "1.10.0")); len(p) != 0 {
		t.Fatalf("pages at the newest notes pass: %v", p)
	}
	if p := checkVersions(versionTree(t, "1.5.1")); len(p) != 0 {
		t.Fatalf("a tree without release notes compares the pages with each other only: %v", p)
	}

	// one translation left behind
	root := versionTree(t, "1.5.1", "1.5.1")
	if err := os.WriteFile(filepath.Join(root, "README.ja.md"), []byte("バージョン: `1.0.0`\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := strings.Join(checkVersions(root), "\n"); !strings.Contains(p, "README.ja.md says 1.0.0, README.md says 1.5.1") {
		t.Fatalf("a README left behind is reported: %q", p)
	}

	// a wiki home page left behind
	root = versionTree(t, "1.5.1", "1.5.1")
	if err := os.WriteFile(filepath.Join(root, "docs", "wiki", "English.md"), []byte("Version `1.4.0` · License: MIT\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := strings.Join(checkVersions(root), "\n"); !strings.Contains(p, "docs/wiki/English.md says 1.4.0") {
		t.Fatalf("a wiki home page left behind is reported: %q", p)
	}

	// a page that lost its version line
	root = versionTree(t, "1.5.1", "1.5.1")
	if err := os.WriteFile(filepath.Join(root, "docs", "wiki", "Home.md"), []byte("# Home\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if p := strings.Join(checkVersions(root), "\n"); !strings.Contains(p, "docs/wiki/Home.md states no version") {
		t.Fatalf("a page without a version is reported: %q", p)
	}

	// README at the old version while release notes of a newer one exist: the bump was forgotten
	root = versionTree(t, "1.5.1", "1.5.1", "1.6.0")
	if p := strings.Join(checkVersions(root), "\n"); !strings.Contains(p, "README.md says 1.5.1, the newest release notes are v1.6.0") {
		t.Fatalf("a README behind the newest release notes is reported: %q", p)
	}
}
