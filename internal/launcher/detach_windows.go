//go:build windows

package launcher

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unsafe"

	"masamong/internal/fsutil"
)

const createNewProcessGroup = 0x00000200
const detachedProcess = 0x00000008

func detach(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: createNewProcessGroup | detachedProcess}
}

// Lock holds the instance lock: a file kept open for the owner's lifetime. Windows opens files
// without delete sharing, so while it is open nobody else can remove and recreate it.
type Lock struct {
	path string
	f    *os.File
}

func AcquireLock(dataRoot string) (*Lock, error) { return AcquireNamedLock(dataRoot, "daemon.lock") }

// AcquireNamedLock takes one of the data folder's single-owner locks (daemon.lock, tray.lock).
func AcquireNamedLock(dataRoot, name string) (*Lock, error) {
	return acquireRetrying(func() (*Lock, error) { return tryNamedLock(dataRoot, name) })
}

// lockHeld reports whether a live process owns the lock without taking it: Windows refuses to delete
// a file another process holds open, and a file left by a crashed owner is removed, as Acquire would.
func lockHeld(dataRoot, name string) bool {
	err := os.Remove(filepath.Join(dataRoot, name))
	return err != nil && !errors.Is(err, fs.ErrNotExist)
}

func tryNamedLock(dataRoot, name string) (*Lock, error) {
	if err := os.MkdirAll(dataRoot, 0o700); err != nil {
		return nil, err
	}
	if err := fsutil.PrivateDir(dataRoot); err != nil {
		return nil, fmt.Errorf("protect %s: %w", dataRoot, err)
	}
	p := filepath.Join(dataRoot, name)
	_ = os.Remove(p) // a file left by a crashed owner; fails while a live owner holds it open
	f, err := os.OpenFile(p, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("%w for %s", errLockHeld, dataRoot)
	}
	if err := fsutil.PrivateFile(p); err != nil {
		_ = f.Close()
		_ = os.Remove(p)
		return nil, fmt.Errorf("protect %s: %w", p, err)
	}
	if _, err := fmt.Fprintf(f, "%d\n", os.Getpid()); err != nil {
		_ = f.Close()
		_ = os.Remove(p)
		return nil, fmt.Errorf("record process ID in %s: %w", p, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(p)
		return nil, fmt.Errorf("sync process ID in %s: %w", p, err)
	}
	return &Lock{path: p, f: f}, nil
}

func (l *Lock) Release() {
	if l != nil && l.f != nil {
		_ = l.f.Close()
		_ = os.Remove(l.path)
		l.f = nil
	}
}

// Seams for tests: the polite request to quit and the forced end of the tray process.
var (
	askTrayToExit = postCloseToWindowsOf
	killTray      = func(p *os.Process) error { return p.Kill() }
)

// trayExitGrace is how long a tray gets to remove its icon and release its lock after it is asked to
// quit, before the process is ended.
var trayExitGrace = 3 * time.Second

// StopTray stops the notification-area process for dataRoot and waits for its lock to be released. The
// recorded process ID is preferred; the tray is first asked to close its window, which removes its icon
// from the notification area, and is ended by force only when it does not leave in time.
func StopTray(dataRoot string) error {
	if !Running(dataRoot, "tray.lock") {
		return nil
	}
	lockPath := filepath.Join(dataRoot, "tray.lock")
	raw, err := os.ReadFile(lockPath)
	if err != nil {
		return fmt.Errorf("read tray process ID from %s: %w", lockPath, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid < 1 {
		// Older releases did not write a PID; find the tray by executable and data-root arguments.
		pid, err = stopTrayProcesses(dataRoot)
		if err != nil {
			return err
		}
		if pid == 0 {
			return errors.New("the tray lock is active, but its process could not be identified")
		}
	} else if pid != os.Getpid() {
		askTrayToExit(pid)
		if !waitTrayGone(dataRoot, trayExitGrace) {
			process, findErr := os.FindProcess(pid)
			if findErr != nil {
				return fmt.Errorf("find tray process %d: %w", pid, findErr)
			}
			if killErr := killTray(process); killErr != nil && !errors.Is(killErr, os.ErrProcessDone) {
				return fmt.Errorf("stop tray process %d: %w", pid, killErr)
			}
		}
	}
	if !waitTrayGone(dataRoot, 10*time.Second) {
		return fmt.Errorf("tray process %d did not release %s", pid, lockPath)
	}
	return nil
}

func waitTrayGone(dataRoot string, limit time.Duration) bool {
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if !Running(dataRoot, "tray.lock") {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return !Running(dataRoot, "tray.lock")
}

var (
	user32                       = syscall.NewLazyDLL("user32.dll")
	procEnumWindows              = user32.NewProc("EnumWindows")
	procGetWindowThreadProcessID = user32.NewProc("GetWindowThreadProcessId")
	procPostMessageW             = user32.NewProc("PostMessageW")
)

const wmClose = 0x0010

// postCloseToWindowsOf sends WM_CLOSE to every top-level window of the process. The tray's hidden
// window answers by deleting its notification-area icon and ending its message loop, which a forced
// end would leave behind as a ghost icon until the mouse passes over it.
func postCloseToWindowsOf(pid int) bool {
	posted := false
	callback := syscall.NewCallback(func(hwnd uintptr, _ uintptr) uintptr {
		var owner uint32
		procGetWindowThreadProcessID.Call(hwnd, uintptr(unsafe.Pointer(&owner)))
		if int(owner) == pid {
			if ok, _, _ := procPostMessageW.Call(hwnd, wmClose, 0, 0); ok != 0 {
				posted = true
			}
		}
		return 1 // keep enumerating
	})
	procEnumWindows.Call(callback, 0)
	return posted
}

// stopTrayProcesses handles tray processes created by versions that did not record their PID in the lock.
func stopTrayProcesses(dataRoot string) (int, error) {
	cli, err := os.Executable()
	if err != nil {
		return 0, err
	}
	script := `$ErrorActionPreference = 'Stop'
$root = [IO.Path]::GetFullPath($env:MASAMONG_INTERNAL_STOP_DATA_ROOT).TrimEnd('\')
$cli = [IO.Path]::GetFullPath($env:MASAMONG_INTERNAL_STOP_CLI)
$rootPattern = '(?i)(?:--data-root\s+["'']?' + [regex]::Escape($root) + '["'']?(?:\s|$))'
$trayPattern = '(?i)(?:^|\s)tray(?:\s|$)'
$found = @(Get-CimInstance Win32_Process | Where-Object {
  $_.ExecutablePath -and [string]::Equals([IO.Path]::GetFullPath($_.ExecutablePath), $cli, [StringComparison]::OrdinalIgnoreCase) -and
  $_.CommandLine -match $trayPattern -and [regex]::IsMatch($_.CommandLine, $rootPattern)
})
foreach ($p in $found) { Stop-Process -Id $p.ProcessId -Force -ErrorAction Stop }
if ($found.Count -gt 0) { $found | ForEach-Object { $_.ProcessId } }`
	cmd := exec.Command("powershell.exe", "-NoProfile", "-NonInteractive", "-Command", script)
	cmd.Env = append(os.Environ(), "MASAMONG_INTERNAL_STOP_DATA_ROOT="+dataRoot, "MASAMONG_INTERNAL_STOP_CLI="+cli)
	output, err := cmd.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf("find or stop tray process: %w: %s", err, strings.TrimSpace(string(output)))
	}
	text := strings.TrimSpace(string(output))
	if text == "" {
		return 0, nil
	}
	pid, err := strconv.Atoi(strings.Split(text, "\r\n")[0])
	if err != nil {
		return 0, fmt.Errorf("read stopped tray process ID: %w", err)
	}
	return pid, nil
}
