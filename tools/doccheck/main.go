// doccheck keeps the documentation honest, in two ways:
//
//   - links: every relative link, and every link to this repository's files or wiki pages, points at a
//     file that exists (wiki links had broken before; nothing caught them);
//
//   - translations: a page and its translations move together. docs/translations.json records a hash of
//     every file of every translated group (the five READMEs, each Korean wiki page and its English
//     page). When one file changes and the record no longer matches, the check fails until the other
//     languages are brought along and the record is written again. The idea comes from DeepSeek
//     Harness's translation-pairing gate.
//
//     go run ./tools/doccheck          check (go test ./tools/doccheck runs the same)
//     go run ./tools/doccheck -write   after updating every language of a changed page, record them
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const (
	repoURL  = "https://github.com/kim0040/masamong2"
	lockFile = "docs/translations.json"
	wikiDir  = "docs/wiki"
)

// frozen holds records kept as they were written (the early plans and work orders); their links point
// at paths of that time and are not maintained.
var frozen = map[string]bool{"docs/archive": true}

// skipDirs are not documentation this repository maintains (or are copies, data and build output).
var skipDirs = map[string]bool{
	".git": true, "node_modules": true, "test-sandbox": true, ".private": true, "output": true, "build": true,
	".masamong-scratch": true, "vendor": true, "dist": true, ".claude": true, ".agents": true,
}

func main() {
	write := flag.Bool("write", false, "record the current translation hashes (after updating every language)")
	flag.Parse()
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "doccheck:", err)
		os.Exit(2)
	}
	if *write {
		groups, err := translationGroups(root)
		if err == nil {
			err = writeLock(root, groups)
		}
		if err != nil {
			fmt.Fprintln(os.Stderr, "doccheck:", err)
			os.Exit(1)
		}
		fmt.Printf("recorded %d translation groups in %s\n", len(groups), lockFile)
	}
	problems := append(checkLinks(root), checkTranslations(root)...)
	for _, p := range problems {
		fmt.Println(p)
	}
	if len(problems) > 0 {
		fmt.Printf("\n%d problem(s)\n", len(problems))
		os.Exit(1)
	}
	fmt.Println("doccheck: links and translations ok")
}

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
			return "", errors.New("no go.mod above the working directory")
		}
		dir = parent
	}
}

// markdownFiles lists the maintained .md files, slash-separated and relative to root.
func markdownFiles(root string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			rel, _ := filepath.Rel(root, p)
			if p != root && (skipDirs[d.Name()] || strings.HasPrefix(d.Name(), ".") || frozen[filepath.ToSlash(rel)]) {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasSuffix(p, ".md") {
			rel, _ := filepath.Rel(root, p)
			out = append(out, filepath.ToSlash(rel))
		}
		return nil
	})
	sort.Strings(out)
	return out, err
}

var (
	fenced     = regexp.MustCompile("(?ms)^\\s*(```|~~~).*?^\\s*(```|~~~)")
	inlineCode = regexp.MustCompile("`[^`\n]*`")
	mdLink     = regexp.MustCompile(`\]\(\s*<?([^)\s>]+)>?(?:\s+"[^"]*")?\s*\)`)
	refLink    = regexp.MustCompile(`(?m)^\s*\[[^\]]+\]:\s*(\S+)`)
	htmlSrc    = regexp.MustCompile(`(?:src|href)="([^"]+)"`)
)

// links returns the link targets of a Markdown text, outside code.
func links(text string) []string {
	text = fenced.ReplaceAllString(text, "")
	text = inlineCode.ReplaceAllString(text, "")
	var out []string
	for _, re := range []*regexp.Regexp{mdLink, refLink, htmlSrc} {
		for _, m := range re.FindAllStringSubmatch(text, -1) {
			out = append(out, m[1])
		}
	}
	return out
}

// checkLinks reports links to files or wiki pages that do not exist.
func checkLinks(root string) []string {
	files, err := markdownFiles(root)
	if err != nil {
		return []string{"links: " + err.Error()}
	}
	exists := func(rel string) bool {
		_, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel)))
		return err == nil
	}
	// a git-ignored page (the local AGENTS.md) is not published, so its links are not checked
	skip := gitIgnored(root, files)
	var problems []string
	type found struct{ file, target, want string }
	var present []found
	for _, f := range files {
		if skip[f] {
			continue
		}
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(f)))
		if err != nil {
			problems = append(problems, f+": "+err.Error())
			continue
		}
		for _, target := range links(string(raw)) {
			want, ok := linkTarget(f, target)
			switch {
			case !ok:
			case !exists(want):
				problems = append(problems, fmt.Sprintf("%s: broken link %s (no %s)", f, target, want))
			default:
				present = append(present, found{f, target, want})
			}
		}
	}
	// a file that is here but git-ignored (docs/archive) is missing on GitHub and in CI
	wants := make([]string, len(present))
	for i, p := range present {
		wants[i] = p.want
	}
	ignored := gitIgnored(root, wants)
	for _, p := range present {
		if ignored[p.want] {
			problems = append(problems, fmt.Sprintf("%s: link %s points at a git-ignored file (%s), missing on GitHub", p.file, p.target, p.want))
		}
	}
	return problems
}

// gitIgnored reports which of the paths git ignores. Without git (a source archive) nothing is.
func gitIgnored(root string, paths []string) map[string]bool {
	out := map[string]bool{}
	if len(paths) == 0 {
		return out
	}
	cmd := exec.Command("git", "-C", root, "check-ignore", "--stdin")
	cmd.Stdin = strings.NewReader(strings.Join(paths, "\n") + "\n")
	raw, _ := cmd.Output() // exit 1: none ignored; other failures: treat as none
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		if line != "" {
			out[line] = true
		}
	}
	return out
}

// linkTarget is the repository file a link from file f must find, or false for links not checked here
// (other sites, in-page anchors, mail, this repository's issues and releases).
func linkTarget(f, target string) (string, bool) {
	if i := strings.IndexAny(target, "#?"); i >= 0 {
		target = target[:i]
	}
	if target == "" || strings.HasPrefix(target, "mailto:") || strings.HasPrefix(target, "data:") {
		return "", false
	}
	if strings.HasPrefix(target, "http://") || strings.HasPrefix(target, "https://") {
		rest, ok := strings.CutPrefix(target, repoURL+"/")
		if !ok {
			return "", false
		}
		if page, ok := strings.CutPrefix(rest, "wiki/"); ok {
			name, err := url.PathUnescape(strings.TrimSuffix(page, "/"))
			if err != nil || name == "" {
				return "", false
			}
			return wikiDir + "/" + name + ".md", true
		}
		for _, kind := range []string{"blob/main/", "raw/main/", "tree/main/"} {
			if p, ok := strings.CutPrefix(rest, kind); ok {
				name, err := url.PathUnescape(strings.TrimSuffix(p, "/"))
				if err != nil {
					return "", false
				}
				return name, true
			}
		}
		return "", false
	}
	if strings.Contains(target, ":") { // another scheme
		return "", false
	}
	name, err := url.PathUnescape(target)
	if err != nil {
		return "", false
	}
	if strings.HasPrefix(name, "/") {
		return strings.TrimPrefix(path.Clean(name), "/"), true
	}
	return path.Join(path.Dir(f), name), true
}

// group is a page and its translations; Files[0] is the page the group is named after.
type group struct {
	Name  string
	Files []string
}

var (
	readmeSwitch = regexp.MustCompile(`\]\((README\.[A-Za-z-]+\.md)\)`)
	wikiEnglish  = regexp.MustCompile(`^\*\*한국어\*\* \| \[English\]\(` + regexp.QuoteMeta(repoURL) + `/wiki/([^)#]+)\)`)
	wikiKorean   = regexp.MustCompile(`^\[한국어\]\(` + regexp.QuoteMeta(repoURL) + `/wiki/([^)#]+)\) \| \*\*English\*\*`)
)

// translationGroups finds the translated pages: the READMEs named by README.md's language switch, and
// every Korean wiki page with the English page its switch names. A wiki page without a switch (other
// than the sidebar and footer) is reported, so a new page cannot skip its translation.
func translationGroups(root string) ([]group, error) {
	var groups []group
	raw, err := os.ReadFile(filepath.Join(root, "README.md"))
	if err != nil {
		return nil, err
	}
	readmes := group{Name: "README", Files: []string{"README.md"}}
	for _, line := range strings.SplitN(string(raw), "\n", 8) {
		for _, m := range readmeSwitch.FindAllStringSubmatch(line, -1) {
			readmes.Files = append(readmes.Files, m[1])
		}
		if len(readmes.Files) > 1 {
			break
		}
	}
	if len(readmes.Files) > 1 {
		groups = append(groups, readmes)
	}
	entries, err := os.ReadDir(filepath.Join(root, wikiDir))
	if err != nil {
		return nil, err
	}
	paired := map[string]bool{}
	var unpaired []string
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".md") || strings.HasPrefix(name, "_") {
			continue
		}
		ko := wikiDir + "/" + name
		switchLine := languageSwitch(filepath.Join(root, wikiDir, name))
		if m := wikiEnglish.FindStringSubmatch(switchLine); m != nil {
			en, err := url.PathUnescape(m[1])
			if err != nil {
				return nil, err
			}
			enFile := wikiDir + "/" + en + ".md"
			groups = append(groups, group{Name: "wiki/" + strings.TrimSuffix(name, ".md"), Files: []string{ko, enFile}})
			paired[enFile] = true
		} else if wikiKorean.MatchString(switchLine) {
			continue // an English page: paired from its Korean side
		} else {
			unpaired = append(unpaired, ko)
		}
	}
	for _, e := range entries {
		f := wikiDir + "/" + e.Name()
		if strings.HasSuffix(f, ".md") && !strings.HasPrefix(e.Name(), "_") && !paired[f] && wikiKorean.MatchString(languageSwitch(filepath.Join(root, filepath.FromSlash(f)))) {
			unpaired = append(unpaired, f+" (its Korean page does not link back to it)")
		}
	}
	if len(unpaired) > 0 {
		return groups, fmt.Errorf("wiki pages without a language switch to their translation: %s", strings.Join(unpaired, ", "))
	}
	return groups, nil
}

// languageSwitch is the first line of a page that looks like a language switch.
func languageSwitch(p string) string {
	raw, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	for _, line := range strings.SplitN(string(raw), "\n", 8) {
		if strings.Contains(line, "**한국어**") || strings.Contains(line, "**English**") {
			return strings.TrimSpace(line)
		}
	}
	return ""
}

func fileHash(p string) (string, error) {
	raw, err := os.ReadFile(p)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(strings.ReplaceAll(string(raw), "\r\n", "\n")))
	return hex.EncodeToString(sum[:])[:16], nil
}

type lock struct {
	Note   string                       `json:"note"`
	Groups map[string]map[string]string `json:"groups"`
}

const lockNote = "Translation record checked by tools/doccheck. When a file of a group changes, bring the other languages along, then run: go run ./tools/doccheck -write"

func writeLock(root string, groups []group) error {
	l := lock{Note: lockNote, Groups: map[string]map[string]string{}}
	for _, g := range groups {
		hashes := map[string]string{}
		for _, f := range g.Files {
			h, err := fileHash(filepath.Join(root, filepath.FromSlash(f)))
			if err != nil {
				return err
			}
			hashes[f] = h
		}
		l.Groups[g.Name] = hashes
	}
	raw, _ := json.MarshalIndent(l, "", "  ")
	return os.WriteFile(filepath.Join(root, lockFile), append(raw, '\n'), 0o644)
}

// checkTranslations reports groups whose files changed since the record: one language was edited and
// the others were not brought along (or the record was not written after they were).
func checkTranslations(root string) []string {
	groups, err := translationGroups(root)
	var problems []string
	if err != nil {
		problems = append(problems, "translations: "+err.Error())
	}
	var l lock
	raw, err := os.ReadFile(filepath.Join(root, lockFile))
	if err != nil || json.Unmarshal(raw, &l) != nil {
		return append(problems, fmt.Sprintf("translations: %s is missing or unreadable; run go run ./tools/doccheck -write", lockFile))
	}
	for _, g := range groups {
		rec, ok := l.Groups[g.Name]
		if !ok {
			problems = append(problems, fmt.Sprintf("translations: %s is new (%s); write every language, then go run ./tools/doccheck -write", g.Name, strings.Join(g.Files, ", ")))
			continue
		}
		var changed, others []string
		for _, f := range g.Files {
			h, err := fileHash(filepath.Join(root, filepath.FromSlash(f)))
			if err != nil {
				problems = append(problems, fmt.Sprintf("translations: %s: %v", f, err))
				continue
			}
			if rec[f] != h {
				changed = append(changed, f)
			} else {
				others = append(others, f)
			}
		}
		if len(changed) == 0 {
			continue
		}
		msg := fmt.Sprintf("translations: %s changed since its translations were last synced", strings.Join(changed, ", "))
		if len(others) > 0 {
			msg += fmt.Sprintf("; bring %s along", strings.Join(others, ", "))
		}
		problems = append(problems, msg+", then go run ./tools/doccheck -write")
	}
	return problems
}
