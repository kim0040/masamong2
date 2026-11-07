package application

import (
	"sort"
	"strings"
	"sync"
	"time"

	"masamong/internal/conversation"
)

// The live window is the channel's last couple of hours as the gateway saw them: every message with who
// it replies to and whom it mentions, the bot's own replies included. It lives in memory only and is
// never written to disk, so it works in channels that do not store messages (capture off) the way a
// person in the room remembers the last few minutes. Attribution (internal/conversation) reads it to
// tell the caller's conversation from the other talk in the room; the stored messages and chat turns
// fill it in after a restart.
const (
	liveMaxLines  = 150
	liveMaxAge    = 3 * time.Hour
	liveTextRunes = 1500
	// A channel that has said nothing for a day keeps no window: lines older than liveMaxAge are of no
	// use to attribution, and stored messages and chat turns fill the window in again when it talks. The
	// count cap keeps a server that opens many short-lived channels or threads from holding a window
	// (up to liveMaxLines lines) for each of them for the life of the process.
	liveIdleEvict   = 24 * time.Hour
	liveMaxChannels = 256
)

type liveWindow struct {
	mu    sync.Mutex
	chans map[string][]conversation.Line
}

func liveKey(guildID, channelID string) string { return guildID + ":" + channelID }

// noteLive adds an observed message. A person who withdrew consent to being remembered keeps their place
// in the structure (it still tells who talks to whom) but not their words.
func (s *Service) noteLive(m ObservedMessage) {
	if m.ChannelID == "" || m.MessageID == "" {
		return
	}
	text := m.Content
	if !m.FromBot && !s.MemoryAllowed(m.AuthorID) {
		text = ""
	}
	l := conversation.Line{ID: m.MessageID, AuthorID: m.AuthorID, Author: m.AuthorName, Bot: m.FromBot, ReplyTo: m.ReplyTo,
		Mentions: append([]string(nil), m.Mentions...), MentionsOthers: m.MentionsOthers, At: m.At, Text: clip(text, liveTextRunes)}
	key := liveKey(m.GuildID, m.ChannelID)
	w := &s.live
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.chans == nil {
		w.chans = map[string][]conversation.Line{}
	}
	lines := w.chans[key]
	for i := range lines {
		if lines[i].ID == l.ID {
			lines[i] = l
			return
		}
	}
	lines = append(lines, l)
	cut := 0
	for cut < len(lines) && (len(lines)-cut > liveMaxLines || m.At.Sub(lines[cut].At) > liveMaxAge) {
		cut++
	}
	w.chans[key] = append([]conversation.Line(nil), lines[cut:]...)
}

// liveLines is a copy of a channel's live window, oldest first.
func (s *Service) liveLines(guildID, channelID string) []conversation.Line {
	w := &s.live
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]conversation.Line(nil), w.chans[liveKey(guildID, channelID)]...)
}

// liveEdit and liveDelete apply an author's edit or deletion, so a removed message never reaches a reply.
func (s *Service) liveEdit(guildID, channelID, messageID, content string) {
	unreached := s.liveUnreached(guildID, channelID)
	w := &s.live
	w.mu.Lock()
	defer w.mu.Unlock()
	s.liveBlankReplies(guildID, channelID, []string{messageID}, unreached)
	for _, key := range s.liveKeys(guildID, channelID) {
		for i, l := range w.chans[key] {
			if l.ID == messageID && l.Text != "" {
				w.chans[key][i].Text = clip(content, liveTextRunes)
			}
		}
	}
}

// liveHasWords reports whether the window holds these exact words for the message.
func (s *Service) liveHasWords(guildID, channelID, messageID, content string) bool {
	w := &s.live
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, l := range w.chans[liveKey(guildID, channelID)] {
		if l.ID == messageID && l.Text != "" && l.Text == content {
			return true
		}
	}
	return false
}

func (s *Service) liveDelete(guildID, channelID string, messageIDs []string) {
	unreached := s.liveUnreached(guildID, channelID)
	gone := map[string]bool{}
	for _, id := range messageIDs {
		gone[id] = true
	}
	w := &s.live
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.chans == nil {
		return
	}
	s.liveBlankReplies(guildID, channelID, messageIDs, unreached)
	for _, key := range s.liveKeys(guildID, channelID) {
		kept := w.chans[key][:0:0]
		for _, l := range w.chans[key] {
			if !gone[l.ID] {
				kept = append(kept, l)
			}
		}
		w.chans[key] = kept
	}
}

// liveUnreached lists the live windows of the guild whose replies cannot have read this channel: what can read a
// room is its own channel, the channel's thread family, and whoever has blocks attached (channelRooms; rooms are never
// shared otherwise). Which room a block holds is slow to read, and blanking a kept reply costs nothing, so a channel
// with blocks counts as reached. A window opened after this look is not listed, and so is reached. nil, everything is
// reached, when the channel is not known or is a deleted thread, whose family is no longer told. It reads the
// database, so it runs before the window's lock is taken.
func (s *Service) liveUnreached(guildID, channelID string) map[string]bool {
	if channelID == "" || s.threadDeleted(channelID) {
		return nil
	}
	w := &s.live
	w.mu.Lock()
	var channels []string
	for key := range w.chans {
		if other, ok := strings.CutPrefix(key, guildID+":"); ok && other != channelID {
			channels = append(channels, other)
		}
	}
	w.mu.Unlock()
	root := func(channel string) string {
		if parent := s.threadParent(channel); parent != "" {
			return parent
		}
		return channel
	}
	family := root(channelID)
	out := map[string]bool{}
	for _, other := range channels {
		// a direct message is its own room: no other channel reads it, and it reads no other
		if guildID != "" {
			parent := root(other)
			if parent == family || len(s.channelPolicy(guildID, parent).RAGView.Blocks) > 0 {
				continue
			}
		}
		out[liveKey(guildID, other)] = true
	}
	return out
}

// liveBlankReplies takes the words out of the bot's lines that may be built on messages about to change: every
// line posted since the earliest of them, and all of them when the window no longer holds one (it is older than
// the window). In the channel and the windows whose replies can read it (liveUnreached names the others), because a
// thread starts from its parent's turns. A reply that is still kept loses nothing (channelLines shows the stored
// answer instead), and the line stays: it still tells who talks to whom. Called with the lock held.
func (s *Service) liveBlankReplies(guildID, channelID string, messageIDs []string, unreached map[string]bool) {
	wanted := map[string]bool{}
	for _, id := range messageIDs {
		wanted[id] = true
	}
	var since time.Time
	found := 0
	for _, key := range s.liveKeys(guildID, channelID) {
		for _, l := range s.live.chans[key] {
			if wanted[l.ID] {
				found++
				if since.IsZero() || l.At.Before(since) {
					since = l.At
				}
			}
		}
	}
	if found < len(wanted) {
		since = time.Time{}
	}
	for key, lines := range s.live.chans {
		if !strings.HasPrefix(key, guildID+":") || unreached[key] {
			continue
		}
		for i := range lines {
			if lines[i].Bot && !wanted[lines[i].ID] && !lines[i].At.Before(since) {
				lines[i].Text = ""
			}
		}
	}
}

// liveForget drops a person's words from every live window (consent withdrawn), and the bot's lines, any of
// which may be built on them.
func (s *Service) liveForget(accountID string) {
	if accountID == "" {
		return
	}
	w := &s.live
	w.mu.Lock()
	defer w.mu.Unlock()
	for key, lines := range w.chans {
		for i := range lines {
			if lines[i].AuthorID == accountID || lines[i].Bot {
				w.chans[key][i].Text = ""
			}
		}
	}
}

// liveKeys is the window an edit or deletion applies to; an event without a channel (a bulk delete the
// gateway could not place) is looked for everywhere. Called with the lock held.
func (s *Service) liveKeys(guildID, channelID string) []string {
	if channelID != "" {
		return []string{liveKey(guildID, channelID)}
	}
	var keys []string
	for k := range s.live.chans {
		if guildID == "" || strings.HasPrefix(k, guildID+":") {
			keys = append(keys, k)
		}
	}
	return keys
}

// sweepLive drops the windows of channels that have been quiet for liveIdleEvict (or are empty), then the
// quietest ones beyond liveMaxChannels. The minute scheduler reaches it through sweepCollectNext.
func (s *Service) sweepLive(now time.Time) {
	w := &s.live
	w.mu.Lock()
	defer w.mu.Unlock()
	type quiet struct {
		key string
		at  time.Time
	}
	var kept []quiet
	for key, lines := range w.chans {
		if len(lines) == 0 || now.Sub(lines[len(lines)-1].At) > liveIdleEvict {
			delete(w.chans, key)
			continue
		}
		kept = append(kept, quiet{key, lines[len(lines)-1].At})
	}
	if len(kept) <= liveMaxChannels {
		return
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].at.Before(kept[j].at) })
	for _, q := range kept[:len(kept)-liveMaxChannels] {
		delete(w.chans, q.key)
	}
}
