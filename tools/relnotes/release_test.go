package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
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
