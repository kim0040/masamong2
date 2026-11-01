package application

import (
	"context"
	"database/sql"
	"strings"
	"time"
	"unicode"

	"masamong/internal/chat"
	"masamong/internal/features/privacy"
	"masamong/internal/lang"
)

// Backing off (docs/plan-memory-conversation.md §5.7.3, experimental with the participation feature): the
// bot that chimes in on its own steps back when people show it is not wanted, without anyone opening the
// console.
//
//   - "조용히 해" (the pack's quiet cues, the whole message) within partEngageWindow of a remark of its own,
//     or a 👎 on one: no remarks in that channel for partMute;
//   - right after someone called the bot (partAfterCall) it does not chime in: it was just talking;
//   - remarks nobody reacts to (no reply to it, no reaction, nobody calling the bot within
//     partEngageWindow) raise that kind's threshold from the third in a row, by partIgnoredStep each up to
//     partIgnoredMax; one that people take up resets it;
//   - "끼어들기 꺼" (the pack's chime-off cues) is the speaker's own opt-out of chiming in (consent scope
//     participation, as in the privacy menu).
//
// Every step back is logged (conversation_log, kind backoff, body-free).
const (
	partMute         = 2 * time.Hour
	partAfterCall    = 5 * time.Minute
	partEngageWindow = 10 * time.Minute
	partIgnoredStep  = 0.1
	partIgnoredMax   = 0.2
	partSpokesKept   = 8
)

// restorePartMute derives the current cooldown from the durable decision log. Both events that start
// this mute (a quiet cue and a dislike) are already recorded there without message text.
func (s *Service) restorePartMute(guildID, channelID string, c *partChannel) bool {
	db, err := s.adminDB()
	if err != nil {
		return false
	}
	var stamp string
	err = db.Reader().QueryRow(`SELECT created_at FROM conversation_log WHERE guild_id=? AND channel_id=? AND kind=?
		AND decision IN ('quiet_cue','disliked') ORDER BY rowid DESC LIMIT 1`, guildID, channelID, convBackoff).Scan(&stamp)
	if err == sql.ErrNoRows {
		return true
	}
	if err != nil {
		return false
	}
	if at, parseErr := time.Parse(time.RFC3339Nano, stamp); parseErr == nil {
		c.mutedUntil = at.Add(partMute)
	}
	return true
}

// partSpoke is one remark the bot made on its own in a channel.
type partSpoke struct {
	msgID, turnID, kind string
	at                  time.Time
	engaged, settled    bool
}

// ChannelReactor is implemented by a gateway that can put a reaction on a person's message.
type ChannelReactor interface {
	React(ctx context.Context, channelID, messageID, emoji string) error
}

// cueOf normalizes a whole message for a cue match: lower case, no surrounding punctuation or "!".
func cueOf(text string) string {
	t := strings.ToLower(strings.TrimSpace(text))
	t = strings.TrimLeft(t, "!/")
	return strings.TrimRightFunc(t, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) || unicode.IsSymbol(r) })
}

func cueIn(t string, lists ...[]string) bool {
	if t == "" {
		return false
	}
	for _, l := range lists {
		for _, c := range l {
			if cueOf(c) == t {
				return true
			}
		}
	}
	return false
}

// backoffObserve is Observe's part (the participation lock held): calls, replies to a remark, and cues.
// It returns a reaction to put on the message ("" = none) and the step back to log.
func (s *Service) backoffObserve(c *partChannel, m ObservedMessage, pack *lang.Pack) (react, logged string) {
	if m.FromBot {
		return "", ""
	}
	if m.BotCall != "" {
		c.lastCall = m.At
		for i := range c.spokes {
			if !c.spokes[i].settled && m.At.Sub(c.spokes[i].at) <= partEngageWindow {
				c.spokes[i].engaged = true
			}
		}
	}
	if m.ReplyTo != "" {
		for i := range c.spokes {
			if c.spokes[i].msgID == m.ReplyTo {
				c.spokes[i].engaged = true
			}
		}
	}
	if m.ToSomeone {
		return "", ""
	}
	t := cueOf(m.Content)
	other := lang.For(chat.DetectLanguage(m.Content), pack.Tag)
	if cueIn(t, pack.Features.ChimeOffCues, other.Features.ChimeOffCues) {
		return "✅", "chime_off"
	}
	if !cueIn(t, pack.Features.QuietCues, other.Features.QuietCues) {
		return "", ""
	}
	recent := false
	for _, sp := range c.spokes {
		recent = recent || m.At.Sub(sp.at) <= partEngageWindow
	}
	if !recent {
		return "", "" // said to the people here, not to a bot that just chimed in
	}
	c.mutedUntil = m.At.Add(partMute)
	if c.pending != nil {
		c.pending.timer.Stop()
		c.pending = nil
	}
	return "🤐", "quiet_cue"
}

// backoffAfter carries out what backoffObserve decided, outside the lock.
func (s *Service) backoffAfter(m ObservedMessage, react, logged string) {
	if logged == "" {
		return
	}
	if logged == "chime_off" && m.AuthorID != "" {
		if _, err := s.decideConsent(m.AuthorID, privacy.Participation, privacy.Withdrawn, "chat"); err != nil {
			return
		}
		s.afterWithdraw(context.Background(), m.AuthorID, privacy.Participation)
	}
	s.logConversation(convDecision{Guild: m.GuildID, Channel: m.ChannelID, Message: m.MessageID, Author: m.AuthorID, Kind: convBackoff, Decision: logged})
	if r, ok := s.Discord.(ChannelReactor); ok && react != "" && s.Discord != nil && s.Discord.Connected() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = r.React(ctx, m.ChannelID, m.MessageID, react)
	}
}

// backoffBlocked is why the channel takes no remark now ("" = it may): muted, or the bot was just called.
func backoffBlocked(c *partChannel, now time.Time) string {
	switch {
	case now.Before(c.mutedUntil):
		return "muted"
	case !c.lastCall.IsZero() && now.Sub(c.lastCall) < partAfterCall:
		return "after_call"
	}
	return ""
}

// ignoredBump settles the remarks whose window ended and returns how much higher kind's threshold is
// (the participation lock held).
func (s *Service) ignoredBump(c *partChannel, kind string, now time.Time) float64 {
	if c.ignored == nil {
		c.ignored = map[string]int{}
	}
	for i := range c.spokes {
		sp := &c.spokes[i]
		if sp.settled {
			continue
		}
		switch {
		case sp.engaged:
			sp.settled = true
			c.ignored[sp.kind] = 0
		case now.Sub(sp.at) > partEngageWindow:
			sp.settled = true
			c.ignored[sp.kind]++
		}
	}
	n := c.ignored[kind] - 2
	if n <= 0 {
		return 0
	}
	return min(partIgnoredMax, float64(n)*partIgnoredStep)
}

// noteSpoke records a remark the bot posted (for the reactions, replies and 👎 that may follow).
func (s *Service) noteSpoke(cand *partCandidate, msgID, turnID string) {
	s.part.mu.Lock()
	defer s.part.mu.Unlock()
	c := s.partChannel(cand.key)
	now := s.Clock.Now().UTC()
	c.spokes = append(c.spokes, partSpoke{msgID: msgID, turnID: turnID, kind: cand.kind, at: now})
	if len(c.spokes) > partSpokesKept {
		c.spokes = c.spokes[len(c.spokes)-partSpokesKept:]
	}
	if turnID != "" {
		s.persistPartSpokeTurnLocked(turnID, cand.guild, cand.channel, now)
	}
}

// partFeedback is a rating of one of the bot's replies: a remark of its own that got 👎 mutes the channel,
// any rating counts as people taking it up.
func (s *Service) partFeedback(turnID string, good bool) {
	if turnID == "" {
		return
	}
	var guild, channel string
	s.part.mu.Lock()
	for key, c := range s.part.chans {
		for i := range c.spokes {
			if c.spokes[i].turnID != turnID {
				continue
			}
			c.spokes[i].engaged = true
			if !good {
				c.mutedUntil = s.Clock.Now().Add(partMute)
				guild, channel, _ = strings.Cut(key, ":")
			}
		}
	}
	s.part.mu.Unlock()
	if !good && channel == "" {
		if restoredGuild, restoredChannel, ok := s.persistedPartSpoke(turnID); ok {
			guild, channel = restoredGuild, restoredChannel
			s.part.mu.Lock()
			c := s.partChannel(guild + ":" + channel)
			c.mutedUntil = s.Clock.Now().Add(partMute)
			s.part.mu.Unlock()
		}
	}
	if channel != "" {
		s.logConversation(convDecision{Guild: guild, Channel: channel, Kind: convBackoff, Decision: "disliked"})
	}
}

func (s *Service) persistPartSpokeTurnLocked(turnID, guildID, channelID string, now time.Time) {
	db, err := s.adminDB()
	if err != nil {
		return
	}
	stamp := now.UTC().Format("2006-01-02T15:04:05.000000000Z")
	if _, err := db.DB.Exec(`INSERT INTO participation_spoke_turns(turn_id,guild_id,channel_id,created_at) VALUES(?,?,?,?)
		ON CONFLICT(turn_id) DO UPDATE SET guild_id=excluded.guild_id,channel_id=excluded.channel_id,created_at=excluded.created_at`,
		turnID, guildID, channelID, stamp); err != nil {
		s.Log.Debug("participation spoke turn write failed", "err", err.Error())
		return
	}
	cutoff := s.Clock.Now().UTC().Add(-24 * time.Hour).Format("2006-01-02T15:04:05.000000000Z")
	if _, err := db.DB.Exec(`DELETE FROM participation_spoke_turns WHERE created_at<?`, cutoff); err != nil {
		s.Log.Debug("participation spoke turn prune failed", "err", err.Error())
		return
	}
	if _, err := db.DB.Exec(`DELETE FROM participation_spoke_turns WHERE turn_id IN (
		SELECT turn_id FROM participation_spoke_turns ORDER BY rowid DESC LIMIT -1 OFFSET 200)`); err != nil {
		s.Log.Debug("participation spoke turn cap failed", "err", err.Error())
	}
}

func (s *Service) persistedPartSpoke(turnID string) (guildID, channelID string, ok bool) {
	db, err := s.adminDB()
	if err != nil {
		return "", "", false
	}
	cutoff := s.Clock.Now().UTC().Add(-24 * time.Hour).Format("2006-01-02T15:04:05.000000000Z")
	err = db.Reader().QueryRow(`SELECT guild_id,channel_id FROM participation_spoke_turns WHERE turn_id=? AND created_at>=?`, turnID, cutoff).
		Scan(&guildID, &channelID)
	return guildID, channelID, err == nil
}
