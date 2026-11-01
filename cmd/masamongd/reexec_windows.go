//go:build windows

package main

import (
	"os"
	"os/exec"
	"syscall"
)

// reexec starts a fresh copy with the same arguments and log handles; this process then exits.
// Windows has no exec(2), and the data folder lock is already released.
func reexec(self string) error {
	cmd := exec.Command(self, os.Args[1:]...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x00000008 | 0x00000200} // DETACHED_PROCESS | CREATE_NEW_PROCESS_GROUP
	if err := cmd.Start(); err != nil {
		return err
	}
	return cmd.Process.Release()
}
