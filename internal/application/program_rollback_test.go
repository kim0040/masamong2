package application

import (
	"errors"
	"path/filepath"
	"testing"
)

func assertInstalledProgram(t *testing.T, j *maintenanceJournal) {
	t.Helper()
	for _, f := range j.Entries {
		if hashMatches(f.Target, f.OldHash) || hashMatches(f.Target, f.NewHash) {
			return
		}
	}
	t.Fatal("no verified program remains at an installed name")
}

func TestProgramRollbackRecoversEveryFileOperation(t *testing.T) {
	for _, forward := range []string{"quarantine:0", "install:0", "quarantine:1", "install:1", "verify"} {
		t.Run(forward, func(t *testing.T) {
			dir, staged := updateFixture(t)
			interruptMaintenance(t, forward)
			if err := swapBinaries(dir, staged); !errors.Is(err, errMaintenanceInterrupted) {
				t.Fatal(err)
			}
			maintenanceHook = nil
			path := filepath.Join(dir, updatePending)
			j, err := readMaintenance(path, dir)
			if err != nil {
				t.Fatal(err)
			}
			assertInstalledProgram(t, j)
			maintenanceHook = func(string) error { assertInstalledProgram(t, j); return nil }
			if err := rollbackPair(path, j); err != nil {
				t.Fatal(err)
			}
			for _, f := range j.Entries {
				if !hashMatches(f.Target, f.OldHash) {
					t.Fatal("old program was not restored")
				}
			}
		})
	}
	for _, point := range []string{"rollback", "rollback:stage:0", "rollback:copy:0", "rollback:replace:0", "rollback:0", "rollback:stage:1", "rollback:copy:1", "rollback:replace:1", "rollback:1", "rolled_back"} {
		t.Run(point, func(t *testing.T) {
			dir, staged := updateFixture(t)
			interruptMaintenance(t, "verify")
			if err := swapBinaries(dir, staged); !errors.Is(err, errMaintenanceInterrupted) {
				t.Fatal(err)
			}
			maintenanceHook = nil
			path := filepath.Join(dir, updatePending)
			j, err := readMaintenance(path, dir)
			if err != nil {
				t.Fatal(err)
			}
			maintenanceHook = func(p string) error {
				assertInstalledProgram(t, j)
				if p == point {
					return errMaintenanceInterrupted
				}
				return nil
			}
			if err := rollbackPair(path, j); !errors.Is(err, errMaintenanceInterrupted) {
				t.Fatalf("%s: %v", point, err)
			}
			maintenanceHook = nil
			assertInstalledProgram(t, j)
			if err := RecoverUpdateAt(dir); err != nil {
				t.Fatal(err)
			}
			for _, f := range j.Entries {
				if !hashMatches(f.Target, f.OldHash) {
					t.Fatal("restart did not restore old program")
				}
			}
		})
	}
}
