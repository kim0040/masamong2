package application

import (
	"context"
	"strings"

	"masamong/internal/features/participation"
	"masamong/internal/features/privacy"
	"masamong/internal/lang"
)

// What the participation decisions ask the store is read before s.part.mu is taken, and only memory is
// read and written under it, so a slow store holds up no channel's decisions (fix plan R03, code standard
// R20). What reading under the lock gave is kept: a message moves the room on before its reads
// (arriveObserved), and reads are made again when what they depend on changed before the lock was taken
// (lockObserved, lockCandidate).

// partRereads bounds how often a decision reads again because the configuration changed (or, for Observe,
// collected rows began waiting) before it took the lock; one that keeps changing gets the last read. Observe
// reads once more past it when rows began waiting during the last read, so they get this message's answer.
const partRereads = 3

// observeArrival is what Observe's first step leaves for the rest.
type observeArrival struct {
	version  int
	gen      uint64
	watched  bool           // collected rows were waiting for what happens next
	answered *partCandidate // the waiting candidate this message answered, taken off the wait
	reason   string         // why it is cancelled (answerCandidate)
}

// arriveObserved is Observe's first step, under s.part.mu and before anything is read for the message: the
// message counts as said, so a draft in flight fails its final check (stillCurrent), and a waiting
// candidate it answers leaves the wait, so it is not judged while the reads run (Sol review of E8, finding
// 3). Observe logs that candidate's cancellation unless a quiet cue drops it; the bot's own message, which
// goes no further, logs it here. The entry's mute is restored later (lockObserved).
func (s *Service) arriveObserved(key string, m ObservedMessage) observeArrival {
	s.part.mu.Lock()
	defer s.part.mu.Unlock()
	c := s.partChannel(key)
	c.version++
	c.recent = append(c.recent, partMsg{id: m.MessageID, author: m.AuthorID, name: m.AuthorName, text: clip(m.Content, 400), at: m.At,
		bot: m.FromBot, call: m.BotCall == "mention"})
	if len(c.recent) > 20 {
		c.recent = c.recent[len(c.recent)-20:]
	}
	a := observeArrival{version: c.version, gen: c.gen, watched: len(c.watches) > 0}
	if p := c.pending; p != nil {
		if reason := answerCandidate(p, c.recent[len(c.recent)-1]); reason != "" {
			p.timer.Stop()
			c.pending = nil
			a.answered, a.reason = p, reason
		}
	}
	if m.FromBot {
		s.logAnswered(a.answered, a.reason, s.partWork())
		a.answered = nil
	}
	return a
}

// answerCandidate applies to a candidate waiting before it what one later message did: someone else or the
// bot speaking means people are handling it (the cancellation reason is returned); the same person going on
// is noted on a moment of collect mode.
func answerCandidate(p *partCandidate, l partMsg) string {
	if l.bot || l.author != p.msg.author {
		reason := "people_answered"
		if l.bot {
			reason = "bot_answered"
		} else if p.collect {
			p.next = "answered"
			if l.call {
				p.next = "called_bot"
			}
		}
		return reason
	}
	if p.collect && p.next == "" {
		p.next = "continued"
		if l.call {
			p.next = "called_bot"
		}
	}
	return ""
}

// waitingBefore is whether c's waiting candidate came before this message; one made by a later message,
// while this one's reads ran, does not filter it (answeredSince settles the two).
func (a observeArrival) waitingBefore(c *partChannel) bool {
	return c.pending != nil && (c.gen != a.gen || c.pending.version <= a.version)
}

// answeredSince applies to a candidate made after later messages were observed (while its own message's
// reads ran) what they would have done to it waiting (answerCandidate), and returns the cancellation reason,
// "" when it may wait. An entry made again meanwhile no longer holds its message: the conversation moved on.
func answeredSince(c *partChannel, a observeArrival, cand *partCandidate) string {
	if c.gen != a.gen {
		return "conversation_moved"
	}
	later := c.recent
	if n := c.version - a.version; n < len(later) {
		later = later[len(later)-n:]
	}
	for _, l := range later {
		if reason := answerCandidate(cand, l); reason != "" {
			return reason
		}
	}
	return ""
}

// logAnswered logs the cancellation of a candidate the room moved past (nil: none). The caller holds
// s.part.mu, which the tests' startWork needs.
func (s *Service) logAnswered(p *partCandidate, reason string, startWork func(string, func(context.Context))) {
	if p == nil {
		return
	}
	startWork("participation cancellation", func(_ context.Context) {
		s.logParticipation(p, s.participationModeOf(p.guild, p.channel), partCancelled, reason, 0, "", 0)
	})
}

// partWork starts the work Observe hands off: s.ownedGo, or the tests' queue (the caller holds s.part.mu).
func (s *Service) partWork() func(string, func(context.Context)) {
	if s.part.startWork != nil {
		return s.part.startWork
	}
	return s.ownedGo
}

// lockPartChannel takes s.part.mu and returns key's entry; the caller unlocks. An entry whose mute was not
// read yet reads it from the decision log before the lock is taken, so a slow store holds up no other
// channel's decisions (fix plan R03). A mute set in memory meanwhile is kept when it ends later.
func (s *Service) lockPartChannel(key string) *partChannel {
	s.part.mu.Lock()
	c := s.partChannel(key)
	if c.backoffLoaded || s.Store == nil {
		return c
	}
	s.part.mu.Unlock()
	guildID, channelID, _ := strings.Cut(key, ":")
	until, ok := s.restorePartMute(guildID, channelID)
	s.part.mu.Lock()
	c = s.partChannel(key) // dropped and made again meanwhile, it is a new entry
	if ok && !c.backoffLoaded {
		c.backoffLoaded = true
		if until.After(c.mutedUntil) {
			c.mutedUntil = until
		}
	}
	return c
}

// observeRead is what Observe asks the store and what it decides by, read before s.part.mu is taken so
// a slow store holds up no channel's decisions (fix plan R03). Each read is made only when Observe would
// make it, in the same order; only a candidate already waiting, which Observe checks first, makes one
// needless.
type observeRead struct {
	mode     string                // the channel's participation mode
	pack     *lang.Pack            // the channel's language pack (its quiet and chime-off cues)
	counted  *bool                 // whether the author may be counted for the collected rows waiting (nil: not read)
	nameSaid bool                  // a name said in collect mode that this message records (not nameCallGate)
	nameSkip string                // collectSkip for it ("" = record it)
	sig      participation.Signals // what the message asks of the room (empty: nothing to take up)
	filter   string                // the first filter reason the store gives ("" = none)
}

func (s *Service) observeReads(m ObservedMessage) observeRead {
	r := observeRead{mode: s.participationModeOf(m.GuildID, m.ChannelID)}
	if m.FromBot || r.mode == "off" {
		return r
	}
	pol := s.channelPolicy(m.GuildID, m.ChannelID)
	r.pack = s.voiceFor(m.GuildID, pol).Pack
	if r.mode == "collect" && m.BotCall == "name" && !(pol.Respond == "mention" && pol.NameCall == "addressed") {
		r.nameSaid = true
		r.nameSkip = s.collectSkip(m.GuildID, m.ChannelID, m.AuthorID)
	}
	if m.Addressed || m.ToSomeone {
		return r
	}
	pack := lang.For(r.pack.Tag, s.Doc().I18n.DefaultLocale)
	r.sig = participation.Read(m.Content, m.At, s.instanceZone(), pack.Time)
	if r.sig.Kind() == "" {
		return r
	}
	switch {
	case s.BlockedIn(m.GuildID, m.AuthorID):
		r.filter = "blocked" // a server's own block list counts as much as the instance's
	case !s.ConsentAllows(m.AuthorID, privacy.Participation):
		r.filter = "opted_out"
	case s.Paused(m.GuildID, m.ChannelID):
		r.filter = "paused"
	case r.mode == "collect" && !s.captureOn(m.GuildID, m.ChannelID):
		r.filter = "capture_off" // collect only records where the channel's memory capture is on
	case r.mode == "collect" && !s.MemoryAllowed(m.AuthorID):
		r.filter = "opted_out"
	}
	return r
}

// lockObserved reads what Observe asks the store (observeReads, and whether the author may be counted when
// collected rows wait), then takes s.part.mu and returns the message's entry; the caller unlocks. When the
// configuration changed while the reads ran (the new entry's mute restore included) or collected rows began
// waiting, they are made again, so the decision uses what reading under the lock gave (Sol review of E8,
// findings 4 and 5).
func (s *Service) lockObserved(key string, m ObservedMessage, watched bool) (*partChannel, observeRead) {
	for try := 1; ; try++ {
		cfg := s.cfg.Load()
		r := s.observeReads(m)
		if watched {
			ok := s.authorCounted(m)
			r.counted = &ok
		}
		c := s.lockPartChannel(key)
		watched = len(c.watches) > 0
		if (try >= partRereads || s.cfg.Load() == cfg) && (r.counted != nil || !watched) {
			return c, r
		}
		s.part.mu.Unlock()
	}
}

// candidateRead is what considerCandidate asks the store when a candidate's wait ends.
type candidateRead struct {
	gone       bool   // the person or the channel no longer allows it (partAllowed)
	collectWhy string // why collect mode records nothing for it now (collectSkip)
	mode       string // the channel's participation mode
}

// lockCandidate reads what considerCandidate asks the store, all of which may have changed during the wait,
// then takes s.part.mu and returns the candidate's entry; the caller unlocks. The reads are made again when
// the configuration changed before the lock was taken, so the mode is the one current under the lock while
// its thread lookup no longer waits under it (Sol review of E8, finding 7).
func (s *Service) lockCandidate(cand *partCandidate) (*partChannel, candidateRead) {
	for try := 1; ; try++ {
		cfg := s.cfg.Load()
		rd := candidateRead{gone: !s.partAllowed(cand), collectWhy: s.collectSkip(cand.guild, cand.channel, cand.msg.author),
			mode: s.participationModeOf(cand.guild, cand.channel)}
		c := s.lockPartChannel(cand.key)
		if try == partRereads || s.cfg.Load() == cfg {
			return c, rd
		}
		s.part.mu.Unlock()
	}
}
