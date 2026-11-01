package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The repository's own documentation passes: links resolve and every translation group is in step.
func TestRepositoryDocs(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range append(checkLinks(root), checkTranslations(root)...) {
		t.Error(p)
	}
}

func TestLinkTargets(t *testing.T) {
	for _, c := range []struct {
		from, link, want string
		checked          bool
	}{
		{"docs/BACKLOG.md", "wiki/보류-중인-결정.md", "docs/wiki/보류-중인-결정.md", true},
		{"docs/BACKLOG.md", "license-audit.md#external", "docs/license-audit.md", true},
		{"README.md", "docs/images/readme/home.png", "docs/images/readme/home.png", true},
		{"docs/wiki/설치.md", repoURL + "/wiki/%EC%84%A4%EC%B9%98#update", "docs/wiki/설치.md", true},
		{"docs/wiki/Home.md", repoURL + "/wiki/Quick-Start", "docs/wiki/Quick-Start.md", true},
		{"README.md", repoURL + "/raw/main/docs/images/readme/home.png", "docs/images/readme/home.png", true},
		{"README.md", repoURL + "/blob/main/SECURITY.md", "SECURITY.md", true},
		{"README.md", repoURL + "/releases", "", false},
		{"README.md", "https://go.dev/dl/", "", false},
		{"README.md", "#install", "", false},
		{"README.md", "mailto:someone@example.com", "", false},
	} {
		got, ok := linkTarget(c.from, c.link)
		if ok != c.checked || got != c.want {
			t.Errorf("linkTarget(%s, %s) = %q %v, want %q %v", c.from, c.link, got, ok, c.want, c.checked)
		}
	}
	if l := links("see [a](x.md) and `[b](y.md)`\n```\n[c](z.md)\n```\n<img src=\"i.png\">"); strings.Join(l, ",") != "x.md,i.png" {
		t.Fatalf("links outside code: %v", l)
	}
}

// A link to a file that exists here but is git-ignored is missing on GitHub and in CI: it is reported.
// A git-ignored page itself is not published, so its links are not checked.
func TestLinksToGitIgnoredFiles(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
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
	if out, err := exec.Command("git", "-C", root, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git init: %v %s", err, out)
	}
	write(".gitignore", "/docs/private/\n/NOTES.md\n")
	write("docs/private/plan.md", "x")
	write("docs/public.md", "y")
	write("docs/README.md", "[plan](private/plan.md) [public](public.md)")
	write("NOTES.md", "[plan](docs/private/plan.md)")
	got := checkLinks(root)
	if len(got) != 1 || !strings.Contains(got[0], "docs/README.md") || !strings.Contains(got[0], "git-ignored") {
		t.Fatalf("problems: %q", got)
	}
}

// One language edited alone fails the check until the other is brought along and recorded.
func TestTranslationDrift(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module x\n")
	write("README.md", "# X\n\n**English** | [한국어](README.ko.md)\n")
	write("README.ko.md", "# X\n\n[English](README.md) | **한국어**\n")
	write("docs/wiki/설치.md", "# 설치\n\n**한국어** | [English]("+repoURL+"/wiki/Installation)\n")
	write("docs/wiki/Installation.md", "# Installation\n\n[한국어]("+repoURL+"/wiki/설치) | **English**\n")
	groups, err := translationGroups(root)
	if err != nil || len(groups) != 2 {
		t.Fatalf("groups %v %v", groups, err)
	}
	if err := writeLock(root, groups); err != nil {
		t.Fatal(err)
	}
	if p := checkTranslations(root); len(p) != 0 {
		t.Fatalf("fresh record: %v", p)
	}
	write("docs/wiki/설치.md", "# 설치\n\n**한국어** | [English]("+repoURL+"/wiki/Installation)\n\n새 절\n")
	p := checkTranslations(root)
	if len(p) != 1 || !strings.Contains(p[0], "설치.md changed") || !strings.Contains(p[0], "bring docs/wiki/Installation.md along") {
		t.Fatalf("drift: %v", p)
	}
	// a page without a translation is reported
	write("docs/wiki/새-페이지.md", "# 새 페이지\n")
	if _, err := translationGroups(root); err == nil || !strings.Contains(err.Error(), "새-페이지.md") {
		t.Fatalf("unpaired page: %v", err)
	}
}
