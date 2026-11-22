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
