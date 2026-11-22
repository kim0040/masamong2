package application

// Whether a Discord room stores its messages follows one rule: memory is on by default, and turning it off
// is explicit and wins. A room stores unless its own entry or, for a thread, its parent channel's entry says
// capture: false. A thread's own capture: true never turns capture back on under a parent that turned it off,
// and a thread the application cannot place (a lookup failed) is not stored in a server that has a channel
// with capture off: it may be one of that channel's threads.

// ChannelPlace is what a connector learned about a server channel before it stores a message from it.
type ChannelPlace struct {
	// Known is whether the connector learned if the channel is a thread (its lookup can fail).
	Known bool
	// Parent is the parent channel when the channel is a thread. A private thread is never registered with
	// the application (it is a room of its own), but its parent's capture off still applies to it.
	Parent string
}

// CapturesIn reports whether a message in a server channel may be stored as memory now: the instance stores
// and neither the channel nor its parent turns capture off.
func (s *Service) CapturesIn(guildID, channelID string, place ChannelPlace) bool {
	if guildID == "" || !s.Doc().Memory.Capture {
		return false
	}
	if !s.captureOffInGuild(guildID) {
		return true // nothing in this server is off, so no room or parent can be
	}
	parent := place.Parent
	if parent == "" {
		parent = s.threadParent(channelID)
	}
	if parent == "" && !place.Known {
		// the connector could not tell whether this is a thread and the live table has no row: a stored row
		// may still name the parent; without one it may be a thread of a channel turned off
		if db, err := s.adminDB(); err == nil {
			parent = storedThreadParent(db, guildID, channelID)
		}
		if parent == "" {
			return false
		}
	}
	return s.captureAllowed(guildID, channelID, parent)
}

// captureAllowed is the rule for a room whose parent is known ("" for a channel).
func (s *Service) captureAllowed(guildID, channelID, parentID string) bool {
	if guildID == "" || !s.ownChannelPolicy(guildID, channelID).Capture {
		return false
	}
	return parentID == "" || parentID == channelID || s.ownChannelPolicy(guildID, parentID).Capture
}

// captureOffInGuild reports whether some channel of the server turns capture off. Without one, every room
// of the server stores, whatever its parent. It reads the entries as parsed (parse settles capture), so it
// costs no normalizing on the message path.
func (s *Service) captureOffInGuild(guildID string) bool {
	for _, p := range s.Doc().Guilds[guildID].Channels {
		if !p.Capture {
			return true
		}
	}
	return false
}

// roomCapturesNow is roomCaptures on the service's own store.
func (s *Service) roomCapturesNow(guildID, channelID string) bool {
	db, err := s.adminDB()
	if err != nil {
		return false
	}
	return s.roomCaptures(db, guildID, channelID)
}
