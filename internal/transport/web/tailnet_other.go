//go:build !noweb && !windows

package web

import "os/exec"

func noWindow(*exec.Cmd) {}
