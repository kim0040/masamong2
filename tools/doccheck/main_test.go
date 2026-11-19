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
	for _, p := range append(append(checkLinks(root), checkTranslations(root)...), checkCommands(root)...) {
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

// The command-name rule, end to end: aside forms, particles, the ASCII-prefix guard, fences, the
// ignore marker, path-like tokens, prefix-only words, page language detection and the exclusions.
func TestCommandNamesInDocs(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		p := filepath.Join(root, filepath.FromSlash(rel))
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("internal/connectors/discord/commands.go", `package discord

func Commands() []Command {
	return []Command{
		{Name: "weather", Desc: "d", Action: "weather.query"},
		{Name: "help", Desc: "d", Action: "help.version"},
		{Name: "subscribe", Desc: "d", Action: "fortune.subscribe", PrefixOnly: true},
	}
}
`)
	write("locales/ko.json", `{"discord.command.weather.name":"날씨","discord.command.help.name":"도움","discord.command.subscribe.name":"구독"}`)
	write("locales/en.json", `{}`)
	write("docs/page.md", `# 페이지

날씨는 /날씨 로 묻는다.
도움말은 /도움 이고 /날씨를 쓴다.
영어 이름 /weather 는 틀린다.
영어 이름 /weather를 붙이면 안 된다.
별명은 `+"`"+`/날씨`+"`"+`(`+"`"+`/weather`+"`"+`) 와 `+"`"+`/날씨`+"`"+` (`+"`"+`/weather`+"`"+`) 다.
폼은 /날씨(/weather) 도 되고 `+"`"+`/날씨`+"`"+`（`+"`"+`/weather`+"`"+`）도 된다.
경로 ./weather ~/weather path=/weather "/weather" '/weather' /weather/x 는 명령이 아니다.
구독 단어 /subscribe 와 /구독 은 슬래시 명령이 아니다.
접두 /weathers /weather-station 은 다른 단어다.
마지막 /weather/ 경로도 아니다.

`+"```bash\n"+`날씨 /weather 예시
`+"```"+`

Finnhub 경로 /weather 안내 <!-- doccheck:not-command -->
`)
	write("docs/en.md", `# Page

The weather command is /weather and help is /help.
A Korean name /날씨 is wrong here.
The prefix word /구독 is not a slash command at all.
`)
	// a page written in both languages accepts any locale's name
	write("docs/mixed.md", `# Gallery

한국어 절입니다.
설명이 이어집니다.
더 있습니다.
영어와 한국어가 섞입니다.
English section follows here.
More English text in this section.
And even more English prose.
/weather 와 /날씨 둘 다 쓸 수 있다.
`)
	// historical records keep the names of their time
	for _, f := range []string{"docs/postmortems/p.md", "docs/adr/a.md", "docs/plan-x.md"} {
		write(f, "# 기록\n\n한국어 설명이 있고 /weather 를 그대로 둔다.\n또 설명이 이어진다.\n더 이어진다.\n")
	}
	got := checkCommands(root)
	want := []string{
		"docs/en.md:4: /날씨 is a ko command name; this page is en, which shows /weather",
		"docs/page.md:5: /weather is a en command name; this page is ko, which shows /날씨",
		"docs/page.md:6: /weather를 is a en command name; this page is ko, which shows /날씨",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Fatalf("problems:\n%s", strings.Join(got, "\n"))
	}
}

// The real Korean pages heavy with code, URLs and English terms are judged Korean, and the
// multi-language pages are not forced to one language.
func TestPageLanguageOfUnpairedDocs(t *testing.T) {
	root, err := repoRoot()
	if err != nil {
		t.Fatal(err)
	}
	for f, want := range map[string]string{
		"docs/TEST_SANDBOX.md":                 "ko",
		"docs/REPO_MAP.md":                     "ko",
		"docs/rag/README.md":                   "ko",
		"docs/rag/evaluation.md":               "ko",
		"docs/rag/retrieval-v2.md":             "ko",
		"docs/license-audit.md":                "ko",
		"docs/README.md":                       "ko",
		"docs/wiki/_Sidebar.md":                "",
		"docs/wiki/_Footer.md":                 "",
		"docs/images/readme/README.md":         "",
		"docs/wiki/설정-레퍼런스.md":                 "ko",
		"docs/wiki/Configuration-Reference.md": "en",
	} {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			t.Fatal(err)
		}
		if got := pageLanguage(root, f, string(raw)); got != want {
			t.Errorf("pageLanguage(%s) = %q, want %q", f, got, want)
		}
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
