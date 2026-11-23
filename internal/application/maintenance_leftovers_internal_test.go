package application

import (
	"os"
	"path/filepath"
	"testing"

	"masamong/internal/storage"
)

func TestRecoveryFileName(t *testing.T) {
	t.Parallel()
	for name, want := range map[string]bool{
		".masamong.db.old-abc123":     true,
		".masamong.new-zz":            true,
		".config.yaml.old-1":          true,
		"masamong.db.old-abc":         false, // no leading dot: it is a live file, not a leftover
		".backup-staging-abc":         false, // staging folders are listed separately
		".masamong.db.old-":           false,
		".masamong.db.bak-abc":        false,
		".maintenance-operation.json": false,
	} {
		if got := recoveryFileName.MatchString(name); got != want {
			t.Errorf("recoveryFileName(%q) = %v", name, got)
		}
	}
}

func TestDataFoldersIncludesACustomDatabaseFolder(t *testing.T) {
	// sequential: it sets process environment variables
	t.Setenv("MASAMONG_DB_BACKEND", "")
	root := t.TempDir()
	if got := dataFolders(root); len(got) != 1 || got[0] != root {
		t.Fatalf("a default root has one data folder: %v", got)
	}
	if err := storage.SaveSettings(root, storage.Settings{Backend: "sqlite", SQLiteFile: "sub/custom.db"}); err != nil {
		t.Fatal(err)
	}
	got := dataFolders(root)
	if len(got) != 2 || got[1] != filepath.Join(root, "sub") {
		t.Fatalf("a custom sqlite_file adds its folder: %v", got)
	}
}

func TestStagingLeftovers(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	if got := stagingLeftovers(root); len(got) != 0 {
		t.Fatalf("no backups folder, no leftovers: %v", got)
	}
	dir := filepath.Join(root, "backups")
	live := filepath.Join(dir, "backup-2026-01-01")
	for _, p := range []string{filepath.Join(dir, ".backup-staging-b"), filepath.Join(dir, ".backup-staging-a"), live} {
		if err := os.MkdirAll(p, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	got := stagingLeftovers(root)
	if len(got) != 2 || got[0] != filepath.Join(dir, ".backup-staging-a") || got[1] != filepath.Join(dir, ".backup-staging-b") {
		t.Fatalf("sorted staging leftovers: %v", got)
	}
}
