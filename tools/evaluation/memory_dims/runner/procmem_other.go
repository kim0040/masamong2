//go:build !windows

package main

import (
	"os"
	"strconv"
	"strings"
)

type procMemory struct {
	WorkingSet     uint64
	PeakWorkingSet uint64
	Private        uint64
}

// readProcMemory reads VmRSS (resident set) and VmHWM (its high-water mark) from /proc.
func readProcMemory() procMemory {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return procMemory{}
	}
	var m procMemory
	for _, line := range strings.Split(string(raw), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 {
			continue
		}
		kb, _ := strconv.ParseUint(f[1], 10, 64)
		switch f[0] {
		case "VmRSS:":
			m.WorkingSet = kb << 10
		case "VmHWM:":
			m.PeakWorkingSet = kb << 10
		case "VmData:":
			m.Private = kb << 10
		}
	}
	return m
}
