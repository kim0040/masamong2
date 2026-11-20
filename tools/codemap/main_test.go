package main

import (
	"os"
	"path/filepath"
	"testing"
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
