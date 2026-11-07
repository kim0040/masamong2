//go:build windows

package launcher

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTrayLockRecordsOwnerProcess(t *testing.T) {
	root := t.TempDir()
	lock, err := AcquireNamedLock(root, "tray.lock")
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Release()
	raw, err := os.ReadFile(filepath.Join(root, "tray.lock"))
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || pid != os.Getpid() {
		t.Fatalf("tray lock process ID = %q, parsed %d (%v), want %d", raw, pid, err, os.Getpid())
	}
}

// trayChild is the test binary playing the tray: it holds tray.lock until a "quit" file appears.
func TestMain(m *testing.M) {
	if root := os.Getenv("MASAMONG_TEST_TRAY_ROOT"); root != "" {
		lock, err := AcquireNamedLock(root, "tray.lock")
		if err != nil {
			os.Exit(2)
		}
		for i := 0; i < 600; i++ {
			if _, err := os.Stat(filepath.Join(root, "quit")); err == nil {
				lock.Release()
				os.Exit(0)
			}
			time.Sleep(50 * time.Millisecond)
		}
		os.Exit(3)
	}
	os.Exit(m.Run())
}

func startFakeTray(t *testing.T, root string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^$")
	cmd.Env = append(os.Environ(), "MASAMONG_TEST_TRAY_ROOT="+root)
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-exited })
	// a freshly built test binary can take seconds to start while antivirus scans it
	deadline := time.Now().Add(30 * time.Second)
	// the lock first, its process ID a moment later: on a slow machine StopTray could read the file in between
	lockHoldsPID := func() bool {
		raw, err := os.ReadFile(filepath.Join(root, "tray.lock"))
		return err == nil && strings.TrimSpace(string(raw)) != ""
	}
	for !Running(root, "tray.lock") || !lockHoldsPID() {
		select {
		case err := <-exited:
			exited <- err
			t.Fatalf("the fake tray exited before taking its lock: %v", err)
		case <-time.After(50 * time.Millisecond):
		}
		if time.Now().After(deadline) {
			t.Fatal("the fake tray never took its lock")
		}
	}
	return cmd
}

func TestStopTrayAsksPolitelyBeforeEndingTheProcess(t *testing.T) {
	root := t.TempDir()
	cmd := startFakeTray(t, root)
	asked, killed := 0, 0
	oldAsk, oldKill := askTrayToExit, killTray
	askTrayToExit = func(pid int) bool {
		asked++
		if pid != cmd.Process.Pid {
			t.Errorf("asked pid %d, the recorded tray is %d", pid, cmd.Process.Pid)
		}
		_ = os.WriteFile(filepath.Join(root, "quit"), nil, 0o600) // the tray closes itself
		return true
	}
	killTray = func(p *os.Process) error { killed++; return p.Kill() }
	t.Cleanup(func() { askTrayToExit, killTray = oldAsk, oldKill })
	if err := StopTray(root); err != nil {
		t.Fatal(err)
	}
	if asked != 1 || killed != 0 {
		t.Fatalf("a tray that quits when asked must not be killed: asked %d, killed %d", asked, killed)
	}
}

func TestStopTrayEndsATrayThatIgnoresTheRequest(t *testing.T) {
	root := t.TempDir()
	startFakeTray(t, root)
	killed := 0
	oldAsk, oldKill, oldGrace := askTrayToExit, killTray, trayExitGrace
	askTrayToExit = func(int) bool { return false }
	killTray = func(p *os.Process) error { killed++; return p.Kill() }
	trayExitGrace = 300 * time.Millisecond
	t.Cleanup(func() { askTrayToExit, killTray, trayExitGrace = oldAsk, oldKill, oldGrace })
	if err := StopTray(root); err != nil {
		t.Fatal(err)
	}
	if killed != 1 || Running(root, "tray.lock") {
		t.Fatalf("killed %d, still running %v", killed, Running(root, "tray.lock"))
	}
}

func TestPostCloseReachesNoWindowOfAProcessWithoutOne(t *testing.T) {
	if postCloseToWindowsOf(os.Getpid() + 1_000_000) {
		t.Fatal("no window should be found for an unknown process")
	}
}
