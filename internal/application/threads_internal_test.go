package application

import (
	"context"
	"testing"
	"time"

	"masamong/internal/clock"
)

// An offer track keeps only the turns the score counts, a finished conversation's track and old offers
// are forgotten, and a closed thread leaves memory after threadKeepClosed.
func TestThreadStateStaysBounded(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	tr := &offerTrack{}
	for i := 0; i < 500; i++ {
		tr.note(now.Add(time.Duration(i) * 5 * time.Minute)) // a conversation that never pauses for 15 minutes
	}
	if n := len(tr.turns); n != 4 {
		t.Fatalf("turns kept: %d", n)
	}

	ts := &threadState{
		tracks: map[string]*offerTrack{
			"g:c:quiet":    {turns: []time.Time{now.Add(-2 * time.Hour)}},
			"g:c:declined": {turns: []time.Time{now.Add(-2 * time.Hour)}, declinedUntil: now.Add(time.Minute)},
			"g:c:talking":  {turns: []time.Time{now.Add(-time.Minute)}},
		},
		offersAt: map[string][]time.Time{"g:c": {now.Add(-2 * time.Hour)}, "g:d": {now.Add(-2 * time.Hour), now.Add(-time.Minute)}},
	}
	ts.sweepTracks(now)
	if _, ok := ts.tracks["g:c:quiet"]; ok || len(ts.tracks) != 2 {
		t.Fatalf("tracks: %v", ts.tracks)
	}
	if _, ok := ts.offersAt["g:c"]; ok || len(ts.offersAt["g:d"]) != 1 {
		t.Fatalf("offers: %v", ts.offersAt)
	}

	s := &Service{Clock: clock.Fixed{T: now}}
	s.threads.loaded = true
	s.threads.threads = map[string]*threadInfo{
		"old":    {Guild: "g", Parent: "c", BotOwned: true, LastAt: now.Add(-threadKeepClosed - time.Hour)},
		"recent": {Guild: "g", Parent: "c", BotOwned: true, LastAt: now.Add(-time.Hour)},
	}
	s.closeIdleThreads(context.Background())
	if _, ok := s.threads.threads["old"]; ok || s.threads.threads["recent"] == nil {
		t.Fatalf("threads: %v", s.threads.threads)
	}
}
