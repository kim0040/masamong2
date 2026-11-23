package application

import (
	"os"
	"path/filepath"
	"testing"
)

func writeHashed(t *testing.T, dir, name, body string) (path, hash string) {
	t.Helper()
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	h, err := regularHash(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, h
}

func TestClassifyUpdate(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	live, liveHash := writeHashed(t, dir, "masamong", "new program")
	oldPath, _ := writeHashed(t, dir, ".masamong.old-x", "old program")
	missing := filepath.Join(dir, "gone")

	if got := classifyUpdate(&maintenanceJournal{}); got != updateResumable {
		t.Fatalf("an empty journal classifies %q", got)
	}
	if got := classifyUpdate(&maintenanceJournal{Entries: []maintenanceFile{
		{Target: live, Stage: "staged", NewHash: liveHash},
	}}); got != updateNewPair {
		t.Fatalf("all-new files classify %q", got)
	}
	if got := classifyUpdate(&maintenanceJournal{Phase: "rollback", Entries: []maintenanceFile{
		{Target: live, Stage: "staged", OldHash: liveHash},
	}}); got != updateOldPair {
		t.Fatalf("all-old files during rollback classify %q", got)
	}
	if got := classifyUpdate(&maintenanceJournal{Phase: "prepared", Entries: []maintenanceFile{
		{Target: live, Stage: "staged", OldHash: liveHash},
	}}); got != updateResumable {
		t.Fatalf("old files outside a rollback classify %q", got)
	}
	if got := classifyUpdate(&maintenanceJournal{Entries: []maintenanceFile{
		{Target: live, Stage: "staged", NewHash: liveHash},
		{Target: live, Stage: "staged", NewHash: "not-the-hash"},
	}}); got != updateStuck {
		t.Fatalf("a file the update never saw classifies %q", got)
	}
	// A missing live file is fine when its set-aside copy still hashes to what the journal recorded.
	if got := classifyUpdate(&maintenanceJournal{Entries: []maintenanceFile{
		{Target: missing, Old: oldPath, OldHash: mustHash(t, oldPath)},
	}}); got != updateResumable {
		t.Fatalf("a set-aside file classifies %q", got)
	}
	// But a missing live file with nothing verifiable left is stuck.
	if got := classifyUpdate(&maintenanceJournal{Entries: []maintenanceFile{
		{Target: missing, OldHash: "not-the-hash"},
	}}); got != updateStuck {
		t.Fatalf("an unverifiable gap classifies %q", got)
	}
}

func mustHash(t *testing.T, path string) string {
	t.Helper()
	h, err := regularHash(path)
	if err != nil {
		t.Fatal(err)
	}
	return h
}
