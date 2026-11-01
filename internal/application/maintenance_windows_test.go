//go:build windows

package application

import (
	"bufio"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"golang.org/x/sys/windows"
	"log/slog"
)

func holdMaintenanceFile(t *testing.T, path string) func() {
	t.Helper()
	p, err := windows.UTF16PtrFromString(path)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	return func() { _ = windows.CloseHandle(h) }
}

func TestWindowsLockedRestorePreservesWALAndRetries(t *testing.T) {
	s, _ := stageRestoreTest(t)
	before, _ := fileSHA256(s.Store.Path())
	wal := s.Store.Path() + "-wal"
	_ = os.WriteFile(wal, []byte("committed crash WAL"), 0o600)
	release := holdMaintenanceFile(t, s.Store.Path())
	res := ApplyPendingRestore(s.DataRoot, slog.Default())
	release()
	if res == nil || res.OK || !res.RollbackOK {
		t.Fatalf("locked restore: %+v", res)
	}
	if !hashMatches(s.Store.Path(), before) {
		t.Fatal("locked DB changed")
	}
	if b, _ := os.ReadFile(wal); string(b) != "committed crash WAL" {
		t.Fatal("locked restore lost WAL")
	}
}

func TestWindowsLockedResetRetainsJournalAndRetries(t *testing.T) {
	s := maintenanceService(t, true)
	maintenanceAction(t, s, "instance.reset", map[string]any{"confirm": "reset", "backup": false})
	_ = s.Close()
	release := holdMaintenanceFile(t, s.Store.Path())
	if err := RecoverMaintenance(s.DataRoot, slog.Default()); err == nil {
		release()
		t.Fatal("locked reset reported success")
	}
	if _, err := os.Stat(filepath.Join(s.DataRoot, maintenancePending)); err != nil {
		release()
		t.Fatal("locked reset lost journal")
	}
	release()
	if err := RecoverMaintenance(s.DataRoot, slog.Default()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.Store.Path()); !os.IsNotExist(err) {
		t.Fatal("retry did not remove custom DB")
	}
}

func TestTwoWindowsSwapsWithOldImageRunning(t *testing.T) {
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	bytes, err := os.ReadFile(self)
	if err != nil {
		t.Fatal(err)
	}
	for _, base := range []string{"masamong", "masamongd"} {
		if err := os.WriteFile(filepath.Join(dir, exeName(base)), bytes, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	cmd := exec.Command(filepath.Join(dir, exeName("masamongd")))
	cmd.Env = append(os.Environ(), "_MASAMONG_TEST_PROGRAM_SLEEP=1")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if line, err := bufio.NewReader(out).ReadString('\n'); err != nil || line != "ready\n" {
		t.Fatalf("fixture readiness %q %v", line, err)
	}
	for i := 1; i <= 2; i++ {
		staged := map[string]string{}
		for _, base := range []string{"masamong", "masamongd"} {
			p := filepath.Join(dir, "staged-"+exeName(base))
			if err := os.WriteFile(p, bytes, 0o755); err != nil {
				t.Fatal(err)
			}
			staged[exeName(base)] = p
		}
		if err := swapBinaries(dir, staged); err != nil {
			t.Fatalf("swap %d while original is mapped: %v", i, err)
		}
	}
	old, _ := filepath.Glob(filepath.Join(dir, "."+exeName("masamongd")+".old-*"))
	if len(old) == 0 {
		t.Fatal("running recovery image was deleted")
	}
}
