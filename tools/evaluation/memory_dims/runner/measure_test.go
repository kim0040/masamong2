package main

import (
	"testing"
	"time"

	"masamong/internal/testutil"
)

func TestPollerSeesGrowthAndProcessMemoryReads(t *testing.T) {
	if readProcMemory().WorkingSet == 0 {
		t.Skip("process memory counters unavailable on this platform")
	}
	p := startPoller(2 * time.Millisecond)
	p.mu.Lock()
	before := p.samples
	p.mu.Unlock()
	hold := make([]byte, 24<<20)
	for i := 0; i < len(hold); i += 4096 {
		hold[i] = 1
	}
	// wait until the poller has sampled twice more: the second of those is after the allocation
	testutil.WaitFor(t, 5*time.Second, "two poller samples after the allocation", func() bool {
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.samples >= before+2
	})
	objects, total, ws, samples := p.finish()
	if samples < 2 || objects < 24<<20 || total < objects || ws < 24<<20 {
		t.Fatalf("poller missed a 24 MiB allocation: objects=%d total=%d ws=%d samples=%d", objects, total, ws, samples)
	}
	_ = hold[0]
}
