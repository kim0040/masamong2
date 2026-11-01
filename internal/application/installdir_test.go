package application

import (
	"os"
	"path/filepath"
	"testing"
)

// os.Executable is not resolved on every platform: an install reached through a link must still find
// its own program-update journal (maintenanceNeeded, the supervisor's recovery and the operator
// commands all use InstallDir).
func TestInstallDirResolvesLinksEverywhere(t *testing.T) {
	real, link := t.TempDir(), filepath.Join(t.TempDir(), "bin")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("cannot create links here: %v", err)
	}
	exe := filepath.Join(real, exeName("masamongd"))
	if err := os.WriteFile(exe, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	old := selfExecutable
	t.Cleanup(func() { selfExecutable = old })
	selfExecutable = func() (string, error) { return filepath.Join(link, exeName("masamongd")), nil }
	want, _ := filepath.EvalSymlinks(real)
	got, err := InstallDir()
	if err != nil || got != want {
		t.Fatalf("InstallDir = %q, %v; want %q", got, err, want)
	}
	if err := os.WriteFile(filepath.Join(real, updatePending), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !maintenanceNeeded(t.TempDir()) {
		t.Fatal("a program-update journal beside the real files must put the install in maintenance")
	}
	if _, _, err := pendingProgramMaintenance(); os.IsNotExist(err) {
		t.Fatal("pendingProgramMaintenance looked in the link's folder, not the install's")
	}
}
