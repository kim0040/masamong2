//go:build windows

package main

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"

	"masamong/internal/launcher"
	"masamong/internal/transport/web"
)

// childAttr starts the daemon without a console window: the supervisor itself runs detached, and a
// console program started from it would otherwise open a new window.
func childAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

// A child without a visible console cannot receive Ctrl+C. Reuse masamong stop's
// authenticated API, but only for this PID and this resolved data folder.
func terminate(ctx context.Context, p *os.Process, _ os.Signal, root string) error {
	token, err := web.ReadToken(root)
	if err != nil {
		return errors.New("child console authentication file unavailable")
	}
	return launcher.RequestStopChild(ctx, root, p.Pid, token)
}

// stopKillPlan arms Process.Kill from the signal for a confirmed session end (the OS budget),
// and only after the stop API succeeds for an ordinary stop.
func stopKillPlan(sessionEnding bool) (fromSignal, afterSuccess bool) {
	if sessionEnding {
		return true, false
	}
	return false, true
}

// resignalStop does not send the stop API again while a stop is already in progress.
func resignalStop() bool { return false }
