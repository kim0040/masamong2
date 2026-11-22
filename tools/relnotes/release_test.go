package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"masamong/internal/relnotes"
)

// The release workflow publishes release-notes/<tag>.md as the release text and the JSON as the asset
// release-notes.json, and keeps the generated notes when there is no file.
func TestReleaseWorkflowUsesTheNotes(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", ".github", "workflows", "release.yml"))
	if err != nil {
		t.Fatal(err)
	}
	wf := string(data)
	publish := wf[strings.Index(wf, "\n  publish:"):]
	for _, want := range []string{
		`release-notes/$GITHUB_REF_NAME.md`,
		`release-notes/$GITHUB_REF_NAME.json`,
		`/release-notes.json`,
		`--notes-file`,
		`--generate-notes`,
		`gh release create`,
	} {
		if !strings.Contains(publish, want) {
			t.Errorf("the publish job of release.yml lacks %q", want)
		}
	}
	// the asset is named release-notes.json whatever the tag: it is copied to that name, not uploaded as <tag>.json
	if strings.Contains(publish, `release-notes/$GITHUB_REF_NAME.json#`) {
		t.Error("a #label does not rename an asset; copy the file to release-notes.json instead")
	}
}

// The committed Markdown is the rendering of the committed JSON (the release workflow publishes the
// Markdown, the console and bot read the JSON: they must not drift). The changelog link depends on the
// tag, so that line is left out of the comparison.
func TestCommittedMarkdownMatchesJSON(t *testing.T) {
	dir := filepath.Join("..", "..", "release-notes")
	files, _ := filepath.Glob(filepath.Join(dir, "v*.json"))
	if len(files) == 0 {
		t.Fatal("no release notes found")
	}
	for _, f := range files {
		data, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		n, err := relnotes.Parse(data)
		if err != nil {
			t.Errorf("%s: %v", f, err)
			continue
		}
		canon, _ := relnotes.Marshal(n)
		if string(canon) != string(data) {
			t.Errorf("%s is not in the canonical form (run go run ./tools/relnotes --version %s --render --force, or format it like relnotes.Marshal)", f, n.Version)
		}
		md, err := os.ReadFile(strings.TrimSuffix(f, ".json") + ".md")
		if err != nil {
			t.Errorf("%s has no Markdown beside it: %v", f, err)
			continue
		}
		if got, want := withoutChangelog(string(md)), withoutChangelog(relnotes.RenderRelease(n, "")); got != want {
			t.Errorf("%s.md differs from the rendering of the JSON (run go run ./tools/relnotes --version %s --render)", n.Version, n.Version)
		}
	}
}

func withoutChangelog(s string) string {
	var keep []string
	for _, l := range strings.Split(s, "\n") {
		if !strings.HasPrefix(l, "**Full Changelog**") {
			keep = append(keep, l)
		}
	}
	return strings.TrimRight(strings.Join(keep, "\n"), "\n")
}
