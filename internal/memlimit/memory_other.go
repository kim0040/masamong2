//go:build !linux && !windows && !darwin && !freebsd

package memlimit

func detect() (uint64, string) { return 0, "unset" }
