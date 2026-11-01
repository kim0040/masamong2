//go:build !windows

package main

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
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
	deadline := time.Now().Add(10 * time.Second)
	for {
		if _, err := os.Stat(filepath.Join(dir, "ready-signal")); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("fake daemon did not wait for a signal")
		}
		time.Sleep(20 * time.Millisecond)
	}
	started := time.Now()
	sigs <- syscall.SIGTERM
	time.Sleep(100 * time.Millisecond)
	sigs <- syscall.SIGTERM
	select {
	case code := <-finished:
		if code != 0 {
			t.Fatalf("code %d, log %s", code, log.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("supervisor did not finish")
	}
	if time.Since(started) < 700*time.Millisecond {
		t.Fatal("kill timer fired before the child finished")
	}
	if text := log.String(); strings.Contains(text, "killing") || strings.Contains(text, "deadline") {
		t.Fatalf("unix killed the child: %s", text)
	}
	if _, err := os.Stat(filepath.Join(dir, "signaled")); err != nil {
		t.Fatal(err)
	}
}
