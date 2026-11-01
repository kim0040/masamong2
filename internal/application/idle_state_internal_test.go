package application

import (
	"context"
	"fmt"
	"testing"
	"time"

	"masamong/internal/clock"
	"masamong/internal/conversation"
)

// A channel's live window goes with the channel once it has been quiet for a day, and the table never
// holds more than liveMaxChannels windows.
func TestLiveWindowsOfIdleChannelsAreDropped(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s := &Service{Clock: clock.Fixed{T: now}}
	s.live.chans = map[string][]conversation.Line{
		"g:idle":  {{ID: "1", At: now.Add(-liveIdleEvict - time.Minute)}},
		"g:empty": {},
		"g:fresh": {{ID: "2", At: now.Add(-time.Hour)}},
	}
	s.sweepLive(now)
	if len(s.live.chans) != 1 || s.live.chans["g:fresh"] == nil {
		t.Fatalf("windows after sweep: %v", s.live.chans)
	}

	for i := 0; i < liveMaxChannels+50; i++ {
		s.live.chans[fmt.Sprintf("g:c%d", i)] = []conversation.Line{{ID: "x", At: now.Add(-time.Duration(i) * time.Minute)}}
	}
	s.sweepLive(now)
	if len(s.live.chans) != liveMaxChannels {
		t.Fatalf("%d windows kept, want %d", len(s.live.chans), liveMaxChannels)
	}
	if s.live.chans["g:c0"] == nil || s.live.chans[fmt.Sprintf("g:c%d", liveMaxChannels+49)] != nil {
		t.Fatal("the cap dropped a recent window instead of the quietest ones")
	}
}

// Participation state of a channel idle for a day is dropped, but never one with a candidate waiting, a
// collected row being followed or a mute running; the count cap spares those too. The minute tick reaches
// the sweep through sweepCollectNext.
func TestParticipationChannelsIdleAreDroppedButBusyOnesStay(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s := &Service{Clock: clock.Fixed{T: now}}
	old := now.Add(-partIdleEvict - time.Hour)
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	s.part.chans = map[string]*partChannel{
		"g:idle":    {touched: old},
		"g:pending": {touched: old, pending: &partCandidate{timer: timer}},
		"g:watched": {touched: old, watches: []nextWatch{{row: "r", until: now.Add(time.Minute)}}},
		"g:muted":   {touched: old, mutedUntil: now.Add(time.Minute)},
		"g:fresh":   {touched: now.Add(-time.Hour)},
	}
	s.live.chans = map[string][]conversation.Line{"g:idle": {{ID: "1", At: old}}}
	s.sweepCollectNext()
	if len(s.part.chans) != 4 || s.part.chans["g:idle"] != nil {
		t.Fatalf("channels after the tick: %v", s.part.chans)
	}
	if len(s.live.chans) != 0 {
		t.Fatalf("the tick did not reach the live windows: %v", s.live.chans)
	}

	for i := 0; i < partMaxChannels+100; i++ {
		s.part.chans[fmt.Sprintf("g:c%d", i)] = &partChannel{touched: now.Add(-time.Duration(i) * time.Second)}
	}
	s.sweepCollectNext()
	if len(s.part.chans) != partMaxChannels {
		t.Fatalf("%d channels kept, want %d", len(s.part.chans), partMaxChannels)
	}
	for _, key := range []string{"g:pending", "g:watched", "g:muted"} {
		if s.part.chans[key] == nil {
			t.Fatalf("%s was dropped with work still attached", key)
		}
	}
}

// Every use of a channel entry stamps it, which is what the idle sweep reads.
func TestPartChannelStampsUse(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s := &Service{Clock: clock.Fixed{T: now}}
	s.part.mu.Lock()
	c := s.partChannel("g:c")
	s.part.mu.Unlock()
	if !c.touched.Equal(now) {
		t.Fatalf("touched = %v", c.touched)
	}
}

// Open threads of people leave memory after a quiet day (their rows stay), the bot's own open threads and
// recent ones stay, and the table is capped without ever dropping a bot thread that is open.
func TestHumanThreadsLeaveMemoryWhenIdleAndAreCapped(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s := &Service{Clock: clock.Fixed{T: now}}
	s.threads.loaded = true
	s.threads.threads = map[string]*threadInfo{
		"human-idle":   {Guild: "g", Parent: "c", Open: true, LastAt: now.Add(-threadKeepHumanOpen - time.Hour)},
		"human-recent": {Guild: "g", Parent: "c", Open: true, LastAt: now.Add(-time.Hour)},
		"bot-open":     {Guild: "g", Parent: "c", BotOwned: true, Open: true, LastAt: now.Add(-time.Minute)},
	}
	s.closeIdleThreads(context.Background())
	if _, ok := s.threads.threads["human-idle"]; ok || len(s.threads.threads) != 2 {
		t.Fatalf("threads: %v", s.threads.threads)
	}

	for i := 0; i < threadMaxKnown+100; i++ {
		s.threads.threads[fmt.Sprintf("t%d", i)] = &threadInfo{Guild: "g", Parent: "c", Open: true, LastAt: now.Add(-time.Duration(i) * time.Second)}
	}
	s.closeIdleThreads(context.Background())
	if len(s.threads.threads) != threadMaxKnown {
		t.Fatalf("%d threads kept, want %d", len(s.threads.threads), threadMaxKnown)
	}
	if s.threads.threads["bot-open"] == nil || s.threads.threads["t0"] == nil {
		t.Fatal("the cap dropped the bot's open thread or the newest thread")
	}
}

// An archive closes a thread of people so the ordinary cleanup can drop it; an unarchive opens it again
// but not a thread the bot closed; a delete removes it at once.
func TestThreadArchiveAndDeleteUpdateTheTable(t *testing.T) {
	s, clk := deliveryService(t)
	now := clk.Now()
	s.threads.loaded = true
	s.threads.threads = map[string]*threadInfo{
		"human": {Guild: "g", Parent: "c", Open: true, LastAt: now.Add(-time.Hour), Owner: "u"},
		"bot":   {Guild: "g", Parent: "c", BotOwned: true, Open: true, LastAt: now},
	}
	s.saveThread("human")
	s.saveThread("bot")
	s.ThreadArchived("human", true)
	if h := s.threads.threads["human"]; h.Open || h.Owner != "" {
		t.Fatalf("archived human thread: %+v", h)
	}
	s.ThreadArchived("human", false)
	if !s.threads.threads["human"].Open {
		t.Fatal("unarchived human thread stayed closed")
	}
	s.ThreadArchived("bot", true)
	s.ThreadArchived("bot", false)
	if s.threads.threads["bot"].Open {
		t.Fatal("a person reopened a thread the bot closed")
	}
	s.ThreadDeleted("human")
	if _, ok := s.threads.threads["human"]; ok {
		t.Fatal("deleted thread is still known")
	}
	// an archived human thread is dropped with the closed ones once it is old enough
	s.threads.threads["old"] = &threadInfo{Guild: "g", Parent: "c", Open: true, LastAt: now.Add(-threadKeepClosed - time.Hour)}
	s.saveThread("old")
	s.ThreadArchived("old", true)
	s.closeIdleThreads(context.Background())
	if _, ok := s.threads.threads["old"]; ok {
		t.Fatal("archived thread was kept past the closed-thread keep time")
	}
}

// A channel entry made again after the old one was dropped counts versions from zero, so a candidate of the
// old entry must not pass the final check there even when the versions happen to match.
func TestCandidateOfADroppedChannelEntryIsNotCurrent(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s := &Service{Clock: clock.Fixed{T: now}}
	s.part.mu.Lock()
	c := s.partChannel("g:c")
	c.version++
	cand := &partCandidate{key: "g:c", version: c.version, gen: c.gen}
	s.part.mu.Unlock()
	if !s.stillCurrent(cand) {
		t.Fatal("the candidate is not current in its own entry")
	}
	s.part.mu.Lock()
	delete(s.part.chans, "g:c")
	c = s.partChannel("g:c")
	c.version++ // the first message of the new entry
	s.part.mu.Unlock()
	if s.stillCurrent(cand) {
		t.Fatal("a candidate of a dropped entry passed the final check in the entry made after it")
	}
}

// Sweeping never drops an entry that still carries a limit — the pause after a call, a remark inside the
// cooldown or the daily budget, a judgement inside the judge budget, an unsettled remark — nor one with a
// candidate being judged. When only those are left the table stays over the cap.
func TestParticipationSweepKeepsEntriesWithLiveLimits(t *testing.T) {
	now := time.Date(2026, 10, 11, 12, 0, 0, 0, time.UTC)
	s := &Service{Clock: clock.Fixed{T: now}}
	oldest := now.Add(-23 * time.Hour)
	limited := map[string]*partChannel{
		"g:called":    {touched: oldest, lastCall: now.Add(-time.Minute)},
		"g:spoke":     {touched: oldest, spoken: []time.Time{now.Add(-3 * time.Hour)}},
		"g:judged":    {touched: oldest, judged: []time.Time{now.Add(-10 * time.Minute)}},
		"g:unsettled": {touched: oldest, spokes: []partSpoke{{at: now.Add(-2 * time.Hour)}}},
		"g:judging":   {touched: oldest},
	}
	limited["g:judging"].inflight.Add(1)
	s.part.chans = map[string]*partChannel{}
	for k, c := range limited {
		s.part.chans[k] = c
	}
	for i := 0; i < partMaxChannels+100; i++ {
		s.part.chans[fmt.Sprintf("g:plain%d", i)] = &partChannel{touched: now.Add(-time.Duration(i+1) * time.Second)}
	}
	s.sweepPartChannels(now)
	for k := range limited {
		if s.part.chans[k] == nil {
			t.Fatalf("%s was dropped with a limit still running", k)
		}
	}
	if len(s.part.chans) != partMaxChannels {
		t.Fatalf("%d channels kept, want %d", len(s.part.chans), partMaxChannels)
	}

	// nothing but limits left to drop: the cap gives way
	s.part.chans = map[string]*partChannel{}
	for i := 0; i < partMaxChannels+10; i++ {
		s.part.chans[fmt.Sprintf("g:s%d", i)] = &partChannel{touched: oldest, spoken: []time.Time{now.Add(-time.Hour)}}
	}
	s.sweepPartChannels(now)
	if len(s.part.chans) != partMaxChannels+10 {
		t.Fatalf("%d channels kept: the sweep dropped limits to meet the cap", len(s.part.chans))
	}

	// once the limits have run out the idle ones go
	s.sweepPartChannels(now.Add(48 * time.Hour))
	if len(s.part.chans) != 0 {
		t.Fatalf("%d channels kept after every limit ran out", len(s.part.chans))
	}
}
