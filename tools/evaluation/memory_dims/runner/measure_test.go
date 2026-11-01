package main

import (
	"testing"
	"time"
)

func TestPollerSeesGrowthAndProcessMemoryReads(t *testing.T) {
	if readProcMemory().WorkingSet == 0 {
		t.Skip("process memory counters unavailable on this platform")
	}
	p := startPoller(2 * time.Millisecond)
	hold := make([]byte, 24<<20)
	for i := 0; i < len(hold); i += 4096 {
		hold[i] = 1
	}
	time.Sleep(20 * time.Millisecond)
	objects, total, ws, samples := p.finish()
	if samples < 2 || objects < 24<<20 || total < objects || ws < 24<<20 {
		t.Fatalf("poller missed a 24 MiB allocation: objects=%d total=%d ws=%d samples=%d", objects, total, ws, samples)
	}
	_ = hold[0]
}
