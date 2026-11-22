package application

import (
	"context"
	"database/sql"
	"errors"
)

// Whether a Discord room stores its messages follows one rule: memory is on by default, and turning it off
// is explicit and wins. A room stores unless its own entry or, for a thread, its parent channel's entry says
// capture: false. A thread's own capture: true never turns capture back on under a parent that turned it off,
// and a thread the application cannot place (a lookup failed) is not stored in a server that has a channel
// with capture off: it may be one of that channel's threads.

func init() {
	// thread_parents keeps the parent of every thread the connector has seen, for as long as the thread's
	// stored messages may be read again: a private thread has no channel_threads row, and retention removes
	// the row of a closed one. IDs only, no member content.
	featureDDL = append(featureDDL, `CREATE TABLE IF NOT EXISTS thread_parents (
		thread_id TEXT PRIMARY KEY,
		guild_id TEXT NOT NULL,
		parent_id TEXT NOT NULL
	)`)
}

// NoteThreadParent stores a thread's parent channel for good (thread_parents), for public and private
// threads alike. The connector calls it the first time it learns a thread, before it stores a message from
// it, so room selection and a catch-up still find the parent's capture off after the connector forgot the
// thread (storedThreadParent). A thread's parent never changes: a second note keeps the first.
func (s *Service) NoteThreadParent(ctx context.Context, guildID, threadID, parentID string) error {
	if guildID == "" || threadID == "" || parentID == "" {
		return errors.New("a thread's server, ID and parent are all needed")
	}
	if s.Store == nil || !s.Store.IsSQLite() {
		return nil // nothing is stored without the SQLite store (featureDDL), so no parent is needed
	}
	db, err := s.adminDB()
	if err != nil {
		return err
	}
	return db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx, `INSERT INTO thread_parents(thread_id,guild_id,parent_id) VALUES(?,?,?)
			ON CONFLICT(thread_id) DO NOTHING`, threadID, guildID, parentID)
		return err
	})
}

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
