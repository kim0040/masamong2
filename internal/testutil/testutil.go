// Package testutil holds the test helpers every package shares (standard rule
// R35): waiting, finding the repository root, loading JSON, reading fixtures
// and comparing floats. Helpers only one package uses stay in that package's
// test files.
package testutil

import (
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// RepoRoot returns the repository root (the directory holding go.mod), found
// by walking up from the test's working directory.
func RepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("go.mod not found")
	return ""
}

// LoadJSON reads the JSON file at path into v.
func LoadJSON(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatal(err)
	}
}

// MustJSON marshals v and fails the test if it cannot.
func MustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Fixture returns the bytes of a file in the calling package's testdata
// directory.
func Fixture(t *testing.T, name string) []byte {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// WaitFor polls ok until it returns true or d elapses. It is for watching
// observable state; prefer injected clocks and channels over real time where
// the code under test allows it (standard rule R34).
func WaitFor(t *testing.T, d time.Duration, what string, ok func() bool) {
	t.Helper()
	for end := time.Now().Add(d); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		if ok() {
			return
		}
	}
	t.Fatalf("timed out waiting for %s", what)
}

// Near reports whether a and b are within tol of each other. The caller picks
// the tolerance; the copies this replaced used 1e-12 and 1e-9.
func Near(a, b, tol float64) bool {
	return math.Abs(a-b) < tol
}
