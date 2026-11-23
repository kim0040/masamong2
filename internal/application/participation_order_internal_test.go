package application

import (
	"testing"
	"time"

	"masamong/internal/config"
	"masamong/internal/features/privacy"
	"masamong/internal/testutil"
)

// Observe and considerCandidate read the store before they take part.mu (fix plan R03). These tests pin
// what reading under the lock gave and the reads must not lose, and that no read waits under the lock
// (Sol review of E8).

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
