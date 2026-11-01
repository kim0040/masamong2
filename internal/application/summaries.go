package application

import (
	"context"
	"database/sql"
	"errors"
	"strings"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/errcode"
	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
	"masamong/internal/storage"
)

// summaries.channel (19장 A27, V1 `!요약`): a structured summary of a channel's recent conversation.
// It reads the channel's stored messages (so the channel must capture), keeps the last summary with the
// message it ends at, and on the next request only folds the new messages into it — unless there are
// many, then it summarizes the recent window again. Nothing new since the last summary returns it as is.
// {saved: true} only reads the last summary (no model call).
//
// The summary is kept only while what it was made from stands: an edit, a deletion or a withdrawn
// consent in the channel drops it (it may hold the removed text, and the anchor alone would not
// notice a change before it), and a summary that was being made while the channel changed is not
// kept. Of two summaries made at once, the one that read less far does not replace the other.

// Defaults of the knobs; tools.summaries.channel.defaults overrides them (look_back, increment_max,
// max_chars, min_messages, max_tokens).
const (
	summaryLookBack     = 120  // messages of a full summary
	summaryIncrementMax = 24   // new messages beyond which a full summary is made instead
	summaryMaxChars     = 3200 // conversation text sent to the model
	summaryMinMessages  = 3
	summaryMaxTokens    = 900
)

func init() {
	featureDDL = append(featureDDL, `CREATE TABLE IF NOT EXISTS channel_summary (
		guild_id TEXT NOT NULL,
		channel_id TEXT NOT NULL,
		anchor_observation TEXT NOT NULL,
		anchor_time TEXT NOT NULL,
		summary TEXT NOT NULL,
		messages INTEGER NOT NULL DEFAULT 0,
		updated_at TEXT NOT NULL,
		PRIMARY KEY(guild_id, channel_id)
	)`)
}

// summaryRevs counts, per room, the edits and deletions seen (engine.OnRevised), so a summary can
// tell whether its messages changed while the model was writing it.
var summaryRevs sync.Map // *Service → *roomRevisions

type roomRevisions struct {
	mu sync.Mutex
	n  map[string]uint64
}

func (s *Service) roomRevisions() *roomRevisions {
	v, _ := summaryRevs.LoadOrStore(s, &roomRevisions{n: map[string]uint64{}})
	return v.(*roomRevisions)
}

func (s *Service) roomRevision(room string) uint64 {
	r := s.roomRevisions()
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n[room]
}

// summariesRevised drops the saved summaries of the rooms an edit, deletion or withdrawn consent
// changed (engine.OnRevised).
func (s *Service) summariesRevised(rooms []string) {
	r := s.roomRevisions()
	r.mu.Lock()
	for _, room := range rooms {
		r.n[room]++
	}
	r.mu.Unlock()
	db, err := s.adminDB()
	if err != nil {
		return
	}
	for _, room := range rooms {
		parts := strings.SplitN(room, ":", 3) // engine.RoomKey
		if len(parts) != 3 || parts[0] != "discord" {
			continue
		}
		guild, alt := parts[1], parts[1]
		if guild == "0" {
			alt = "" // RoomKey writes a missing guild as "0"
		}
		if err := db.WithWriter(func(tx *sql.Tx) error {
			_, err := tx.Exec(`DELETE FROM channel_summary WHERE channel_id=? AND guild_id IN (?, ?)`, parts[2], guild, alt)
			return err
		}); err != nil {
			s.Log.Debug("summary invalidation failed", "err", err.Error())
		}
	}
}

type channelSummaryState struct {
	AnchorObservation, AnchorTime, Summary, UpdatedAt string
	Messages                                          int
}

func (s *Service) loadChannelSummary(guildID, channelID string) (channelSummaryState, bool) {
	db, err := s.adminDB()
	if err != nil {
		return channelSummaryState{}, false
	}
	var st channelSummaryState
	err = db.DB.QueryRow(`SELECT anchor_observation, anchor_time, summary, messages, updated_at FROM channel_summary WHERE guild_id=? AND channel_id=?`,
		guildID, channelID).Scan(&st.AnchorObservation, &st.AnchorTime, &st.Summary, &st.Messages, &st.UpdatedAt)
	return st, err == nil && st.Summary != ""
}

// conversationText renders messages as "speaker: text" lines, keeping the latest when too long.
func conversationText(msgs []corpus.Message, maxChars int) string {
	lines := make([]string, 0, len(msgs))
	for _, m := range msgs {
		if t := strings.TrimSpace(m.Content); t != "" {
			lines = append(lines, m.ActorLabel+": "+t)
		}
	}
	out := strings.Join(lines, "\n")
	if r := []rune(out); len(r) > maxChars {
		out = string(r[len(r)-maxChars:])
		if i := strings.IndexByte(out, '\n'); i >= 0 {
			out = out[i+1:] // start at a whole line
		}
	}
	return out
}

func (s *Service) channelSummary(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	guildID, channelID := strVal(in, "guild_id", ""), strVal(in, "channel_id", "")
	if req.Invoker != nil && req.Invoker.ChannelID != "" {
		guildID, channelID = req.Invoker.GuildID, req.Invoker.ChannelID
	}
	if channelID == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id and channel_id required", nil)
	}
	if boolVal(in, "saved") {
		// the console's channel page: show the last summary without making one
		prev, ok := s.loadChannelSummary(guildID, channelID)
		if !ok {
			return action.OK(req, map[string]any{"status": "none"})
		}
		return action.OK(req, map[string]any{"status": "ok", "summary": prev.Summary, "messages": prev.Messages, "updated_at": prev.UpdatedAt, "cached": true})
	}
	eng, err := s.MemoryEngine()
	if err != nil {
		return action.Fail(req, errcode.NotSetup, "generic.error", err.Error(), nil)
	}
	lookBack := min(1000, max(10, intVal(in, "look_back", summaryLookBack)))
	incrementMax := max(1, intVal(in, "increment_max", summaryIncrementMax))
	maxChars := min(20000, max(500, intVal(in, "max_chars", summaryMaxChars)))
	room := engine.RoomKey(guildID, channelID)
	rev := s.roomRevision(room)
	msgs, err := eng.Store.RecentMessages(room, lookBack)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	title := s.msg(req, "summary.title")
	if len(msgs) < max(1, intVal(in, "min_messages", summaryMinMessages)) {
		hint := "summary.too_few"
		if !s.channelPolicy(guildID, channelID).Capture || !s.Doc().Memory.Capture {
			hint = "summary.not_capturing"
		}
		return action.WithCard(action.OK(req, map[string]any{"status": "empty", "messages": len(msgs)}),
			action.Card{Title: title, Text: s.msg(req, hint), Tone: "warn"})
	}
	last := msgs[len(msgs)-1]
	prev, havePrev := s.loadChannelSummary(guildID, channelID)
	out := map[string]any{"status": "ok", "guild_id": guildID, "channel_id": channelID}
	if havePrev && prev.AnchorObservation == last.ObservationID && !boolVal(in, "refresh") {
		out["summary"], out["cached"], out["messages"], out["updated_at"] = prev.Summary, true, prev.Messages, prev.UpdatedAt
		return action.WithCard(action.OK(req, out), action.Card{Title: title, Text: prev.Summary, Footer: s.msg(req, "summary.cached")})
	}
	var fresh []corpus.Message
	if havePrev && !boolVal(in, "refresh") {
		for _, m := range msgs {
			if m.ParsedTime > prev.AnchorTime || (m.ParsedTime == prev.AnchorTime && m.ObservationID > prev.AnchorObservation) {
				fresh = append(fresh, m)
			}
		}
	}
	pack := s.voiceFor(guildID, s.channelPolicy(guildID, channelID)).Pack
	var prompt string
	incremental := havePrev && len(fresh) > 0 && len(fresh) <= incrementMax && !boolVal(in, "refresh")
	used := msgs
	if incremental {
		used = fresh
		prompt = lang.Fill(pack.Features.ChannelSummaryIncremental, "previous", prev.Summary, "conversation", conversationText(fresh, maxChars))
	} else {
		prompt = lang.Fill(pack.Features.ChannelSummary, "conversation", conversationText(msgs, maxChars))
	}
	text, cost, err := s.featureText(ctx, "summary", "summary", "", prompt, max(100, intVal(in, "max_tokens", summaryMaxTokens)), 0.3)
	if err != nil {
		if errors.Is(err, ErrChatBudget) {
			return action.Fail(req, errcode.BudgetExceeded, "summary.budget", s.msg(req, "summary.budget"), nil)
		}
		return action.Fail(req, errcode.ProviderError, "generic.error", err.Error(), nil)
	}
	now := s.Clock.Now().UTC().Format(time.RFC3339Nano)
	if s.roomRevision(room) != rev {
		// a message it read was edited or deleted meanwhile: shown once, not kept
		out["stale"] = true
	} else if db, err := s.adminDB(); err == nil {
		if err := s.saveChannelSummary(ctx, db, guildID, channelID, last, text, len(msgs), now, used, incremental, prev); err != nil {
			out["stale"] = true
			s.Log.Debug("channel summary not persisted", "err", err)
		}
	}
	out["summary"], out["incremental"], out["messages"], out["read"], out["cost_usd"], out["updated_at"] = text, incremental, len(msgs), len(used), cost, now
	footer := s.msg(req, "summary.full", "n", len(used))
	if incremental {
		footer = s.msg(req, "summary.incremental", "n", len(used))
	}
	return action.WithCard(action.OK(req, out), action.Card{Title: title, Text: text, Footer: footer})
}

// The model's source snapshot and its source index commit with the summary. In-memory room revision
// checks alone leave a crash and race gap between checking and acquiring the database writer.
func (s *Service) saveChannelSummary(ctx context.Context, db *storage.Store, guild, channel string, last corpus.Message, text string, n int, at string, used []corpus.Message, incremental bool, previous channelSummaryState) error {
	return db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		keys := []string{}
		for _, m := range used {
			var content, label, parsed, deleted string
			if err := tx.QueryRowContext(ctx, `SELECT content,actor_label,parsed_time,deleted_at FROM memory_observations WHERE observation_id=?`, m.ObservationID).Scan(&content, &label, &parsed, &deleted); err != nil {
				return err
			}
			current, err := store.ObservationCurrent(tx, m.ObservationID)
			if err != nil {
				return err
			}
			if deleted != "" || strings.TrimSpace(content) != m.Content || label != m.ActorLabel || parsed != m.ParsedTime || !current {
				return errEvidenceDeleted
			}
			keys = append(keys, m.ObservationID)
		}
		id := "summary:" + guild + ":" + channel
		if incremental {
			var current channelSummaryState
			if err := tx.QueryRow(`SELECT anchor_observation,anchor_time,summary,updated_at FROM channel_summary WHERE guild_id=? AND channel_id=?`, guild, channel).Scan(&current.AnchorObservation, &current.AnchorTime, &current.Summary, &current.UpdatedAt); err != nil {
				return err
			}
			if current.AnchorObservation != previous.AnchorObservation || current.AnchorTime != previous.AnchorTime || current.Summary != previous.Summary || current.UpdatedAt != previous.UpdatedAt {
				return errEvidenceDeleted
			}
			rows, err := tx.Query(`SELECT t.at FROM privacy_tombstones t JOIN evidence_refs r ON r.source_id=t.source_id WHERE r.holder_kind='channel_summary' AND r.holder_id=?`, id)
			if err != nil {
				return err
			}
			saved, parseErr := time.Parse(time.RFC3339Nano, previous.UpdatedAt)
			stale := parseErr != nil
			for rows.Next() {
				var at string
				if err := rows.Scan(&at); err != nil {
					rows.Close()
					return err
				}
				changed, err := time.Parse(time.RFC3339Nano, at)
				stale = stale || err != nil || !saved.After(changed)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
			if stale {
				return errEvidenceDeleted
			}
			rows, err = tx.Query(`SELECT source_id FROM evidence_refs WHERE holder_kind='channel_summary' AND holder_id=?`, id)
			if err != nil {
				return err
			}
			for rows.Next() {
				var key string
				if err := rows.Scan(&key); err != nil {
					rows.Close()
					return err
				}
				keys = append(keys, key)
			}
			err = rows.Err()
			rows.Close()
			if err != nil {
				return err
			}
		}
		result, err := tx.Exec(`INSERT INTO channel_summary(guild_id,channel_id,anchor_observation,anchor_time,summary,messages,updated_at,evidence_state) VALUES(?,?,?,?,?,?,?,1)
			ON CONFLICT(guild_id,channel_id) DO UPDATE SET anchor_observation=excluded.anchor_observation,anchor_time=excluded.anchor_time,summary=excluded.summary,messages=excluded.messages,updated_at=excluded.updated_at,evidence_state=1
			WHERE excluded.anchor_time>channel_summary.anchor_time OR (excluded.anchor_time=channel_summary.anchor_time AND excluded.anchor_observation>=channel_summary.anchor_observation)`, guild, channel, last.ObservationID, last.ParsedTime, text, n, at)
		if err != nil {
			return err
		}
		changed, err := result.RowsAffected()
		if err != nil || changed == 0 {
			return err
		}
		if !incremental {
			if _, err := tx.Exec(`DELETE FROM evidence_refs WHERE holder_kind='channel_summary' AND holder_id=?`, id); err != nil {
				return err
			}
		}
		keys, err = copySources(tx, keys)
		if err != nil {
			return err
		}
		return indexCopy(tx, "channel_summary", id, keys, false)
	})
}
