package application

import (
	"context"
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/memory/engine"
)

// Catching up on Discord messages posted while the bot was away (docs/wiki/보류-중인-결정 #7). Live
// capture only sees messages while connected; after a restart or a new gateway session the gap is read
// back from the Discord API, per channel that captures, from the newest stored message on — at most
// memory.backfill_days back. A channel never captured before is not read (that would be importing its
// history, which is the operator's decision). Caught-up messages are stored, never answered.

// HistoryMessage is one message read back from a channel's history.
type HistoryMessage struct {
	ID, AuthorID, AuthorName, Content string
	At                                time.Time
	Bot                               bool
}

// DiscordHistory is implemented by a gateway that can read channel history.
type DiscordHistory interface {
	// History returns up to limit messages posted after afterID (any order).
	History(ctx context.Context, channelID, afterID string, limit int) ([]HistoryMessage, error)
}

// backfillPages is how many 100-message pages one channel's catch-up may read (memory.backfill_max_messages).
func (s *Service) backfillPages() int {
	return max(1, (s.Doc().Memory.BackfillMax()+99)/100)
}

// BackfillChannel is one channel's catch-up result.
type BackfillChannel struct {
	GuildID   string `json:"guild_id"`
	ChannelID string `json:"channel_id"`
	Read      int    `json:"read"`
	Stored    int    `json:"stored"`
	Skipped   string `json:"skipped,omitempty"` // never_captured | no_history | error text
	Truncated bool   `json:"truncated,omitempty"`
}

// BackfillReport is one catch-up run.
type BackfillReport struct {
	At       string            `json:"at"`
	Trigger  string            `json:"trigger"` // connect | manual
	Days     int               `json:"days"`
	Channels []BackfillChannel `json:"channels"`
	Stored   int               `json:"stored"`
}

// discordEpoch is Discord's snowflake epoch (2015-01-01T00:00:00Z) in milliseconds.
const discordEpoch = 1420070400000

// snowflakeAt is the smallest message ID Discord could assign at t.
func snowflakeAt(t time.Time) string {
	ms := t.UnixMilli() - discordEpoch
	if ms < 0 {
		ms = 0
	}
	return strconv.FormatUint(uint64(ms)<<22, 10)
}

// idLess orders snowflakes as numbers (they are decimal strings without leading zeros).
func idLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

// laterID returns the larger of two snowflakes.
func laterID(a, b string) string {
	if idLess(a, b) {
		return b
	}
	return a
}

// capturingChannels are the configured channels whose messages are stored (instance and channel on).
func (s *Service) capturingChannels() [][2]string {
	doc := s.Doc()
	if !doc.Memory.Capture {
		return nil
	}
	var out [][2]string
	for g, gs := range doc.Guilds {
		for c := range gs.Channels {
			if s.channelPolicy(g, c).Capture {
				out = append(out, [2]string{g, c})
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i][0]+":"+out[i][1] < out[j][0]+":"+out[j][1] })
	return out
}

// Backfill catches up on the capturing channels (all of them, or the one given). trigger is "connect"
// or "manual".
func (s *Service) Backfill(ctx context.Context, trigger, guildID, channelID string) (BackfillReport, error) {
	doc := s.Doc()
	window := doc.Memory.Backfill()
	rep := BackfillReport{At: s.Clock.Now().UTC().Format(time.RFC3339), Trigger: trigger, Days: int(window / (24 * time.Hour)), Channels: []BackfillChannel{}}
	if window == 0 {
		return rep, errors.New("memory.backfill_days is off")
	}
	if !doc.Discord.MessageContent {
		return rep, errors.New("reading message text needs the message content intent (discord.message_content)")
	}
	h, ok := s.Discord.(DiscordHistory)
	if s.Discord == nil || !ok || !s.Discord.Connected() {
		return rep, errors.New("discord is not connected")
	}
	eng, err := s.MemoryEngine()
	if err != nil {
		return rep, err
	}
	targets := s.capturingChannels()
	if channelID != "" {
		targets = [][2]string{{guildID, channelID}}
	}
	cutoff := snowflakeAt(s.Clock.Now().Add(-window))
	for _, t := range targets {
		if ctx.Err() != nil {
			break
		}
		ch := s.backfillChannel(ctx, eng, h, t[0], t[1], cutoff)
		rep.Stored += ch.Stored
		rep.Channels = append(rep.Channels, ch)
	}
	_ = s.kvSet("backfill:last", rep)
	if rep.Stored > 0 {
		s.emit("memory", "backfill", map[string]any{"stored": rep.Stored})
	}
	return rep, ctx.Err()
}

func (s *Service) backfillChannel(ctx context.Context, eng *engine.Engine, h DiscordHistory, guildID, channelID, cutoff string) BackfillChannel {
	out := BackfillChannel{GuildID: guildID, ChannelID: channelID}
	last, err := eng.Store.LatestNativeID(engine.RoomKey(guildID, channelID))
	if err != nil {
		out.Skipped = err.Error()
		return out
	}
	if last == "" {
		out.Skipped = "never_captured"
		return out
	}
	after := laterID(last, cutoff)
	for page, pages := 0, s.backfillPages(); page < pages; page++ {
		batch, err := h.History(ctx, channelID, after, 100)
		if err != nil {
			out.Skipped = err.Error()
			return out
		}
		if len(batch) == 0 {
			return out
		}
		sort.Slice(batch, func(i, j int) bool { return idLess(batch[i].ID, batch[j].ID) })
		for _, m := range batch {
			out.Read++
			after = laterID(after, m.ID)
			if m.Bot || strings.TrimSpace(m.Content) == "" || !s.MemoryAllowed(m.AuthorID) {
				continue
			}
			stored, err := eng.Capture(engine.LiveMessage{GuildID: guildID, ChannelID: channelID, MessageID: m.ID, AuthorID: m.AuthorID,
				AuthorName: m.AuthorName, Content: m.Content, At: m.At})
			if err != nil {
				out.Skipped = err.Error()
				return out
			}
			if stored {
				out.Stored++
			}
		}
		if len(batch) < 100 {
			return out
		}
	}
	out.Truncated = true
	return out
}

// BackfillAfterConnect is what the gateway runs when a new session is ready.
func (s *Service) BackfillAfterConnect(ctx context.Context) {
	if s.Store == nil || s.Doc().Memory.Backfill() == 0 || len(s.capturingChannels()) == 0 {
		return
	}
	rep, err := s.Backfill(ctx, "connect", "", "")
	if err != nil && ctx.Err() == nil {
		s.Log.Warn("discord: catch-up skipped", "err", err)
		return
	}
	s.Log.Info("discord: caught up on missed messages", "channels", len(rep.Channels), "stored", rep.Stored)
}

// memoryBackfill is the memory.backfill Action: catch up now (one channel or all) and show the last run.
func (s *Service) memoryBackfill(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	if boolVal(in, "status") {
		var last BackfillReport
		ok, _ := s.kvGet("backfill:last", &last)
		out := map[string]any{"days": int(s.Doc().Memory.Backfill() / (24 * time.Hour)), "channels": len(s.capturingChannels())}
		if ok {
			out["last"] = last
		}
		return action.OK(req, out)
	}
	channelID := strVal(in, "channel_id", "")
	guildID := strVal(in, "guild_id", "")
	if channelID != "" && guildID == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id required with channel_id", nil)
	}
	if channelID != "" && !s.channelPolicy(guildID, channelID).Capture {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "this channel does not capture messages", nil)
	}
	rep, err := s.Backfill(ctx, "manual", guildID, channelID)
	if err != nil {
		return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), map[string]any{"report": rep})
	}
	return action.OK(req, map[string]any{"report": rep})
}
