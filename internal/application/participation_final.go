package application

import "masamong/internal/features/privacy"

// partAllowed is what the person and the room allow a candidate: the person is not blocked here and has
// not withdrawn participation, and the channel is not paused. The quiet wait's end checks it
// (considerCandidate), and so does the final check before the bot speaks: a judge or a reply turn takes
// seconds, and a withdrawal in that time stops the reply and the offer as one during the wait does. It
// asks the store, so it is called outside s.part.mu.
func (s *Service) partAllowed(cand *partCandidate) bool {
	return !s.BlockedIn(cand.guild, cand.msg.author) && s.ConsentAllows(cand.msg.author, privacy.Participation) && !s.Paused(cand.guild, cand.channel)
}

// partFinal is the final check before a plan's offer is saved and posted. A candidate the person or the
// room no longer allows ends with no log entry, as one dropped at the wait's end does; one the
// conversation moved past is logged as cancelled.
func (s *Service) partFinal(cand *partCandidate, mode string, score float64, draft string, cost float64) bool {
	if !s.partAllowed(cand) {
		return false
	}
	if !s.stillCurrent(cand) {
		s.logParticipation(cand, mode, partCancelled, "conversation_moved", score, draft, cost)
		return false
	}
	return true
}

// stillCurrent is the final check: nothing was said in the channel since the candidate, nobody called the bot
// since (a call counts as said before its mute restore, and sets the after-call pause after it), and the
// channel's entry is the one the candidate was made in (a dropped and made-again entry counts versions anew).
func (s *Service) stillCurrent(cand *partCandidate) bool {
	c := s.lockPartChannel(cand.key)
	defer s.part.mu.Unlock()
	return c.gen == cand.gen && c.version == cand.version && c.lastCall.Equal(cand.lastCall)
}
