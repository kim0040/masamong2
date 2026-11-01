//go:build darwin || freebsd

package memlimit

import (
	"context"
	"os/exec"
	"runtime"
	"time"
)

func detect() (uint64, string) {
	key := "hw.memsize"
	if runtime.GOOS == "freebsd" {
		key = "hw.physmem"
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, "sysctl", "-n", key).Output()
	if err != nil {
		return 0, "unset"
	}
	n, ok := parseBytes(string(raw))
	if !ok {
		return 0, "unset"
	}
	return n, "sysctl " + key
}
