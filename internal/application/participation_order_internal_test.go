package application

import (
	"testing"
	"time"

	"masamong/internal/config"
)

// Observe and considerCandidate read the store before they take part.mu (fix plan R03). These tests pin
// what reading under the lock gave and the reads must not lose, and that no read waits under the lock
// (Sol review of E8).

const partQuestion = "누가 텐트 챙기기로 했지?"

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
