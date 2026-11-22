package application

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

// These tests pin what the participation back-off keeps and stores, so the change that moved the store
// reads and writes out of part.mu (fix plan R03) is checked to keep them.

func spokeCandidate(channel string) *partCandidate {
	return &partCandidate{key: "g:" + channel, guild: "g", channel: channel, kind: "answer"}
}

// A remark of the bot's own is kept in memory (the last partSpokesKept) and its turn is stored for a 👎
// after a restart: a day at most, 200 at most.
func TestNoteSpokeKeepsRecentAndStoresTurns(t *testing.T) {
	s, clk := banterLimitsService(t)
	cand := spokeCandidate("a")
	for i := range 205 {
		clk.t = clk.t.Add(time.Second)
		s.noteSpoke(cand, fmt.Sprintf("msg-%03d", i), fmt.Sprintf("turn-%03d", i))
	}
	s.part.mu.Lock()
	spokes := append([]partSpoke(nil), s.part.chans["g:a"].spokes...)
	s.part.mu.Unlock()
	if len(spokes) != partSpokesKept || spokes[0].turnID != "turn-197" || spokes[len(spokes)-1].turnID != "turn-204" {
		t.Fatalf("kept spokes: %d, first %q, last %q", len(spokes), spokes[0].turnID, spokes[len(spokes)-1].turnID)
	}
	if n := countWhere(t, s, `SELECT COUNT(*) FROM participation_spoke_turns`); n != 200 {
		t.Fatalf("stored spoke turns = %d, want 200", n)
	}
	if n := countWhere(t, s, `SELECT COUNT(*) FROM participation_spoke_turns WHERE turn_id IN ('turn-000','turn-004')`); n != 0 {
		t.Fatal("the oldest spoke turns were kept over the cap")
	}
	if g, c, ok := s.persistedPartSpoke("turn-204"); !ok || g != "g" || c != "a" {
		t.Fatalf("the newest spoke turn is not found: %q %q %v", g, c, ok)
	}
	clk.t = clk.t.Add(25 * time.Hour)
	s.noteSpoke(cand, "msg-late", "turn-late")
	if n := countWhere(t, s, `SELECT COUNT(*) FROM participation_spoke_turns`); n != 1 {
		t.Fatalf("spoke turns older than a day left: %d rows", n)
	}
	s.noteSpoke(cand, "msg-no-turn", "")
	if n := countWhere(t, s, `SELECT COUNT(*) FROM participation_spoke_turns`); n != 1 {
		t.Fatal("a remark without a turn was stored")
	}
}

// Remarks noted at the same time are all stored.
func TestNoteSpokeConcurrentAllStored(t *testing.T) {
	s, _ := banterLimitsService(t)
	const n = 30
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.noteSpoke(spokeCandidate(fmt.Sprintf("c%d", i%3)), fmt.Sprintf("msg-%d", i), fmt.Sprintf("turn-%d", i))
		}()
	}
	wg.Wait()
	if got := countWhere(t, s, `SELECT COUNT(*) FROM participation_spoke_turns`); got != n {
		t.Fatalf("stored spoke turns = %d, want %d", got, n)
	}
	s.part.mu.Lock()
	defer s.part.mu.Unlock()
	for i := range 3 {
		if c := s.part.chans[fmt.Sprintf("g:c%d", i)]; c == nil || len(c.spokes) != partSpokesKept {
			t.Fatalf("channel c%d keeps %v spokes, want %d", i, c, partSpokesKept)
		}
	}
}

// A channel entry made anew (a restart, or after it was dropped for being idle) takes its mute from the
// decision log: a quiet cue or a dislike in the last partMute still mutes, an older one does not.
func TestPartChannelRestoresTheMuteFromTheLog(t *testing.T) {
	s, clk := banterLimitsService(t)
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	logBackoff := func(channel, decision string, at time.Time) {
		t.Helper()
		if _, err := db.DB.Exec(`INSERT INTO conversation_log(id,created_at,guild_id,channel_id,kind,decision)
			VALUES(?,?,?,?,?,?)`, "cv_"+channel+decision, at.UTC().Format(time.RFC3339Nano), "g", channel, convBackoff, decision); err != nil {
			t.Fatal(err)
		}
	}
	logBackoff("recent", "quiet_cue", clk.t.Add(-30*time.Minute))
	logBackoff("disliked", "disliked", clk.t.Add(-time.Hour))
	logBackoff("old", "quiet_cue", clk.t.Add(-partMute-time.Minute))
	for channel, want := range map[string]string{"recent": "muted", "disliked": "muted", "old": "", "none": ""} {
		s.stillCurrent(&partCandidate{key: "g:" + channel}) // makes the entry, as every caller does
		s.part.mu.Lock()
		c := s.part.chans["g:"+channel]
		got, loaded := backoffBlocked(c, clk.t), c.backoffLoaded
		s.part.mu.Unlock()
		if got != want || !loaded {
			t.Fatalf("%s: blocked %q (loaded %v), want %q", channel, got, loaded, want)
		}
	}
}
