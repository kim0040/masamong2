package application

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"masamong/internal/config"
	"masamong/internal/features/privacy"
	"masamong/internal/testutil"
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

// A channel's participation decision does not wait while another channel's reads or writes the store
// (fix plan R03).
func TestParticipationDecisionsDoNotWaitForTheStore(t *testing.T) {
	t.Run("spoke write", func(t *testing.T) {
		s, _ := banterLimitsService(t)
		s.stillCurrent(spokeCandidate("b"))
		release := takeWriter(t, s)
		noted := make(chan struct{})
		go func() { defer close(noted); s.noteSpoke(spokeCandidate("a"), "msg-1", "turn-1") }()
		waitsInStore(t, "(*Service).noteSpoke(")
		other := returnsWhileWaiting(t, "another channel's decision", func() { s.stillCurrent(spokeCandidate("b")) })
		release()
		<-noted
		<-other
		if g, c, ok := s.persistedPartSpoke("turn-1"); !ok || g != "g" || c != "a" {
			t.Fatal("the remark's turn was not stored once the store was free")
		}
	})
	t.Run("mute restore", func(t *testing.T) {
		s, _ := banterLimitsService(t)
		s.stillCurrent(spokeCandidate("b"))
		release := holdReaders(t, s)
		made := make(chan struct{})
		go func() { defer close(made); s.stillCurrent(spokeCandidate("new")) }()
		waitsInStore(t, "(*Service).stillCurrent(")
		other := returnsWhileWaiting(t, "another channel's decision", func() { s.stillCurrent(spokeCandidate("b")) })
		release()
		<-made
		<-other
		s.part.mu.Lock()
		loaded := s.part.chans["g:new"].backoffLoaded
		s.part.mu.Unlock()
		if !loaded {
			t.Fatal("the new entry's mute was not read once the store was free")
		}
	})
	t.Run("consent read", func(t *testing.T) {
		s, clk := banterLimitsService(t)
		_, drain := ManualParticipationForTest(s)
		doc := s.Doc()
		doc.Discord.Enabled = true
		doc.Participation = config.Participation{Enabled: true, Mode: "conservative"}
		doc.Guilds = map[string]config.GuildSettings{"g": {Locale: "ko", Channels: map[string]config.ChannelPolicy{
			"a": {Respond: "mention", Participation: "conservative"}, "b": {Respond: "mention", Participation: "conservative"}}}}
		s.setDoc(doc, s.Revision())
		// a person who withdrew memory consent: their message is not counted for the ranking, so the
		// first store access of Observe is the participation consent check
		if _, err := s.decideConsent("no-memory", privacy.Memory, privacy.Withdrawn, "test"); err != nil {
			t.Fatal(err)
		}
		// the withdrawal's redaction opens the memory engine under the service's life lock, which Observe
		// takes first (beginWork): let it finish before the store is held
		testutil.WaitFor(t, lockScopeWait, "the redaction to finish", func() bool {
			pending, err := s.redactionPending()
			return err == nil && !pending && !s.redaction.running.Load()
		})
		// load what Observe reads before its lock (the opt-out list, the channel's thread lookup) before
		// the store is held
		s.MemoryAllowed("no-memory")
		s.Observe(ObservedMessage{GuildID: "g", ChannelID: "a", MessageID: "m0", AuthorID: "someone", AuthorName: "y", Content: "안녕", At: clk.t})
		s.stillCurrent(spokeCandidate("b"))
		release := takeWriter(t, s)
		observed := make(chan struct{})
		go func() {
			defer close(observed)
			s.Observe(ObservedMessage{GuildID: "g", ChannelID: "a", MessageID: "m1", AuthorID: "no-memory", AuthorName: "x",
				Content: "누가 텐트 챙기기로 했지?", At: clk.t})
		}()
		waitsInStore(t, "(*Service).Observe(")
		other := returnsWhileWaiting(t, "another channel's decision", func() { s.watching("g:b") })
		release()
		<-observed
		<-other
		drain()
		s.part.mu.Lock()
		pending := s.part.chans["g:a"].pending != nil
		s.part.mu.Unlock()
		if !pending {
			t.Fatal("the question did not become a candidate once the store was free")
		}
	})
}

// Observe reads whether the author may be counted only when a collected row waited before it took the
// lock (nil otherwise). A row collected since is newer than the message: it is left for the next one,
// except that a row whose window ended is settled as silent all the same.
func TestAdvanceWatchesWithoutTheAuthorsAnswer(t *testing.T) {
	s, clk := banterLimitsService(t)
	c := &partChannel{watches: []nextWatch{
		{row: "open", msg: "src-1", author: "u1", until: clk.t.Add(time.Minute)},
		{row: "ended", msg: "src-2", author: "u1", until: clk.t.Add(-time.Second)},
	}}
	m := ObservedMessage{GuildID: "g", ChannelID: "a", MessageID: "m2", AuthorID: "u1", At: clk.t}
	done := s.advanceWatches(c, m, nil)
	if len(done) != 1 || done[0].row != "ended" || done[0].next != "silent" {
		t.Fatalf("settled %+v, want only the ended row as silent", done)
	}
	if len(c.watches) != 1 || c.watches[0].row != "open" {
		t.Fatalf("kept %+v, want the open row", c.watches)
	}
	yes := true
	if done := s.advanceWatches(c, m, &yes); len(done) != 1 || done[0].next != "continued" {
		t.Fatalf("with the author counted: %+v, want continued", done)
	}
}
