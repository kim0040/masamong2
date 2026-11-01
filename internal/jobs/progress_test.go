package jobs

import (
	"testing"
	"time"
)

func TestSnapshotPercentAndETA(t *testing.T) {
	started := time.Unix(0, 0)
	now := started.Add(10 * time.Second)
	p := Snapshot("summaries", 50, 100, started, now)
	if p.PhasePct != 50 || p.OverallPct != 50 {
		t.Fatalf("pct = %#v", p)
	}
	if p.Remaining != 50 {
		t.Fatalf("remaining %d", p.Remaining)
	}
	if p.ETAS == nil || *p.ETAS != 10 {
		t.Fatalf("eta %#v", p.ETAS)
	}
	if p.Label == "" {
		t.Fatal("empty label")
	}
}

func TestParseProgressPlainCheckpoint(t *testing.T) {
	p := ParseProgress("leaf-42")
	if p.Label != "leaf-42" {
		t.Fatalf("got %#v", p)
	}
}
