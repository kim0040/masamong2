package application

import (
	"database/sql"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"sort"
	"sync"
	"time"

	"masamong/internal/config"
)

// Banter (docs/plan-memory-conversation.md §5.7.3, experimental, off by default): when someone talks
// about the bot in fun ("마사몽 또 고장났네 ㅋㅋ") — the name-call classifier hears "about the bot" in a
// playful tone — the bot may answer back with one emoji or one light line. Channel setting
// conversation.banter: off (default) | shadow (record what it would have done) | on.
//
// Guard rails: never when the tone is not playful (a real complaint gets silence), at most
// banterLinesPerHour lines and banterEmojiPerHour emoji per channel, only an emoji when the room is
// fast, and after a 👎 on a banter reply no lines in that channel for banterMute. The words of the line
// come from the pack's banter rules: tease yourself or say "you're no better", never a person's looks,
// family, health or money.
const (
	banterLinesPerHour = 2
	banterEmojiPerHour = 4
	banterFastPerMin   = 6
	banterMute         = 2 * time.Hour
)

type banterChannel struct {
	lines, emoji []time.Time
	mutedUntil   time.Time
}

type banterTurn struct {
	guild, channel string
	at             time.Time
}

type banterState struct {
	mu          sync.Mutex
	chans       map[string]*banterChannel
	loaded      map[string]bool
	turns       map[string]banterTurn // recent banter turn → channel, for a later 👎 mute
	turnsLoaded bool
}

func init() {
	featureDDL = append(featureDDL,
		`CREATE TABLE IF NOT EXISTS banter_channel_state (
			guild_id TEXT NOT NULL,
			channel_id TEXT NOT NULL,
			lines TEXT NOT NULL DEFAULT '[]',
			emoji TEXT NOT NULL DEFAULT '[]',
			muted_until TEXT NOT NULL DEFAULT '',
			PRIMARY KEY(guild_id, channel_id)
		)`,
		`CREATE TABLE IF NOT EXISTS banter_turns (
			turn_id TEXT PRIMARY KEY,
			guild_id TEXT NOT NULL,
			channel_id TEXT NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_banter_turns_created ON banter_turns(created_at)`,
	)
}

// ReactOnly is a turn the bot answers with a reaction only (an emoji on the message). Sent must be
// called by the gateway only after Discord accepted the reaction.
type ReactOnly struct {
	Emoji string
	sent  func()
}

func (r ReactOnly) Error() string { return "chat: answered with a reaction" }

func (r ReactOnly) Sent() {
	if r.sent != nil {
		r.sent()
	}
}

// banterStep picks the rung of the ladder for a playful mention: "line", "emoji" or "" (stay quiet).
func (s *Service) banterStep(guildID, channelID string) string {
	key := guildID + ":" + channelID
	now := s.Clock.Now()
	fast := 0
	for _, l := range s.liveLines(guildID, channelID) {
		if now.Sub(l.At) <= time.Minute {
			fast++
		}
	}
	b := &s.banter
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.chans == nil {
		b.chans = map[string]*banterChannel{}
	}
	c := b.chans[key]
	if c == nil {
		c = &banterChannel{}
		b.chans[key] = c
	}
	s.restoreBanterChannelLocked(guildID, channelID, key, c)
	c.lines, c.emoji = within(c.lines, now, time.Hour), within(c.emoji, now, time.Hour)
	switch {
	case fast < banterFastPerMin && now.After(c.mutedUntil) && len(c.lines) < banterLinesPerHour:
		return "line"
	case len(c.emoji) < banterEmojiPerHour:
		return "emoji"
	}
	return ""
}

// noteBanter counts a rung that was used.
func (s *Service) noteBanter(guildID, channelID, step, turnID string) {
	key := guildID + ":" + channelID
	b := &s.banter
	b.mu.Lock()
	defer b.mu.Unlock()
	c := b.chans[key]
	if c == nil {
		return
	}
	now := s.Clock.Now().UTC()
	if step == "line" {
		c.lines = append(within(c.lines, now, time.Hour), now)
		if turnID != "" {
			if b.turns == nil {
				b.turns = map[string]banterTurn{}
			}
			b.turns[turnID] = banterTurn{guild: guildID, channel: channelID, at: now}
			s.pruneBanterTurnsLocked(now)
		}
	} else if step == "emoji" {
		c.emoji = append(within(c.emoji, now, time.Hour), now)
	} else {
		return
	}
	s.persistBanterChannelLocked(guildID, channelID, c, now)
	if step == "line" && turnID != "" {
		s.persistBanterTurnLocked(turnID, guildID, channelID, now)
	}
}

// banterDisliked mutes lines in the channel of a banter reply someone gave a 👎.
func (s *Service) banterDisliked(turnID string) {
	b := &s.banter
	b.mu.Lock()
	defer b.mu.Unlock()
	s.loadBanterTurnsLocked()
	s.pruneBanterTurnsLocked(s.Clock.Now().UTC())
	entry, ok := b.turns[turnID]
	if !ok {
		return
	}
	key := entry.guild + ":" + entry.channel
	if b.chans == nil {
		b.chans = map[string]*banterChannel{}
	}
	c := b.chans[key]
	if c == nil {
		c = &banterChannel{}
		b.chans[key] = c
	}
	s.restoreBanterChannelLocked(entry.guild, entry.channel, key, c)
	now := s.Clock.Now().UTC()
	c.mutedUntil = now.Add(banterMute)
	s.persistBanterChannelLocked(entry.guild, entry.channel, c, now)
}

// restoreBanterChannelLocked loads one channel the first time it is checked. The caller holds banter.mu.
func (s *Service) restoreBanterChannelLocked(guildID, channelID, key string, c *banterChannel) {
	b := &s.banter
	if b.loaded == nil {
		b.loaded = map[string]bool{}
	}
	if b.loaded[key] || s.Store == nil {
		return
	}
	db, err := s.adminDB()
	if err != nil {
		return
	}
	var linesRaw, emojiRaw, muted string
	err = db.Reader().QueryRow(`SELECT lines,emoji,muted_until FROM banter_channel_state WHERE guild_id=? AND channel_id=?`, guildID, channelID).
		Scan(&linesRaw, &emojiRaw, &muted)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return
	}
	b.loaded[key] = true
	if err == nil {
		_ = json.Unmarshal([]byte(linesRaw), &c.lines)
		_ = json.Unmarshal([]byte(emojiRaw), &c.emoji)
		now := s.Clock.Now()
		oldLines, oldEmoji := len(c.lines), len(c.emoji)
		c.lines, c.emoji = within(c.lines, now, time.Hour), within(c.emoji, now, time.Hour)
		futureMuted := false
		if at, parseErr := time.Parse(time.RFC3339Nano, muted); parseErr == nil {
			// a mute lies in the future by design (now+banterMute); only one beyond that horizon
			// was written before the clock went back
			if at.After(now.Add(banterMute)) {
				futureMuted = true
			} else {
				c.mutedUntil = at
			}
		}
		if futureMuted || len(c.lines) != oldLines || len(c.emoji) != oldEmoji || !now.Before(c.mutedUntil) && !c.mutedUntil.IsZero() {
			s.persistBanterChannelLocked(guildID, channelID, c, now)
		}
	}
}

func (s *Service) persistBanterChannelLocked(guildID, channelID string, c *banterChannel, now time.Time) {
	db, err := s.adminDB()
	if err != nil {
		return
	}
	c.lines, c.emoji = within(c.lines, now, time.Hour), within(c.emoji, now, time.Hour)
	if len(c.lines) == 0 && len(c.emoji) == 0 && !now.Before(c.mutedUntil) {
		if _, err := db.DB.Exec(`DELETE FROM banter_channel_state WHERE guild_id=? AND channel_id=?`, guildID, channelID); err != nil {
			s.Log.Debug("banter state prune failed", "err", err.Error())
		}
		return
	}
	lines, _ := json.Marshal(c.lines)
	emoji, _ := json.Marshal(c.emoji)
	muted := ""
	if now.Before(c.mutedUntil) {
		muted = c.mutedUntil.UTC().Format(time.RFC3339Nano)
	}
	if _, err := db.DB.Exec(`INSERT INTO banter_channel_state(guild_id,channel_id,lines,emoji,muted_until)
		VALUES(?,?,?,?,?) ON CONFLICT(guild_id,channel_id) DO UPDATE SET lines=excluded.lines,emoji=excluded.emoji,muted_until=excluded.muted_until`,
		guildID, channelID, string(lines), string(emoji), muted); err != nil {
		s.Log.Debug("banter state write failed", "err", err.Error())
	}
}

func (s *Service) loadBanterTurnsLocked() {
	b := &s.banter
	if b.turnsLoaded || s.Store == nil {
		return
	}
	db, err := s.adminDB()
	if err != nil {
		return
	}
	now := s.Clock.Now().UTC()
	rows, err := db.Reader().Query(`SELECT turn_id,guild_id,channel_id,created_at FROM banter_turns WHERE created_at>=? AND created_at<=? ORDER BY created_at DESC LIMIT 200`,
		banterStamp(now.Add(-24*time.Hour)), banterStamp(now))
	if err != nil {
		return
	}
	if b.turns == nil {
		b.turns = map[string]banterTurn{}
	}
	for rows.Next() {
		var id, guildID, channelID, stamp string
		if rows.Scan(&id, &guildID, &channelID, &stamp) != nil {
			continue
		}
		at, parseErr := time.Parse(time.RFC3339Nano, stamp)
		if parseErr == nil {
			b.turns[id] = banterTurn{guild: guildID, channel: channelID, at: at}
		}
	}
	_ = rows.Close()
	_, _ = db.DB.Exec(`DELETE FROM banter_turns WHERE created_at<? OR created_at>?`, banterStamp(now.Add(-24*time.Hour)), banterStamp(now))
	b.turnsLoaded = true
	s.pruneBanterTurnsLocked(now)
}

func (s *Service) pruneBanterTurnsLocked(now time.Time) {
	b := &s.banter
	kept := make([]struct {
		id string
		at time.Time
	}, 0, len(b.turns))
	for id, entry := range b.turns {
		if entry.at.After(now) || now.Sub(entry.at) > 24*time.Hour {
			delete(b.turns, id)
			continue
		}
		kept = append(kept, struct {
			id string
			at time.Time
		}{id: id, at: entry.at})
	}
	if len(kept) <= 200 {
		return
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].at.Before(kept[j].at) })
	for _, old := range kept[:len(kept)-200] {
		delete(b.turns, old.id)
	}
}

func (s *Service) persistBanterTurnLocked(turnID, guildID, channelID string, now time.Time) {
	db, err := s.adminDB()
	if err != nil {
		return
	}
	stamp := banterStamp(now)
	if _, err := db.DB.Exec(`INSERT INTO banter_turns(turn_id,guild_id,channel_id,created_at) VALUES(?,?,?,?)
		ON CONFLICT(turn_id) DO UPDATE SET guild_id=excluded.guild_id,channel_id=excluded.channel_id,created_at=excluded.created_at`,
		turnID, guildID, channelID, stamp); err != nil {
		s.Log.Debug("banter turn write failed", "err", err.Error())
		return
	}
	cutoff := banterStamp(now.Add(-24 * time.Hour))
	if _, err := db.DB.Exec(`DELETE FROM banter_turns WHERE created_at<?`, cutoff); err != nil {
		s.Log.Debug("banter turn prune failed", "err", err.Error())
		return
	}
	if _, err := db.DB.Exec(`DELETE FROM banter_turns WHERE turn_id IN (SELECT turn_id FROM banter_turns ORDER BY created_at DESC LIMIT -1 OFFSET 200)`); err != nil {
		s.Log.Debug("banter turn cap failed", "err", err.Error())
	}
}

func banterStamp(at time.Time) string {
	return at.UTC().Format("2006-01-02T15:04:05.000000000Z")
}

func (s *Service) banterEmoji(pol config.ChannelPolicy, guildID string) string {
	list := s.voiceFor(guildID, pol).Pack.Conversation.BanterEmoji
	if len(list) == 0 {
		return "😏"
	}
	return list[rand.IntN(len(list))]
}

func within(ts []time.Time, now time.Time, d time.Duration) []time.Time {
	var out []time.Time
	for _, t := range ts {
		if !t.After(now) && now.Sub(t) < d {
			out = append(out, t)
		}
	}
	return out
}
