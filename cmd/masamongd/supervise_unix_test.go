//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"masamong/internal/testutil"
)

func TestUnixStopKillPlanHasNoTimer(t *testing.T) {
	for _, ending := range []bool{false, true} {
		from, after := stopKillPlan(ending)
		if from || after {
			t.Fatal("unix armed a kill timer")
		}
	}
	if !resignalStop() {
		t.Fatal("unix stopped repeating the signal")
	}
}

func TestUnixSupervisorWaitsWithoutAKillTimer(t *testing.T) {
	dir := t.TempDir()
	root := filepath.Join(dir, "data")
	t.Setenv(fakeDaemonEnv, dir)
	t.Setenv("MASAMONG_FAKE_SCRIPT", "81")
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	log := &lockedBuffer{}
	sv := &supervisor{self: self, dataRoot: root, log: log, now: time.Now, stopGrace: 200 * time.Millisecond}
	sigs := make(chan os.Signal, 2)
	finished := make(chan int, 1)
	go func() { finished <- sv.run(sigs) }()
	testutil.WaitFor(t, 10*time.Second, "the fake daemon to wait for a signal", func() bool {
		_, err := os.Stat(filepath.Join(dir, "ready-signal"))
		return err == nil
	})
	sigs <- syscall.SIGTERM
	testutil.WaitFor(t, 10*time.Second, "the fake daemon to take the signal", func() bool {
		_, err := os.Stat(filepath.Join(dir, "signaled"))
		return err == nil
	})
	sigs <- syscall.SIGTERM // a resent signal must not shorten anything
	// the child exits only when it is released, so it cannot have finished yet
	select {
	case code := <-finished:
		t.Fatalf("finished before the child could exit: %d", code)
	default:
	}
	if err := os.WriteFile(filepath.Join(dir, "release"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	select {
	case code := <-finished:
		if code != 0 {
			t.Fatalf("code %d, log %s", code, log.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not finish")
	}
	if text := log.String(); strings.Contains(text, "killing") || strings.Contains(text, "deadline") {
		t.Fatalf("unix killed the child: %s", text)
	}
}
