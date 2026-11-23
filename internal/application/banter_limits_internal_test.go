package application

import (
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"masamong/internal/testutil"
)

// These tests pin the banter ladder's limits and what is stored for a restart, so the change that moved
// the store reads and writes out of banter.mu (fix plan R03) is checked to keep them.

func banterLimitsService(t *testing.T) (*Service, *movableClock) {
	t.Helper()
	s, clk := deliveryService(t)
	clk.t = time.Date(2026, 11, 23, 12, 0, 0, 0, time.UTC)
	return s, clk
}

// reopenForTest closes s and opens its data folder again, as a restart does, with the same clock.
func reopenForTest(t *testing.T, s *Service, clk *movableClock) *Service {
	t.Helper()
	dir := s.DataRoot
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	next, err := Open(dir, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = next.Close() })
	next.Clock = clk
	return next
}

func banterRow(t *testing.T, s *Service, guildID, channelID string) (lines, emoji int, muted string, ok bool) {
	t.Helper()
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	var linesRaw, emojiRaw string
	err = db.DB.QueryRow(`SELECT lines,emoji,muted_until FROM banter_channel_state WHERE guild_id=? AND channel_id=?`, guildID, channelID).
		Scan(&linesRaw, &emojiRaw, &muted)
	if err != nil {
		return 0, 0, "", false
	}
	var l, e []time.Time
	if err := json.Unmarshal([]byte(linesRaw), &l); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(emojiRaw), &e); err != nil {
		t.Fatal(err)
	}
	return len(l), len(e), muted, true
}

func countWhere(t *testing.T, s *Service, query string, args ...any) int {
	t.Helper()
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	var n int
	if err := db.DB.QueryRow(query, args...).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

// Two lines an hour, then up to four emoji, then silence; channels count apart; an hour later lines
// come back. A fast room gets only an emoji.
func TestBanterLadderLimitsPerChannel(t *testing.T) {
	s, clk := banterLimitsService(t)
	want := func(channel, step string) {
		t.Helper()
		if got := s.banterStep("g", channel); got != step {
			t.Fatalf("banterStep(%s) = %q, want %q", channel, got, step)
		}
	}
	for i := range banterLinesPerHour {
		want("a", "line")
		s.noteBanter("g", "a", "line", fmt.Sprintf("turn-%d", i))
	}
	for range banterEmojiPerHour {
		want("a", "emoji")
		s.noteBanter("g", "a", "emoji", "")
	}
	want("a", "")
	want("b", "line")
	clk.t = clk.t.Add(time.Hour + time.Second)
	want("a", "line")

	for i := range banterFastPerMin {
		s.noteLive(ObservedMessage{GuildID: "g", ChannelID: "fast", MessageID: fmt.Sprintf("m%d", i), AuthorID: "u1", Content: "ㅋㅋ", At: clk.t})
	}
	want("fast", "emoji")
}

// A 👎 on a banter line stops lines in its channel for banterMute; emoji still go. A rating of a turn that
// was not banter changes nothing.
func TestBanterDislikeMutesLinesNotEmoji(t *testing.T) {
	s, clk := banterLimitsService(t)
	if s.banterStep("g", "a") != "line" {
		t.Fatal("precondition: a line is allowed")
	}
	s.noteBanter("g", "a", "line", "turn-1")
	s.banterDisliked("not-banter")
	if got := s.banterStep("g", "a"); got != "line" {
		t.Fatalf("an unrelated rating muted the channel: %q", got)
	}
	s.banterDisliked("turn-1")
	if got := s.banterStep("g", "a"); got != "emoji" {
		t.Fatalf("after a dislike: %q, want emoji", got)
	}
	if got := s.banterStep("g", "b"); got != "line" {
		t.Fatalf("another channel is not muted: %q", got)
	}
	clk.t = clk.t.Add(banterMute + time.Second)
	if got := s.banterStep("g", "a"); got != "line" {
		t.Fatalf("after the mute: %q, want line", got)
	}
}

// The counts and the mute are stored: a restart keeps the limits, and a dislike after a restart finds the
// banter turn. Expired state is cleaned from the store when the channel is next checked.
func TestBanterStateSurvivesRestart(t *testing.T) {
	s, clk := banterLimitsService(t)
	s.banterStep("g", "a")
	s.noteBanter("g", "a", "line", "turn-1")
	s.noteBanter("g", "a", "line", "turn-2")
	if lines, _, _, ok := banterRow(t, s, "g", "a"); !ok || lines != 2 {
		t.Fatalf("stored lines = %d (row %v), want 2", lines, ok)
	}
	s = reopenForTest(t, s, clk)
	if got := s.banterStep("g", "a"); got != "emoji" {
		t.Fatalf("after a restart the two lines still count: %q", got)
	}

	s.banterStep("g", "b")
	s.noteBanter("g", "b", "line", "turn-b")
	s = reopenForTest(t, s, clk)
	s.banterDisliked("turn-b")
	if _, _, muted, ok := banterRow(t, s, "g", "b"); !ok || muted == "" {
		t.Fatalf("the mute is not stored (row %v, muted %q)", ok, muted)
	}
	s = reopenForTest(t, s, clk)
	if got := s.banterStep("g", "b"); got != "emoji" {
		t.Fatalf("the mute did not survive a restart: %q", got)
	}

	clk.t = clk.t.Add(banterMute + time.Second)
	s = reopenForTest(t, s, clk)
	if got := s.banterStep("g", "a"); got != "line" {
		t.Fatalf("expired lines still count: %q", got)
	}
	if got := s.banterStep("g", "b"); got != "line" {
		t.Fatalf("an expired mute still counts: %q", got)
	}
	if n := countWhere(t, s, `SELECT COUNT(*) FROM banter_channel_state`); n != 0 {
		t.Fatalf("expired channel rows left: %d", n)
	}
}

// Banter turns (for a later 👎) are kept for a day, at most 200.
func TestBanterTurnsCappedAndPruned(t *testing.T) {
	s, clk := banterLimitsService(t)
	s.banterStep("g", "a")
	for i := range 205 {
		clk.t = clk.t.Add(time.Second)
		s.noteBanter("g", "a", "line", fmt.Sprintf("turn-%03d", i))
	}
	if n := countWhere(t, s, `SELECT COUNT(*) FROM banter_turns`); n != 200 {
		t.Fatalf("stored banter turns = %d, want 200", n)
	}
	if n := countWhere(t, s, `SELECT COUNT(*) FROM banter_turns WHERE turn_id IN ('turn-000','turn-004')`); n != 0 {
		t.Fatal("the oldest turns were kept over the cap")
	}
	if n := countWhere(t, s, `SELECT COUNT(*) FROM banter_turns WHERE turn_id='turn-204'`); n != 1 {
		t.Fatal("the newest turn was dropped")
	}
	clk.t = clk.t.Add(25 * time.Hour)
	s.banterStep("g", "a")
	s.noteBanter("g", "a", "line", "turn-late")
	if n := countWhere(t, s, `SELECT COUNT(*) FROM banter_turns`); n != 1 {
		t.Fatalf("turns older than a day left: %d rows", n)
	}
}

// Notes made at the same time all count, in memory and in the store: the stored row is the latest state,
// not an older one written last.
func TestBanterConcurrentNotesAllStored(t *testing.T) {
	s, clk := banterLimitsService(t)
	s.banterStep("g", "a")
	const n = 24
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(2)
		go func() { defer wg.Done(); s.noteBanter("g", "a", "line", fmt.Sprintf("turn-%d", i)) }()
		go func() { defer wg.Done(); s.noteBanter("g", "a", "emoji", "") }()
	}
	wg.Wait()
	s.banter.mu.Lock()
	memLines, memEmoji := len(s.banter.chans["g:a"].lines), len(s.banter.chans["g:a"].emoji)
	s.banter.mu.Unlock()
	if memLines != n || memEmoji != n {
		t.Fatalf("in memory: %d lines, %d emoji, want %d each", memLines, memEmoji, n)
	}
	if lines, emoji, _, ok := banterRow(t, s, "g", "a"); !ok || lines != n || emoji != n {
		t.Fatalf("stored: %d lines, %d emoji (row %v), want %d each", lines, emoji, ok, n)
	}
	if got := countWhere(t, s, `SELECT COUNT(*) FROM banter_turns`); got != n {
		t.Fatalf("stored banter turns = %d, want %d", got, n)
	}
	s = reopenForTest(t, s, clk)
	if got := s.banterStep("g", "a"); got != "" {
		t.Fatalf("after a restart the full ladder still counts: %q", got)
	}
}

// A channel's banter decision does not wait while another channel's banter reads or writes the store
// (fix plan R03).
func TestBanterDecisionsDoNotWaitForTheStore(t *testing.T) {
	t.Run("write", func(t *testing.T) {
		s, _ := banterLimitsService(t)
		s.banterStep("g", "a")
		s.banterStep("g", "b")
		s.banterDisliked("turn-unknown") // reads the stored turns once: later dislikes need only memory
		release := takeWriter(t, s)
		noted := make(chan struct{})
		go func() { defer close(noted); s.noteBanter("g", "a", "line", "turn-1") }()
		waitsInStore(t, "(*Service).noteBanter(")
		var step string
		stepped := returnsWhileWaiting(t, "banterStep", func() { step = s.banterStep("g", "b") })
		disliked := returnsWhileWaiting(t, "banterDisliked", func() { s.banterDisliked("turn-unknown") })
		release()
		<-noted
		<-stepped
		<-disliked
		if step != "line" {
			t.Fatalf("banterStep = %q", step)
		}
		if lines, _, _, ok := banterRow(t, s, "g", "a"); !ok || lines != 1 {
			t.Fatalf("the note was not stored once the store was free: %d lines (row %v)", lines, ok)
		}
	})
	t.Run("read", func(t *testing.T) {
		s, _ := banterLimitsService(t)
		s.banterStep("g", "b")
		release := holdReaders(t, s)
		stepped := make(chan struct{})
		go func() { defer close(stepped); s.banterStep("g", "new") }()
		waitsInStore(t, "(*Service).banterStep(")
		other := returnsWhileWaiting(t, "banterStep of a loaded channel", func() { s.banterStep("g", "b") })
		release()
		<-stepped
		<-other
	})
}

// syncClock is a clock the test moves while the service's goroutines read it.
type syncClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *syncClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *syncClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

// The first 👎 lookup reads the stored turns while a banter line is noted: the noted turn stays, in memory
// and in the store, so a 👎 on it still mutes lines (Sol review of E8, finding 2).
func TestBanterTurnNotedWhileTheTurnsAreReadIsKept(t *testing.T) {
	s, mc := banterLimitsService(t)
	clk := &syncClock{t: mc.t}
	s.Clock = clk
	if s.banterStep("g", "a") != "line" {
		t.Fatal("precondition: a line is allowed")
	}
	release := takeWriter(t, s)
	looked := make(chan struct{})
	go func() { defer close(looked); s.banterDisliked("turn-unknown") }() // the first lookup reads the turns
	waitsInStore(t, "(*Service).loadBanterTurns(")
	clk.Advance(2 * time.Minute) // past any slack for a clock that runs on while the store waits
	noted := make(chan struct{})
	go func() { defer close(noted); s.noteBanter("g", "a", "line", "turn-new") }()
	testutil.WaitFor(t, lockScopeWait, "the note in memory", func() bool {
		s.banter.mu.Lock()
		defer s.banter.mu.Unlock()
		_, ok := s.banter.turns["turn-new"]
		return ok
	})
	release()
	<-looked
	<-noted
	if n := countWhere(t, s, `SELECT COUNT(*) FROM banter_turns WHERE turn_id='turn-new'`); n != 1 {
		t.Fatalf("the noted turn's row: %d, want 1", n)
	}
	s.banterDisliked("turn-new")
	if got := s.banterStep("g", "a"); got != "emoji" {
		t.Fatalf("after a 👎 on the noted line: %q, want emoji", got)
	}
}

// After the clock goes back a little, a stored banter turn dated up to a minute ahead is kept for a later
// run, as before E8; one further ahead is deleted (Sol round-2 review of E8, regression R3).
func TestBanterTurnSlightlyAheadAfterAClockRollbackIsKept(t *testing.T) {
	s, clk := banterLimitsService(t)
	start := clk.t
	s.banterStep("g", "a")
	s.noteBanter("g", "a", "line", "turn-near")
	clk.t = start.Add(2 * time.Minute)
	s.banterStep("g", "b")
	s.noteBanter("g", "b", "line", "turn-far")
	clk.t = start.Add(-30 * time.Second)
	s = reopenForTest(t, s, clk)
	s.banterDisliked("turn-unknown") // the first lookup reads the stored turns
	if n := countWhere(t, s, `SELECT COUNT(*) FROM banter_turns WHERE turn_id='turn-near'`); n != 1 {
		t.Fatalf("the turn 30 s ahead: %d rows, want 1", n)
	}
	if n := countWhere(t, s, `SELECT COUNT(*) FROM banter_turns WHERE turn_id='turn-far'`); n != 0 {
		t.Fatalf("the turn 2.5 min ahead: %d rows, want 0", n)
	}
	clk.t = start.Add(3 * time.Minute)
	s = reopenForTest(t, s, clk)
	s.banterDisliked("turn-near")
	if got := s.banterStep("g", "a"); got != "emoji" {
		t.Fatalf("after a 👎 on the kept line: %q, want emoji", got)
	}
}
