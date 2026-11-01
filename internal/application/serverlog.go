package application

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/errclass"
	"masamong/internal/errcode"
)

// Server log: what happened in a server that its managers may want to see is posted to the server's
// log channel (config guilds.<id>.log), as the previous Masamong posted to a channel named "logs".
// It is off until a server's managers turn it on: a channel ID, or "auto" for a text channel named
// "logs" when the server has one.
//
//   - A deleted message: who wrote it, in which channel, when and what it said (log.deleted_text, on
//     unless turned off), from the gateway's cache of recent messages or the stored messages, read
//     before memory forgets it. Masamong keeps no copy: the entry is a post in the server's own log
//     channel. A bot's message and the log channel's own are left out.
//   - A problem in this server: a reply that failed, a tool that failed in a reply (the answer went out
//     without it), a scheduled post that could not be posted, a missing permission.
//
// Posting never waits on the event: entries go through a small queue, the same problem is posted once
// per serverLogRepeat, and a server gets at most serverLogBurst entries per serverLogWindow (the rest
// are counted and the next entry says how many were left out). Nothing is posted in practice mode,
// while replies are paused everywhere, or for a server waiting for approval.

const (
	serverLogRepeat = 15 * time.Minute // the same problem in the same channel
	serverLogWindow = 10 * time.Minute
	serverLogBurst  = 20
	serverLogQueue  = 64
	serverLogRest   = 30 * time.Minute // after Discord refused the log channel
	serverLogNames  = 10               // authors named in a bulk delete
	serverLogText   = 1500             // runes of a deleted message's text shown
	serverLogLines  = 10               // messages of a bulk delete shown with their text
	serverLogLine   = 200              // runes per message in a bulk delete
)

// DeletedMessage is one deleted message as the connector knows it: its author, text and attached file
// names when the gateway's cache still had the message (AuthorID "" = not known).
type DeletedMessage struct {
	ID        string
	AuthorID  string
	AuthorBot bool
	Text      string
	Files     []string
}

type serverLogEntry struct {
	guild, channel string
	card           func(locale string) action.Card
}

type serverLogState struct {
	once    sync.Once
	queue   chan serverLogEntry
	mu      sync.Mutex
	seen    map[string]time.Time   // problem key → last posted
	window  map[string][]time.Time // guild → posts in the current window
	dropped map[string]int         // guild → entries left out since the last post
	resting map[string]time.Time   // guild → Discord refused its log channel until
	last    map[string]serverLogResult
}

type serverLogResult struct {
	At      string `json:"at"`
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
	Channel string `json:"channel_id,omitempty"`
}

// serverLogChannel is the server's log channel and how it was found: set (an ID in config), auto (the
// channel named logs), off (nothing set, the default), or none (auto and no channel named logs).
func (s *Service) serverLogChannel(guildID string) (string, string) {
	l := s.Doc().Guilds[guildID].Log
	switch l.Channel {
	case "", "off":
		return "", "off"
	case config.GuildLogAuto:
		if s.Discord == nil {
			return "", "none"
		}
		for _, c := range s.Discord.Channels(guildID) {
			if strings.EqualFold(c.Name, config.GuildLogName) {
				return c.ID, "auto"
			}
		}
		return "", "none"
	}
	return l.Channel, "set"
}

// serverLogOK reports whether the server takes log entries now and where they go.
func (s *Service) serverLogOK(guildID string) (string, bool) {
	if s.practice || guildID == "" || s.Discord == nil || !s.Discord.Connected() || !s.GuildAllowed(guildID) || s.pauseState().All {
		return "", false
	}
	ch, _ := s.serverLogChannel(guildID)
	return ch, ch != ""
}

// LogDeletes posts deleted messages to the server's log. Authors the connector did not know are looked
// up in the stored messages; it is called before the deletion reaches memory.
func (s *Service) LogDeletes(guildID, channelID string, msgs []DeletedMessage) {
	logCh, ok := s.serverLogOK(guildID)
	if !ok || !s.Doc().Guilds[guildID].Log.DeletesOn() || channelID == logCh || len(msgs) == 0 {
		return
	}
	showText := s.Doc().Guilds[guildID].Log.TextOn()
	var lookup []string
	for _, m := range msgs {
		if m.AuthorID == "" || (showText && m.Text == "") {
			lookup = append(lookup, m.ID)
		}
	}
	stored := s.storedMessages(lookup)
	kept := make([]DeletedMessage, 0, len(msgs))
	for _, m := range msgs {
		if st, ok := stored[m.ID]; ok {
			if m.AuthorID == "" {
				m.AuthorID, m.AuthorBot = st.AuthorID, st.AuthorBot
			}
			if m.Text == "" {
				m.Text = st.Text
			}
		}
		if !showText {
			m.Text, m.Files = "", nil
		}
		if !m.AuthorBot {
			kept = append(kept, m)
		}
	}
	if len(kept) == 0 {
		return
	}
	now := s.Clock.Now()
	s.serverLogPost(serverLogEntry{guild: guildID, channel: logCh, card: func(locale string) action.Card {
		return s.deleteCard(locale, channelID, kept, showText, now)
	}})
}

// storedMessages finds deleted messages in what Masamong stored: the captured messages and the recent
// conversation (author and text), and its own replies (bots).
func (s *Service) storedMessages(ids []string) map[string]DeletedMessage {
	out := map[string]DeletedMessage{}
	if s.Store == nil || len(ids) == 0 {
		return out
	}
	if len(ids) > 200 {
		ids = ids[:200]
	}
	raw, _ := json.Marshal(ids)
	q := func(sql string, each func(id, a, b string)) {
		rows, err := s.Store.DB.Query(sql, string(raw))
		if err != nil {
			return
		}
		defer rows.Close()
		for rows.Next() {
			var id, a, b string
			if rows.Scan(&id, &a, &b) == nil {
				each(id, a, b)
			}
		}
	}
	fill := func(id, author, text string) {
		m := out[id]
		if m.AuthorID == "" {
			m.AuthorID = author
		}
		if m.Text == "" {
			m.Text = text
		}
		out[id] = m
	}
	q(`SELECT native_message_id, actor_account, content FROM memory_observations WHERE platform='discord' AND native_message_id IN (SELECT value FROM json_each(?))`, fill)
	q(`SELECT native_id, role, content FROM chat_turns WHERE native_id IN (SELECT value FROM json_each(?))`, func(id, role, text string) {
		if role == "assistant" {
			m := out[id]
			m.AuthorBot = true
			out[id] = m
		}
	})
	q(`SELECT native_id, speaker_id, content FROM chat_turns WHERE native_id IN (SELECT value FROM json_each(?)) AND role<>'assistant'`, fill)
	q(`SELECT message_id, '', '' FROM reply_messages WHERE message_id IN (SELECT value FROM json_each(?))`, func(id, _, _ string) {
		m := out[id]
		m.AuthorBot = true
		out[id] = m
	})
	return out
}

func (s *Service) deleteCard(locale, channelID string, msgs []DeletedMessage, showText bool, at time.Time) action.Card {
	t := func(key string, args ...any) string { return s.I18n.T(locale, key, args...) }
	footer := "serverlog.delete.footer"
	if showText {
		footer = "serverlog.delete.footer_text"
	}
	card := action.Card{Tone: "neutral", Footer: t(footer), Timestamp: at.UTC().Format(time.RFC3339)}
	if len(msgs) == 1 {
		m := msgs[0]
		author := t("serverlog.delete.unknown_author")
		if m.AuthorID != "" {
			author = "<@" + m.AuthorID + ">"
		}
		card.Title = t("serverlog.delete.title")
		if showText {
			card.Text = deletedText(t, m, serverLogText)
		}
		card.Fields = []action.CardField{{Name: t("serverlog.field.author"), Value: author, Inline: true},
			{Name: t("serverlog.field.channel"), Value: "<#" + channelID + ">", Inline: true}}
		if w := snowflakeTime(m.ID); !w.IsZero() {
			card.Fields = append(card.Fields, action.CardField{Name: t("serverlog.field.written"), Value: discordTime(w), Inline: true})
		}
		return card
	}
	card.Title = t("serverlog.delete.bulk_title", "n", len(msgs))
	seen := map[string]bool{}
	var names []string
	unknown := 0
	var first, last time.Time
	for _, m := range msgs {
		if w := snowflakeTime(m.ID); !w.IsZero() {
			if first.IsZero() || w.Before(first) {
				first = w
			}
			if w.After(last) {
				last = w
			}
		}
		switch {
		case m.AuthorID == "":
			unknown++
		case !seen[m.AuthorID]:
			seen[m.AuthorID] = true
			names = append(names, "<@"+m.AuthorID+">")
		}
	}
	authors := ""
	if len(names) > serverLogNames {
		authors = strings.Join(names[:serverLogNames], ", ") + " " + t("serverlog.delete.more_authors", "n", len(names)-serverLogNames)
	} else {
		authors = strings.Join(names, ", ")
	}
	if unknown > 0 {
		if authors != "" {
			authors += "\n"
		}
		authors += t("serverlog.delete.unknown_n", "n", unknown)
	}
	card.Fields = []action.CardField{{Name: t("serverlog.field.channel"), Value: "<#" + channelID + ">", Inline: true},
		{Name: t("serverlog.field.authors"), Value: authors, Inline: true}}
	if showText {
		var lines []string
		for i, m := range msgs {
			if i == serverLogLines {
				lines = append(lines, t("serverlog.delete.more_messages", "n", len(msgs)-serverLogLines))
				break
			}
			who := t("serverlog.delete.someone")
			if m.AuthorID != "" {
				who = "<@" + m.AuthorID + ">"
			}
			lines = append(lines, who+": "+strings.ReplaceAll(deletedText(t, m, serverLogLine), "\n", " "))
		}
		card.Text = strings.Join(lines, "\n")
	}
	if !first.IsZero() {
		span := discordTime(first)
		if last.Sub(first) > time.Minute {
			span += " – " + discordTime(last)
		}
		card.Fields = append(card.Fields, action.CardField{Name: t("serverlog.field.written"), Value: span})
	}
	return card
}

// deletedText is a deleted message's text for its entry, cut to n runes, with its attached files
// named; a message whose text Masamong never saw says so.
func deletedText(t func(string, ...any) string, m DeletedMessage, n int) string {
	text := strings.TrimSpace(m.Text)
	if text != "" {
		text = clip(text, n)
	}
	if len(m.Files) > 0 {
		files := t("serverlog.delete.files", "names", strings.Join(m.Files, ", "))
		if text != "" {
			text += "\n"
		}
		text += files
	}
	if text == "" {
		text = t("serverlog.delete.no_text")
	}
	return text
}

// Server log problem kinds (serverlog.problem.<kind> strings).
const (
	logReplyFailed    = "reply_failed"    // a reply could not be made
	logToolFailed     = "tool_failed"     // a tool failed in a reply; the answer went without it
	logPermission     = "permission"      // the bot could not post in a channel
	logReminderFailed = "reminder_failed" // a reminder could not be posted
	logTaskFailed     = "task_failed"     // a scheduled task made no answer
	logDisasterFailed = "disaster_failed" // an earthquake notice or weather warning could not be posted
)

// LogProblem posts a problem in a server to its log. tool names the tool (logToolFailed), class why.
func (s *Service) LogProblem(guildID, channelID, kind, tool string, class errclass.Class) {
	logCh, ok := s.serverLogOK(guildID)
	if !ok || !s.Doc().Guilds[guildID].Log.ProblemsOn() {
		return
	}
	if kind == logPermission && channelID == logCh {
		return // the log channel itself refused: posting the entry there would fail the same way
	}
	key := guildID + "|" + channelID + "|" + kind + "|" + tool
	st := &s.serverLog
	st.mu.Lock()
	now := s.Clock.Now()
	if at, ok := st.seen[key]; ok && now.Sub(at) < serverLogRepeat {
		st.mu.Unlock()
		return
	}
	if st.seen == nil {
		st.seen = map[string]time.Time{}
	}
	for k, at := range st.seen {
		if now.Sub(at) >= serverLogRepeat {
			delete(st.seen, k)
		}
	}
	st.seen[key] = now
	st.mu.Unlock()
	s.serverLogPost(serverLogEntry{guild: guildID, channel: logCh, card: func(locale string) action.Card {
		t := func(key string, args ...any) string { return s.I18n.T(locale, key, args...) }
		name := tool
		if label, ok := uiCatalog().Lookup(locale, "tool."+tool); ok {
			name = label
		}
		card := action.Card{Tone: "warn", Title: t("serverlog.problem."+kind+".title", "tool", name), Text: t("serverlog.problem."+kind, "tool", name),
			Footer: t("serverlog.problem.footer"), Timestamp: now.UTC().Format(time.RFC3339)}
		if channelID != "" {
			card.Fields = append(card.Fields, action.CardField{Name: t("serverlog.field.channel"), Value: "<#" + channelID + ">", Inline: true})
		}
		if class != "" && class != errclass.None {
			reason, ok := s.I18n.Lookup(locale, "serverlog.reason."+string(class))
			if !ok {
				reason = t("serverlog.reason.unknown")
			}
			card.Fields = append(card.Fields, action.CardField{Name: t("serverlog.field.reason"), Value: reason, Inline: true})
		}
		return card
	}})
}

// logToolProblem reports a tool that failed in a reply because a service let it down (no key, no
// answer, too many requests, an answer that did not parse). What the person asked wrongly, a limit
// reached or a missing right of the person are not problems of the server.
func (s *Service) logToolProblem(guildID, channelID, tool string, res action.Result, panicked bool) {
	if panicked {
		s.LogProblem(guildID, channelID, logToolFailed, tool, errclass.Unknown)
		return
	}
	if res.OK || res.Error == nil {
		return
	}
	switch res.Error.Code {
	case errcode.ProviderUnconfigured, errcode.ProviderUnavailable, errcode.ProviderRateLimited, errcode.ProviderFormat, errcode.ProviderRejected, errcode.ProviderError, errcode.IO:
		s.LogProblem(guildID, channelID, logToolFailed, tool, toolErrClass(res.Error.Code))
	}
}

// serverLogPost queues an entry; a full queue drops it (counted with the server's left-out entries).
func (s *Service) serverLogPost(e serverLogEntry) {
	st := &s.serverLog
	st.once.Do(func() {
		st.queue = make(chan serverLogEntry, serverLogQueue)
		go s.serverLogWorker()
	})
	select {
	case st.queue <- e:
	default:
		st.mu.Lock()
		if st.dropped == nil {
			st.dropped = map[string]int{}
		}
		st.dropped[e.guild]++
		st.mu.Unlock()
	}
}

func (s *Service) serverLogWorker() {
	for e := range s.serverLog.queue {
		s.serverLogSend(e)
	}
}

// serverLogSend posts one entry within the server's burst limit, noting entries left out before it.
func (s *Service) serverLogSend(e serverLogEntry) {
	st := &s.serverLog
	now := s.Clock.Now()
	st.mu.Lock()
	if until, ok := st.resting[e.guild]; ok && now.Before(until) {
		st.mu.Unlock()
		return
	}
	if st.window == nil {
		st.window = map[string][]time.Time{}
	}
	if st.dropped == nil {
		st.dropped = map[string]int{}
	}
	kept := st.window[e.guild][:0]
	for _, at := range st.window[e.guild] {
		if now.Sub(at) < serverLogWindow {
			kept = append(kept, at)
		}
	}
	if len(kept) >= serverLogBurst {
		st.window[e.guild] = kept
		st.dropped[e.guild]++
		st.mu.Unlock()
		return
	}
	st.window[e.guild] = append(kept, now)
	dropped := st.dropped[e.guild]
	delete(st.dropped, e.guild)
	st.mu.Unlock()

	poster, ok := s.Discord.(ChannelPoster)
	if !ok {
		return
	}
	locale := s.effectiveLocale(e.guild)
	card := e.card(locale)
	if dropped > 0 {
		note := s.I18n.T(locale, "serverlog.dropped", "n", dropped)
		if card.Text != "" {
			card.Text += "\n"
		}
		card.Text += note
	}
	ctx, cancel := context.WithTimeout(s.serverLogContext(), 20*time.Second)
	_, err := poster.PostChannel(ctx, e.channel, "", "", &card)
	cancel()
	res := serverLogResult{At: now.UTC().Format(time.RFC3339), OK: err == nil, Channel: e.channel}
	st.mu.Lock()
	if err != nil {
		res.Reason = string(errclass.Of(err))
		if c := errclass.Of(err); c == errclass.Permission || c == errclass.NotFound {
			if st.resting == nil {
				st.resting = map[string]time.Time{}
			}
			st.resting[e.guild] = now.Add(serverLogRest)
		}
	} else {
		delete(st.resting, e.guild)
	}
	if st.last == nil {
		st.last = map[string]serverLogResult{}
	}
	st.last[e.guild] = res
	st.mu.Unlock()
	if err != nil {
		s.Log.Warn("server log: post failed", "class", res.Reason)
	}
}

// serverLogContext bounds a post to the daemon run.
func (s *Service) serverLogContext() context.Context {
	if s.jobCtx != nil {
		return s.jobCtx
	}
	return context.Background()
}

// serverLogView is a server's log for channels.list: where it goes, how it was found, what it takes and
// the last post.
func (s *Service) serverLogView(guildID string) map[string]any {
	l := s.Doc().Guilds[guildID].Log
	ch, how := s.serverLogChannel(guildID)
	out := map[string]any{"channel_id": ch, "source": how, "deletes": l.DeletesOn(), "deleted_text": l.TextOn(), "problems": l.ProblemsOn()}
	if ch != "" && s.Discord != nil && s.Discord.Connected() {
		if bits, ok := s.Discord.Permissions(guildID, ch); ok && bits&permAdministrator == 0 {
			var missing []string
			for _, p := range requiredPerms {
				if p.Key != "read_message_history" && bits&p.Bit == 0 {
					missing = append(missing, p.Key)
				}
			}
			if len(missing) > 0 {
				out["missing"] = missing
			}
		}
		for _, c := range s.Discord.Channels(guildID) {
			if c.ID == ch {
				out["channel_name"] = c.Name
			}
		}
	}
	s.serverLog.mu.Lock()
	if r, ok := s.serverLog.last[guildID]; ok {
		out["last"] = r
	}
	s.serverLog.mu.Unlock()
	return out
}

// serverLogTest posts a test entry to a server's log channel and waits for the outcome
// (serverlog.test, input guild_id).
func (s *Service) serverLogTest(ctx context.Context, req action.Request) action.Result {
	var in struct {
		GuildID string `json:"guild_id"`
	}
	_ = json.Unmarshal(req.Input, &in)
	if in.GuildID == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "guild_id is required", nil)
	}
	if s.practice {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "practice mode posts nothing to Discord", map[string]any{"reason": "practice"})
	}
	poster, ok := s.Discord.(ChannelPoster)
	if !ok || !s.Discord.Connected() {
		return action.Fail(req, errcode.DiscordNotConnected, "generic.error", "Discord is not connected", map[string]any{"reason": "discord_offline"})
	}
	ch, how := s.serverLogChannel(in.GuildID)
	if ch == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "this server has no log channel ("+how+")", map[string]any{"reason": how})
	}
	locale := s.effectiveLocale(in.GuildID)
	now := s.Clock.Now()
	card := action.Card{Tone: "neutral", Title: s.I18n.T(locale, "serverlog.test.title"), Text: s.I18n.T(locale, "serverlog.test.text"),
		Footer: s.I18n.T(locale, "serverlog.problem.footer"), Timestamp: now.UTC().Format(time.RFC3339)}
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	_, err := poster.PostChannel(pctx, ch, "", "", &card)
	cancel()
	res := serverLogResult{At: now.UTC().Format(time.RFC3339), OK: err == nil, Channel: ch}
	if err != nil {
		res.Reason = string(errclass.Of(err))
	}
	s.serverLog.mu.Lock()
	if s.serverLog.last == nil {
		s.serverLog.last = map[string]serverLogResult{}
	}
	s.serverLog.last[in.GuildID] = res
	if err == nil {
		delete(s.serverLog.resting, in.GuildID)
	}
	s.serverLog.mu.Unlock()
	if err != nil {
		return action.Fail(req, errcode.ProviderUnavailable, "generic.error", "Discord refused the post: "+res.Reason, map[string]any{"reason": res.Reason, "channel_id": ch})
	}
	return action.OK(req, map[string]any{"channel_id": ch, "source": how, "last": res})
}

// snowflakeTime is when a Discord ID was made (a message: when it was written); zero if not an ID.
func snowflakeTime(id string) time.Time {
	n, err := strconv.ParseUint(id, 10, 64)
	if err != nil || n < 1<<22 {
		return time.Time{}
	}
	return time.UnixMilli(int64(n>>22) + 1420070400000)
}

// discordTime is a time Discord shows in each reader's own time zone.
func discordTime(t time.Time) string {
	return "<t:" + strconv.FormatInt(t.Unix(), 10) + ":f>"
}
