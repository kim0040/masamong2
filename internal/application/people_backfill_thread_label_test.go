package application

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

// pbLabelSentinel is the display name person 103 has only in thread t1: a window of #c1 that mentions
// 103 shows it while the thread lives. Synthetic names, ids and text only.
const pbLabelSentinel = "Threadlabelsentinel"

// pbThreadLabelSeed gives 103 a label only in thread t1 (one short line there, under the meaningful-rune
// floor, so the thread's own window sends nothing) and adds 20 segments of #c1 in which Mina mentions 103
// by <@id>; Joon's line in each is the control that must still be sent.
func pbThreadLabelSeed(t *testing.T, s *Service) {
	t.Helper()
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if ok, err := e.Capture(engine.LiveMessage{GuildID: "g1", ChannelID: "t1", MessageID: "77001", AuthorID: "103", AuthorName: pbLabelSentinel,
		Content: "ok", At: time.Date(2026, 9, 1, 0, 30, 0, 0, time.UTC)}); err != nil || !ok {
		t.Fatal(ok, err)
	}
	for i := 70; i < 90; i++ {
		pbSegment(t, s, i, [3]string{"101", "Mina", fmt.Sprintf("<@103> is coming over mentionday%d tonight", i)},
			[3]string{"102", "Joon", fmt.Sprintf("I really like mentionctl%d too", i)})
	}
}

// pbDeleteThreadOnMention deletes thread t1 (the real path: pending in memory, then its tombstone) when the
// first window of #c1 that mentions 103 is built while armed is set, between that build and its send.
func pbDeleteThreadOnMention(t *testing.T, s *Service, armed *atomic.Bool) *atomic.Bool {
	t.Helper()
	var done atomic.Bool
	var mu sync.Mutex // a window built meanwhile waits here until the deletion is done
	pbBuilt = func(windowID string) {
		mu.Lock()
		defer mu.Unlock()
		if !armed.Load() || done.Load() {
			return
		}
		runs, err := store.BackfillRuns(s.Store.Reader(), "g1")
		if err != nil || len(runs) == 0 {
			t.Error("no run", err)
			return
		}
		w, _, err := store.LoadBackfillWindow(s.Store.Reader(), runs[0].ID, windowID)
		if err != nil || w.Room != engine.RoomKey("g1", "c1") {
			return
		}
		raw, _ := json.Marshal(w.Obs)
		var mentions bool
		if err := s.Store.Reader().QueryRow(`SELECT EXISTS(SELECT 1 FROM memory_observations WHERE observation_id IN (SELECT value FROM json_each(?)) AND instr(content,'<@103>')>0)`,
			string(raw)).Scan(&mentions); err != nil || !mentions {
			return
		}
		s.ThreadDeleted("t1")
		done.Store(true)
	}
	t.Cleanup(func() { pbBuilt = nil })
	return &done
}

// pbMentionWindowPrompt builds the prompt of a window of the run that mentions 103, as a send would now.
func pbMentionWindowPrompt(t *testing.T, s *Service, runID string) pbPrompt {
	t.Helper()
	ws, err := store.BackfillWindows(s.Store.Reader(), runID, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range ws {
		if w.Room != engine.RoomKey("g1", "c1") {
			continue
		}
		bp, err := s.pbBuildPrompt(s.Store, "g1", w)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(bp.user, "mentionday") {
			return bp
		}
	}
	t.Fatal("no window of the run mentions 103")
	return pbPrompt{}
}
