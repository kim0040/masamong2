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
	"masamong/internal/storage"
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
	// writing marks the channels whose row is being stored now, dirty those changed since that write
	// took its copy (writeBanterChannel)
	writing, dirty map[string]bool
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
	c, rewrite := s.lockBanterChannel(guildID, channelID, key)
	c.lines, c.emoji = within(c.lines, now, time.Hour), within(c.emoji, now, time.Hour)
	step := ""
	switch {
	case fast < banterFastPerMin && now.After(c.mutedUntil) && len(c.lines) < banterLinesPerHour:
		step = "line"
	case len(c.emoji) < banterEmojiPerHour:
		step = "emoji"
	}
	b.mu.Unlock()
	if rewrite {
		s.writeBanterChannel(guildID, channelID, key)
	}
	return step
}

// noteBanter counts a rung that was used.
func (s *Service) noteBanter(guildID, channelID, step, turnID string) {
	key := guildID + ":" + channelID
	b := &s.banter
	b.mu.Lock()
	c := b.chans[key]
	if c == nil {
		b.mu.Unlock()
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
		b.mu.Unlock()
		return
	}
	b.mu.Unlock()
	s.writeBanterChannel(guildID, channelID, key)
	if step == "line" && turnID != "" {
		s.persistBanterTurn(turnID, guildID, channelID, now)
	}
}

// banterDisliked mutes lines in the channel of a banter reply someone gave a 👎.
func (s *Service) banterDisliked(turnID string) {
	s.loadBanterTurns()
	b := &s.banter
	b.mu.Lock()
	s.pruneBanterTurnsLocked(s.Clock.Now().UTC())
	entry, ok := b.turns[turnID]
	b.mu.Unlock()
	if !ok {
		return
	}
	key := entry.guild + ":" + entry.channel
	c, _ := s.lockBanterChannel(entry.guild, entry.channel, key) // the mute below is written either way
	c.mutedUntil = s.Clock.Now().UTC().Add(banterMute)
	b.mu.Unlock()
	s.writeBanterChannel(entry.guild, entry.channel, key)
}

// lockBanterChannel takes banter.mu and returns the channel's entry; the caller unlocks. The first time
// a channel is checked its stored row is read before the lock is taken, so a slow store holds up no other
// channel's banter (fix plan R03). rewrite says the row held expired state, to be written again once the
// lock is released.
func (s *Service) lockBanterChannel(guildID, channelID, key string) (c *banterChannel, rewrite bool) {
	b := &s.banter
	b.mu.Lock()
	if b.loaded[key] || s.Store == nil {
		return b.channelLocked(key), false
	}
	b.mu.Unlock()
	row, ok := s.readBanterChannel(guildID, channelID)
	b.mu.Lock()
	c = b.channelLocked(key)
	if ok && !b.loaded[key] { // another call may have read it meanwhile
		if b.loaded == nil {
			b.loaded = map[string]bool{}
		}
		b.loaded[key] = true
		rewrite = s.restoreBanterChannel(c, row)
	}
	return c, rewrite
}

// channelLocked returns key's entry, made when missing. The caller holds mu.
func (b *banterState) channelLocked(key string) *banterChannel {
	if b.chans == nil {
		b.chans = map[string]*banterChannel{}
	}
	c := b.chans[key]
	if c == nil {
		c = &banterChannel{}
		b.chans[key] = c
	}
	return c
}

// storedBanter is a channel's banter_channel_state row; found is false when there is none.
type storedBanter struct {
	lines, emoji, muted string
	found               bool
}

// readBanterChannel reads a channel's stored row; ok is false when the store could not be read (the
// channel is read again next time).
func (s *Service) readBanterChannel(guildID, channelID string) (row storedBanter, ok bool) {
	db, err := s.adminDB()
	if err != nil {
		return row, false
	}
	err = db.Reader().QueryRow(`SELECT lines,emoji,muted_until FROM banter_channel_state WHERE guild_id=? AND channel_id=?`, guildID, channelID).
		Scan(&row.lines, &row.emoji, &row.muted)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return row, false
	}
	row.found = err == nil
	return row, true
}

// restoreBanterChannel puts a channel's stored row into its entry and reports whether the row held
// expired state to clean. The caller holds banter.mu.
func (s *Service) restoreBanterChannel(c *banterChannel, row storedBanter) (rewrite bool) {
	if !row.found {
		return false
	}
	_ = json.Unmarshal([]byte(row.lines), &c.lines)
	_ = json.Unmarshal([]byte(row.emoji), &c.emoji)
	now := s.Clock.Now()
	oldLines, oldEmoji := len(c.lines), len(c.emoji)
	c.lines, c.emoji = within(c.lines, now, time.Hour), within(c.emoji, now, time.Hour)
	futureMuted := false
	if at, parseErr := time.Parse(time.RFC3339Nano, row.muted); parseErr == nil {
		// a mute lies in the future by design (now+banterMute); only one beyond that horizon
		// was written before the clock went back
		if at.After(now.Add(banterMute)) {
			futureMuted = true
		} else {
			c.mutedUntil = at
		}
	}
	return futureMuted || len(c.lines) != oldLines || len(c.emoji) != oldEmoji || !now.Before(c.mutedUntil) && !c.mutedUntil.IsZero()
}

// writeBanterChannel stores a channel's current state, without banter.mu held while the store works.
// One write per channel runs at a time and it writes the latest state: a change made while it runs is
// written again right after, so the stored row never ends older than memory.
func (s *Service) writeBanterChannel(guildID, channelID, key string) {
	db, err := s.adminDB()
	if err != nil {
		return
	}
	b := &s.banter
	b.mu.Lock()
	if b.writing[key] {
		b.dirty[key] = true // the running write stores it once more
		b.mu.Unlock()
		return
	}
	if b.writing == nil {
		b.writing, b.dirty = map[string]bool{}, map[string]bool{}
	}
	b.writing[key] = true
	for {
		delete(b.dirty, key)
		c := b.channelLocked(key)
		now := s.Clock.Now()
		c.lines, c.emoji = within(c.lines, now, time.Hour), within(c.emoji, now, time.Hour)
		keep := len(c.lines) > 0 || len(c.emoji) > 0 || now.Before(c.mutedUntil)
		lines, _ := json.Marshal(c.lines)
		emoji, _ := json.Marshal(c.emoji)
		muted := ""
		if now.Before(c.mutedUntil) {
			muted = c.mutedUntil.UTC().Format(time.RFC3339Nano)
		}
		b.mu.Unlock()
		s.storeBanterChannel(db, guildID, channelID, keep, string(lines), string(emoji), muted)
		b.mu.Lock()
		if !b.dirty[key] {
			break
		}
	}
	delete(b.writing, key)
	b.mu.Unlock()
}

// storeBanterChannel writes (or, with nothing to keep, deletes) a channel's row.
func (s *Service) storeBanterChannel(db *storage.Store, guildID, channelID string, keep bool, lines, emoji, muted string) {
	if !keep {
		if _, err := db.DB.Exec(`DELETE FROM banter_channel_state WHERE guild_id=? AND channel_id=?`, guildID, channelID); err != nil {
			s.Log.Debug("banter state prune failed", "err", err.Error())
		}
		return
	}
	if _, err := db.DB.Exec(`INSERT INTO banter_channel_state(guild_id,channel_id,lines,emoji,muted_until)
		VALUES(?,?,?,?,?) ON CONFLICT(guild_id,channel_id) DO UPDATE SET lines=excluded.lines,emoji=excluded.emoji,muted_until=excluded.muted_until`,
		guildID, channelID, lines, emoji, muted); err != nil {
		s.Log.Debug("banter state write failed", "err", err.Error())
	}
}

// banterTurnSlack is how far after the read began a stored banter turn may lie and still be kept: after a
// short clock rollback a later run reads it once the clock has caught up.
const banterTurnSlack = time.Minute

// loadBanterTurns reads the stored banter turns of the last day once, before banter.mu is taken. Rows dated
// more than banterTurnSlack after the read began were written before the clock went back and are deleted,
// except the turns noted while this reads: a turn is in memory before its row is written, so memory tells
// them apart.
func (s *Service) loadBanterTurns() {
	b := &s.banter
	b.mu.Lock()
	loaded := b.turnsLoaded || s.Store == nil
	b.mu.Unlock()
	if loaded {
		return
	}
	db, err := s.adminDB()
	if err != nil {
		return
	}
	now := s.Clock.Now().UTC()
	read, future, ok := readBanterTurns(db, now)
	if !ok {
		return
	}
	b.mu.Lock()
	if !b.turnsLoaded {
		if b.turns == nil {
			b.turns = map[string]banterTurn{}
		}
		for id, t := range read {
			if _, noted := b.turns[id]; !noted { // a turn noted meanwhile is the newer one
				b.turns[id] = t
			}
		}
		b.turnsLoaded = true
		s.pruneBanterTurnsLocked(s.Clock.Now().UTC()) // not now: a turn noted meanwhile is later than it
	}
	for id := range future {
		if _, noted := b.turns[id]; noted {
			delete(future, id)
		}
	}
	b.mu.Unlock()
	_, _ = db.DB.Exec(`DELETE FROM banter_turns WHERE created_at<?`, banterStamp(now.Add(-24*time.Hour))) // tidying: the read skips them
	for id, stamp := range future {
		_, _ = db.DB.Exec(`DELETE FROM banter_turns WHERE turn_id=? AND created_at=?`, id, stamp) // tidying: the read skips them
	}
}

// readBanterTurns reads the stored banter turns of the day before now, and the turn IDs (with their stamps)
// dated more than banterTurnSlack after it; ok is false when the store could not be read.
func readBanterTurns(db *storage.Store, now time.Time) (read map[string]banterTurn, future map[string]string, ok bool) {
	rows, err := db.Reader().Query(`SELECT turn_id,guild_id,channel_id,created_at FROM banter_turns WHERE created_at>=? AND created_at<=? ORDER BY created_at DESC LIMIT 200`,
		banterStamp(now.Add(-24*time.Hour)), banterStamp(now))
	if err != nil {
		return nil, nil, false
	}
	read = map[string]banterTurn{}
	for rows.Next() {
		var id, guildID, channelID, stamp string
		if rows.Scan(&id, &guildID, &channelID, &stamp) != nil {
			continue
		}
		at, parseErr := time.Parse(time.RFC3339Nano, stamp)
		if parseErr == nil {
			read[id] = banterTurn{guild: guildID, channel: channelID, at: at}
		}
	}
	_ = rows.Close()
	rows, err = db.Reader().Query(`SELECT turn_id,created_at FROM banter_turns WHERE created_at>?`, banterStamp(now.Add(banterTurnSlack)))
	if err != nil {
		return nil, nil, false
	}
	defer rows.Close()
	future = map[string]string{}
	for rows.Next() {
		var id, stamp string
		if rows.Scan(&id, &stamp) == nil {
			future[id] = stamp
		}
	}
	return read, future, true
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

// persistBanterTurn stores a banter turn for a 👎 after a restart (a day at most, 200 at most). The
// caller does not hold banter.mu.
func (s *Service) persistBanterTurn(turnID, guildID, channelID string, now time.Time) {
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
