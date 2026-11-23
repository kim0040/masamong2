package application

import (
	"strings"

	"masamong/internal/features/participation"
	"masamong/internal/features/privacy"
	"masamong/internal/lang"
)

// What the participation decisions ask the store is read before s.part.mu is taken, and only memory is
// read and written under it, so a slow store holds up no channel's decisions (fix plan R03, code standard
// R20). What reading under the lock gave is kept: reads are made again when what they depend on changed
// before the lock was taken (lockCandidate).

// partRereads bounds how often a decision reads again because the configuration changed before it took the
// lock; one that keeps changing gets the last read.
const partRereads = 3

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
	if r.mode == "collect" && m.BotCall == "name" && !(pol.Respond == "mention" && pol.NameCall == "addressed") {
		r.nameSaid = true
		r.nameSkip = s.collectSkip(m.GuildID, m.ChannelID, m.AuthorID)
	}
	if m.Addressed || m.ToSomeone {
		return r
	}
	pack := lang.For(s.voiceFor(m.GuildID, pol).Pack.Tag, s.Doc().I18n.DefaultLocale)
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
