package application

import (
	"runtime"
	"strings"
	"testing"
	"time"

	"masamong/internal/config"
	"masamong/internal/features/privacy"
	"masamong/internal/testutil"
)

// Observe and considerCandidate read the store before they take part.mu (fix plan R03). These tests pin
// what reading under the lock gave and the reads must not lose: a message makes the room move on before
// its reads, the mode and the author's consent are what they are once the lock is taken, and no read waits
// under the lock (Sol review of E8, findings 3, 4, 5 and 7).

const partQuestion = "누가 텐트 챙기기로 했지?"

// watching is whether a channel has collected rows waiting: a call that needs nothing but part.mu.
func (s *Service) watching(key string) bool {
	s.part.mu.Lock()
	defer s.part.mu.Unlock()
	c := s.part.chans[key]
	return c != nil && len(c.watches) > 0
}

// partOrderService has participation on, with the channels "a" and "b" in mode, and the work Observe
// starts queued until drain runs it.
func partOrderService(t *testing.T, mode string) (s *Service, clk *movableClock, drain func()) {
	t.Helper()
	s, clk = banterLimitsService(t)
	_, drain = ManualParticipationForTest(s)
	doc := s.Doc()
	doc.Discord.Enabled = true
	doc.Memory.Capture = true
	doc.Participation = config.Participation{Enabled: true, Mode: "conservative"}
	doc.Guilds = map[string]config.GuildSettings{"g": {Locale: "ko", Channels: map[string]config.ChannelPolicy{
		"a": {Respond: "mention", Participation: mode}, "b": {Respond: "mention", Participation: mode}}}}
	s.setDoc(doc, s.Revision())
	return s, clk, drain
}

// withdrawMemory makes userID a person who withdrew memory consent: their message is not counted for the
// ranking, so the first store access of their Observe is the participation consent check.
func withdrawMemory(t *testing.T, s *Service, userID string) {
	t.Helper()
	if _, err := s.decideConsent(userID, privacy.Memory, privacy.Withdrawn, "test"); err != nil {
		t.Fatal(err)
	}
	// the withdrawal's redaction opens the memory engine under the service's life lock, which Observe takes
	// first (beginWork): let it finish before the store is held
	testutil.WaitFor(t, lockScopeWait, "the redaction to finish", func() bool {
		pending, err := s.redactionPending()
		return err == nil && !pending && !s.redaction.running.Load()
	})
	s.MemoryAllowed(userID) // the opt-out list is read now, not while the store is held
}

// A message from someone else makes the room move on before Observe reads the store for it: a waiting
// candidate is taken off the wait, and a draft in flight fails its final check, while the reads wait.
func TestObserveMovesTheRoomOnBeforeItReadsTheStore(t *testing.T) {
	for _, inFlight := range []bool{false, true} {
		name := "waiting"
		if inFlight {
			name = "in flight"
		}
		t.Run(name, func(t *testing.T) {
			s, clk, drain := partOrderService(t, "conservative")
			withdrawMemory(t, s, "no-memory")
			s.Observe(ObservedMessage{GuildID: "g", ChannelID: "a", MessageID: "m1", AuthorID: "u1", AuthorName: "u1", Content: partQuestion, At: clk.t})
			s.part.mu.Lock()
			c := s.part.chans["g:a"]
			cand := c.pending
			if cand != nil && inFlight {
				c.pending = nil // considerCandidate took it: it is judged and drafted now
			}
			s.part.mu.Unlock()
			if cand == nil {
				t.Fatal("precondition: the question is a candidate")
			}
			release := takeWriter(t, s)
			observed := make(chan struct{})
			go func() {
				defer close(observed)
				s.Observe(ObservedMessage{GuildID: "g", ChannelID: "a", MessageID: "m2", AuthorID: "no-memory", AuthorName: "x",
					Content: "그럼 버너는 누가 챙기지?", At: clk.t})
			}()
			waitsInStore(t, "(*Service).Observe(")
			if inFlight {
				if s.stillCurrent(cand) {
					t.Error("a draft passes the final check while a later message's reads wait")
				}
			} else {
				s.part.mu.Lock()
				waiting := s.part.chans["g:a"].pending == cand
				s.part.mu.Unlock()
				if waiting {
					t.Error("the candidate still waits while a later message from someone else reads the store")
				}
			}
			release()
			<-observed
			drain()
			if !inFlight {
				if n := countWhere(t, s, `SELECT COUNT(*) FROM participation_log WHERE message_id='m1' AND state=? AND reason='people_answered'`, partCancelled); n != 1 {
					t.Fatalf("the answered candidate's cancellation: %d rows, want 1", n)
				}
			}
		})
	}
}

// Two messages whose reads wait at once are decided in the order they came, whichever read ends first: the
// earlier question was answered by the later message from someone else, so it is logged as cancelled and
// the later one is the candidate.
func TestObserveDecidesInTheOrderMessagesCame(t *testing.T) {
	s, clk, drain := partOrderService(t, "conservative")
	withdrawMemory(t, s, "no-memory")
	withdrawMemory(t, s, "no-memory-2")
	s.Observe(ObservedMessage{GuildID: "g", ChannelID: "a", MessageID: "m0", AuthorID: "u0", AuthorName: "u0", Content: "안녕", At: clk.t})
	release := takeWriter(t, s)
	observed := make(chan struct{}, 2)
	say := func(id, author, text string) {
		go func() {
			defer func() { observed <- struct{}{} }()
			s.Observe(ObservedMessage{GuildID: "g", ChannelID: "a", MessageID: id, AuthorID: author, AuthorName: author, Content: text, At: clk.t})
		}()
	}
	say("m1", "no-memory", partQuestion)
	waitsInStore(t, "(*Service).Observe(")
	say("m2", "no-memory-2", "그럼 버너는 누가 챙기지?")
	testutil.WaitFor(t, lockScopeWait, "the second message to be said", func() bool {
		s.part.mu.Lock()
		defer s.part.mu.Unlock()
		return s.part.chans["g:a"].version == 3
	})
	release()
	<-observed
	<-observed
	drain()
	s.part.mu.Lock()
	pending := s.part.chans["g:a"].pending
	s.part.mu.Unlock()
	if pending == nil || pending.msg.id != "m2" {
		t.Fatalf("waiting candidate %+v, want the later question m2", pending)
	}
	if n := countWhere(t, s, `SELECT COUNT(*) FROM participation_log WHERE message_id='m1' AND state=? AND reason='people_answered'`, partCancelled); n != 1 {
		t.Fatalf("the earlier question's cancellation: %d rows, want 1", n)
	}
}

// The back-off uses the mode current once the new entry's mute is restored: a switch to a speaking mode
// while the restore waits still starts the pause after a call to the bot.
func TestObserveUsesTheModeCurrentAfterTheMuteRestore(t *testing.T) {
	s, clk, drain := partOrderService(t, "off")
	s.Paused("g", "a") // the thread lookup is warm: only the mute restore reads the store
	release := holdReaders(t, s)
	called := make(chan struct{})
	go func() {
		defer close(called)
		s.Observe(ObservedMessage{GuildID: "g", ChannelID: "a", MessageID: "m1", AuthorID: "u1", AuthorName: "u1",
			Content: "마사몽 이거 알아?", At: clk.t, Addressed: true, BotCall: "mention"})
	}()
	waitsInStore(t, "(*Service).Observe(")
	doc := s.Doc() // a snapshot others read: its maps are replaced, not changed
	doc.Guilds = map[string]config.GuildSettings{"g": {Locale: "ko", Channels: map[string]config.ChannelPolicy{
		"a": {Respond: "mention", Participation: "conservative"}, "b": {Respond: "mention", Participation: "off"}}}}
	s.setDoc(doc, s.Revision())
	release()
	<-called
	s.Observe(ObservedMessage{GuildID: "g", ChannelID: "a", MessageID: "m2", AuthorID: "u2", AuthorName: "u2", Content: partQuestion, At: clk.t})
	drain()
	s.part.mu.Lock()
	c := s.part.chans["g:a"]
	afterCall, pending := c.filtered["after_call"], c.pending != nil
	s.part.mu.Unlock()
	if afterCall != 1 || pending {
		t.Fatalf("question right after a call: after_call %d, candidate %v; want filtered as after_call", afterCall, pending)
	}
}

// A collected row that began waiting while a message's reads ran is settled by that message, with the
// author's consent read for it.
func TestObserveSettlesAWatchThatBeganWhileItRead(t *testing.T) {
	s, clk, drain := partOrderService(t, "collect")
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.DB.Exec(`INSERT INTO participation_log(id, guild_id, channel_id, message_id, created_at, mode, kind, state, reason, score,
		message, draft, features, cost_usd, author_ref, evidence_state) VALUES('pl_seed','g','a','src',?,'collect','question',?,'unanswered',0,'','','{"kind":"question"}',0,?,1)`,
		clk.t.UTC().Format(time.RFC3339), partCollected, personRef("u1")); err != nil {
		t.Fatal(err)
	}
	s.Paused("g", "a") // the thread lookup is warm: only the new entry's mute restore reads the store
	release := holdReaders(t, s)
	answered := make(chan struct{})
	go func() {
		defer close(answered)
		s.Observe(ObservedMessage{GuildID: "g", ChannelID: "a", MessageID: "m2", AuthorID: "u2", AuthorName: "u2",
			Content: "도윤이가 챙긴대", At: clk.t, ReplyTo: "src", ReplyAuthorID: "u1", ToSomeone: true})
	}()
	waitsInStore(t, "(*Service).Observe(")
	s.part.mu.Lock()
	c := s.partChannel("g:a")
	c.watches = append(c.watches, nextWatch{row: "pl_seed", msg: "src", author: "u1", until: clk.t.Add(collectNextWindow)})
	s.part.mu.Unlock()
	release()
	<-answered
	drain()
	var next string
	if err := db.DB.QueryRow(`SELECT COALESCE(json_extract(features,'$.next'),'') FROM participation_log WHERE id='pl_seed'`).Scan(&next); err != nil {
		t.Fatal(err)
	}
	if next != "answered" {
		t.Fatalf("features.next = %q, want answered", next)
	}
}

// considerCandidate reads the channel's mode before it takes part.mu: a cold thread lookup for an
// unconfigured channel holds up no other channel's decision.
func TestConsiderCandidateReadsTheModeBeforeTheLock(t *testing.T) {
	s, clk, _ := partOrderService(t, "conservative")
	doc := s.Doc()
	doc.Access.BlockedUsers = []string{"blocked-user"} // the block check skips the consent and pause reads
	s.setDoc(doc, s.Revision())
	s.stillCurrent(spokeCandidate("b"))
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	cand := &partCandidate{key: "g:unlisted", guild: "g", channel: "unlisted", kind: "question", timer: timer,
		msg: partMsg{id: "m1", author: "blocked-user", name: "x", text: partQuestion, at: clk.t}}
	s.part.mu.Lock()
	c := s.partChannel(cand.key)
	c.backoffLoaded, c.pending = true, cand
	s.part.mu.Unlock()
	release := holdReaders(t, s)
	considered := make(chan struct{})
	go func() { defer close(considered); s.considerCandidate(cand) }()
	waitsInStore(t, "(*Service).considerCandidate(")
	other := returnsWhileWaiting(t, "another channel's decision", func() { s.watching("g:b") })
	release()
	<-considered
	<-other
}

// A call to the bot held at its cold mute restore still stops a draft made after it arrived: a chime does not
// post once the call has set the after-call pause, as when the call moved the room on under the lock (Sol
// round-2 review of E8, regression R2).
func TestAFinalCheckFailsOnceAHeldCallSetsTheAfterCallPause(t *testing.T) {
	s, clk, drain := partOrderService(t, "conservative")
	s.Paused("g", "a") // the thread lookup is warm: only the mute restore reads the store
	release := holdReaders(t, s)
	called := make(chan struct{})
	go func() {
		defer close(called)
		s.Observe(ObservedMessage{GuildID: "g", ChannelID: "a", MessageID: "m1", AuthorID: "u1", AuthorName: "u1",
			Content: "마사몽 이거 알아?", At: clk.t, Addressed: true, BotCall: "mention"})
	}()
	waitsInStore(t, "(*Service).Observe(")
	s.part.mu.Lock()
	s.partChannel("g:a").backoffLoaded = true // as a concurrent restore would
	s.part.mu.Unlock()
	s.Observe(ObservedMessage{GuildID: "g", ChannelID: "a", MessageID: "m2", AuthorID: "u2", AuthorName: "u2", Content: partQuestion, At: clk.t})
	s.part.mu.Lock()
	c := s.part.chans["g:a"]
	cand := c.pending
	c.pending = nil // considerCandidate took it: it is judged and drafted now
	s.part.mu.Unlock()
	if cand == nil {
		t.Fatal("precondition: the question is a candidate")
	}
	release()
	<-called
	drain()
	s.part.mu.Lock()
	lastCall := s.part.chans["g:a"].lastCall
	s.part.mu.Unlock()
	if !lastCall.Equal(clk.t) {
		t.Fatalf("precondition: the call set the after-call pause (lastCall %v)", lastCall)
	}
	if s.stillCurrent(cand) {
		t.Fatal("a draft passes the final check after a call set the after-call pause")
	}
}

// lockClock is a fixed clock that runs onLock when lockPartChannel reads it: once per try of Observe's
// reads for a loaded entry (partChannel), after the try's reads and under part.mu.
type lockClock struct {
	t      time.Time
	onLock func()
}

func (c *lockClock) Now() time.Time {
	pc := make([]uintptr, 32)
	frames := runtime.CallersFrames(pc[:runtime.Callers(2, pc)])
	for {
		f, more := frames.Next()
		if strings.HasSuffix(f.Function, "(*Service).lockPartChannel") {
			c.onLock()
			break
		}
		if !more {
			break
		}
	}
	return c.t
}

// When the configuration changes during every read, the last read still has the author's answer for
// collected rows that began waiting during it: the retry limit does not leave advanceWatches without it, so
// the row gets this message as its answer (Sol round-2 review of E8, finding 5).
func TestObserveReadsTheAuthorsAnswerAtTheRetryLimit(t *testing.T) {
	s, clk, _ := partOrderService(t, "collect")
	s.Paused("g", "a") // the thread lookup is warm
	s.part.mu.Lock()
	s.partChannel("g:a").backoffLoaded = true // no mute restore: one clock read per try
	s.part.mu.Unlock()
	tries := 0
	s.Clock = &lockClock{t: clk.t, onLock: func() {
		tries++
		switch {
		case tries < partRereads:
			next := *s.cfg.Load() // the configuration changes during this try's reads
			s.cfg.Store(&next)
		case tries == partRereads: // a collected row begins waiting during the last try's reads
			c := s.part.chans["g:a"]
			c.watches = append(c.watches, nextWatch{row: "pl_seed", msg: "src", author: "u1", until: clk.t.Add(collectNextWindow)})
		}
	}}
	m := ObservedMessage{GuildID: "g", ChannelID: "a", MessageID: "m2", AuthorID: "u1", AuthorName: "u1", Content: partQuestion, At: clk.t}
	c, r := s.lockObserved("g:a", m, false)
	done := s.advanceWatches(c, m, r.counted)
	s.part.mu.Unlock()
	if tries < partRereads {
		t.Fatalf("precondition: %d tries, want the retry limit %d", tries, partRereads)
	}
	if len(done) != 1 || done[0].next != "continued" {
		t.Fatalf("settled rows = %+v, want pl_seed continued by this message", done)
	}
}
