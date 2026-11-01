//go:build !windows

package launcher

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
)

// detach starts the daemon in its own session so it outlives the launcher and its terminal.
func detach(cmd *exec.Cmd) { cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} }

// Lock holds the instance lock for the life of the daemon: one owner per data folder.
type Lock struct{ f *os.File }

// AcquireLock fails when another daemon already owns dataRoot.
func AcquireLock(dataRoot string) (*Lock, error) { return AcquireNamedLock(dataRoot, "daemon.lock") }

// AcquireNamedLock takes one of the data folder's single-owner locks (daemon.lock, tray.lock). A
// Running check holds the lock for an instant, so contention is retried briefly before giving up.
func AcquireNamedLock(dataRoot, name string) (*Lock, error) {
	return acquireRetrying(func() (*Lock, error) { return tryNamedLock(dataRoot, name) })
}

// lockHeld reports whether another process owns the lock; flock has no test-only call, so the
// probe takes and drops it at once (owners retry over that instant).
func lockHeld(dataRoot, name string) bool {
	l, err := tryNamedLock(dataRoot, name)
	if err != nil {
		return true
	}
	l.Release()
	return false
}

func tryNamedLock(dataRoot, name string) (*Lock, error) {
	if err := os.MkdirAll(dataRoot, 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(filepath.Join(dataRoot, name), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("%w for %s", errLockHeld, dataRoot)
	}
	return &Lock{f: f}, nil
}

// Release drops the lock.
func (l *Lock) Release() {
	if l != nil && l.f != nil {
		_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
		_ = l.f.Close()
	}
}
