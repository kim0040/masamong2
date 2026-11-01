//go:build !noweb && windows

package web

import (
	"os/exec"
	"syscall"
)

// noWindow keeps the look-up from flashing a console window on the desktop.
func noWindow(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}
