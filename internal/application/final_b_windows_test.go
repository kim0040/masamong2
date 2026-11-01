package application

import (
	"errors"
	"golang.org/x/sys/windows"
	"path/filepath"
	"testing"
)

func TestFinalBProgramRollbackCanRemoveLastInstalledProgram(t *testing.T) {
	dir, staged := updateFixture(t)
	interruptMaintenance(t, "quarantine:1")
	if err := swapBinaries(dir, staged); !errors.Is(err, errMaintenanceInterrupted) {
		t.Fatal(err)
	}
	maintenanceHook = nil
	j, err := readMaintenance(filepath.Join(dir, updatePending), dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := windows.UTF16PtrFromString(j.Entries[0].Old)
	if err != nil {
		t.Fatal(err)
	}
	// A scanner may read this recovery copy while denying delete/rename sharing.
	handle, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer windows.CloseHandle(handle)
	err = rollbackPair(filepath.Join(dir, updatePending), j)
	cli := exists(filepath.Join(dir, exeName("masamong")))
	daemon := exists(filepath.Join(dir, exeName("masamongd")))
	t.Logf("rollback error=%v installed CLI=%v installed daemon=%v old CLI=%v old daemon=%v", err, cli, daemon, exists(j.Entries[0].Old), exists(j.Entries[1].Old))
	// A read-only lock on the old copy permits copy-then-replace to succeed.
	if err != nil {
		t.Fatal(err)
	}
	if !cli && !daemon {
		t.Fatal("rollback removed the last program before confirming its old copy could be installed")
	}
}

func TestProgramRollbackRetriesLockedInstalledImage(t *testing.T) {
	dir, staged := updateFixture(t)
	interruptMaintenance(t, "quarantine:1")
	if err := swapBinaries(dir, staged); !errors.Is(err, errMaintenanceInterrupted) {
		t.Fatal(err)
	}
	maintenanceHook = nil
	path := filepath.Join(dir, updatePending)
	j, err := readMaintenance(path, dir)
	if err != nil {
		t.Fatal(err)
	}
	p, err := windows.UTF16PtrFromString(j.Entries[0].Target)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(p, windows.GENERIC_READ, windows.FILE_SHARE_READ, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := rollbackPair(path, j); err == nil {
		windows.CloseHandle(h)
		t.Fatal("locked target should defer replacement")
	}
	assertInstalledProgram(t, j)
	if !hashMatches(j.Entries[0].Old, j.Entries[0].OldHash) {
		t.Fatal("old recovery image was lost")
	}
	if err := windows.CloseHandle(h); err != nil {
		t.Fatal(err)
	}
	if err := RecoverUpdateAt(dir); err != nil {
		t.Fatal(err)
	}
	for _, f := range j.Entries {
		if !hashMatches(f.Target, f.OldHash) {
			t.Fatal("retry did not restore old pair")
		}
	}
}
