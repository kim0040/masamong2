//go:build !windows

package main

import (
	"context"
	"os"
	"os/exec"
)

// childAttr keeps the daemon in the supervisor's session and process group.
func childAttr(*exec.Cmd) {}

// terminate passes the supervisor's stop signal to the daemon, which shuts down cleanly.
func terminate(_ context.Context, p *os.Process, sig os.Signal, _ string) error { return p.Signal(sig) }

func supervisorNotifications(chan os.Signal, <-chan struct{}) (func(), error) {
	return func() {}, nil
}

// stopKillPlan never arms Process.Kill. The supervisor signals the child and waits for it to exit.
func stopKillPlan(bool) (fromSignal, afterSuccess bool) { return false, false }

// resignalStop repeats the signal while a stop is already in progress.
func resignalStop() bool { return true }
