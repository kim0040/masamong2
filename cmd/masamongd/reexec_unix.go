//go:build !windows

package main

import (
	"os"
	"syscall"
)

// reexec replaces this process with a fresh copy (same PID, so launchd/systemd keep tracking it).
// It only returns on failure.
func reexec(self string) error { return syscall.Exec(self, os.Args, os.Environ()) }
