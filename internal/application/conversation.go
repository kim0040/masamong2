package application

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/conversation"
	"masamong/internal/lang"
	"masamong/internal/memory/blocks"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
)

// The caller's conversation (docs/plan-memory-conversation.md §5.1): instead of handing the reply the
// channel's last dozen lines and asking it to ignore what is not for it, the turn is built from the lines
// that belong to the caller's own conversation — the call, its replies and mentions, the bot's earlier
// answers to this person, their bursts — plus the lines a small classifier finds the call pointing at.
// Everything else in the room stays out of the prompt.

// L0 budget: the newest lines that fit, never fewer than the eight before the call (the context of a short
// call usually sits in the two to seven lines before it).
const (
	transcriptChars    = 2400
	transcriptMinLines = 8
	referTimeout       = 8 * time.Second
)

// convContext is what the turn reads instead of the plain history and recent room lines.
type convContext struct {
	history []chat.Turn
	sources []string
	ledger  map[string]any // body-free record of the decision, kept in the turn log
	costUSD float64
}

// botAccount is the bot's own Discord account ID, when known.
func (s *Service) botAccount(lines []conversation.Line) string {
	if s.Discord != nil {
		if id, _ := s.Discord.BotUser(); id != "" {
			return id
		}
	}
	for _, l := range lines {
		if l.Bot && l.AuthorID != "" {
			return l.AuthorID
		}
	}
	return "bot"
}

// callerConversation builds the turn's context from the caller's conversation. ok is false when the
// channel turned attribution off or the turn has no Discord message to start from (console and lab
// turns), and the turn uses the plain history as before.
func (s *Service) callerConversation(ctx context.Context, in ChatInput, eng *engine.Engine, pol config.ChannelPolicy, voice chat.Voice, llm chatLLM) (convContext, bool) {
	mode := pol.Attribution
	if mode == "" {
		mode = config.AttributionModes[0]
	}
	if mode == "off" || in.MessageID == "" || in.ChannelID == "" {
		return convContext{}, false
	}
	lines := s.channelLines(in, eng)
	botID := s.botAccount(lines)
	found := false
	for _, l := range lines {
		if l.ID == in.MessageID {
			found = true
			break
		}
	}
	if !found {
		// a console-sent or racing message the window has not seen yet: the call itself is always there
		lines = append(lines, conversation.Line{ID: in.MessageID, AuthorID: in.SpeakerID, Author: in.Speaker, At: s.Clock.Now(),
			Text: in.Message, Mentions: []string{botID}})
	}
	att := conversation.Attribute(lines, []string{in.MessageID}, in.SpeakerID,
		conversation.Options{BotID: botID, Names: conversation.NamesFrom(lines)})
	out := convContext{ledger: map[string]any{"mode": mode, "included": len(att.Included), "excluded": att.Excluded,
		"candidates": len(att.Candidates)}}
	reasons := map[string]int{}
	for _, why := range att.Reasons {
		reasons[why]++
	}
	out.ledger["reasons"] = reasons
	keep := append([]string(nil), att.Included...)
	if mode == "on" && len(att.Candidates) > 0 {
		refs, cost, state := s.resolveReference(ctx, in, conversation.Keep(lines, att.Candidates), voice, llm)
		out.costUSD += cost
		out.ledger["refer"] = state
		out.ledger["referenced"] = len(refs)
		keep = append(keep, refs...)
	}
	picked := conversation.Keep(lines, keep)
	// how many people talk to the bot in this conversation (a thread is not offered to a group)
	people := map[string]bool{}
	for _, l := range picked {
		if !l.Bot && (containsString(l.Mentions, botID) || authorOfLine(lines, l.ReplyTo) == botID) {
			people[l.AuthorID] = true
		}
	}
	out.ledger["callers"] = len(people)
	var context []conversation.Line
	for _, l := range picked {
		if l.ID != in.MessageID && strings.TrimSpace(l.Text) != "" {
			context = append(context, l)
		}
	}
	context, omitted := conversation.Select(context, transcriptChars, transcriptMinLines)
	if omitted > 0 {
		out.ledger["omitted"] = omitted
	}
	room := engine.RoomKey(in.GuildID, in.ChannelID)
	names := map[string]string{}
	for _, l := range lines {
		names[l.ID] = l.Author
		if l.Bot {
			names[l.ID] = voice.BotName
		}
	}
	for _, l := range context {
		if l.Bot {
			out.history = append(out.history, chat.Turn{Role: "assistant", Content: l.Text, At: l.At})
		} else {
			speaker := l.Author
			// a reply to a line outside the shown context still says whom it answered
			if target, ok := names[l.ReplyTo]; ok && target != "" && target != l.Author {
				speaker += " ↩" + target
			}
			out.history = append(out.history, chat.Turn{Role: "user", Speaker: speaker, Content: l.Text, At: l.At})
		}
		id := strings.TrimPrefix(l.ID, "turn:")
		out.sources = append(out.sources, id)
		if !l.Bot {
			out.sources = append(out.sources, corpus.DiscordLiveObservationID(room, id))
		}
	}
	return out, true
}

// channelLines merges what is known of the channel's recent messages: stored messages (capture on), the
// stored conversation with the bot (its answers keep exactly what reached the channel) and the live window
// (reply and mention links, the bot's own posts), one line per message.
func (s *Service) channelLines(in ChatInput, eng *engine.Engine) []conversation.Line {
	byID := map[string]conversation.Line{}
	var order []string
	put := func(l conversation.Line) {
		if l.ID == "" {
			return
		}
		if _, ok := byID[l.ID]; !ok {
			order = append(order, l.ID)
		}
		byID[l.ID] = l
	}
	room := engine.RoomKey(in.GuildID, in.ChannelID)
	if stored, err := eng.Store.RecentLines(room, 40); err == nil {
		for _, m := range stored {
			at, _, _ := corpus.ParseTime(m.ParsedTime)
			id := m.NativeID
			if id == "" {
				id = m.ObservationID
			}
			put(conversation.Line{ID: id, AuthorID: m.Account, Author: m.Label, At: at, Text: m.Content})
		}
	}
	answered := map[string]bool{} // user messages the stored conversation has the bot's answer to
	if turns, err := eng.Store.RecentChatTurns(blocks.ChannelKey(in.GuildID, in.ChannelID), 24); err == nil {
		for _, t := range turns {
			if t.NativeID == "" {
				continue
			}
			at, _ := time.Parse(time.RFC3339Nano, t.CreatedAt)
			switch t.Role {
			case "user":
				if _, ok := byID[t.NativeID]; !ok {
					put(conversation.Line{ID: t.NativeID, AuthorID: t.SpeakerID, Author: t.Speaker, At: at, Text: t.Content, Mentions: []string{"bot"}})
				}
			case "assistant":
				answered[t.NativeID] = true
				put(conversation.Line{ID: "turn:" + t.NativeID, Bot: true, ReplyTo: t.NativeID, At: at.Add(time.Millisecond), Text: t.Content})
			}
		}
	}
	for _, l := range s.liveLines(in.GuildID, in.ChannelID) {
		if l.Bot && answered[l.ReplyTo] {
			continue // the stored answer has the whole reply; its posted parts would repeat it
		}
		if old, ok := byID[l.ID]; ok && l.Text == "" && old.Text != "" && !l.Bot {
			l.Text = old.Text // consent is checked when the window is filled; the store checks its own
		}
		put(l)
	}
	out := make([]conversation.Line, 0, len(order))
	bot := s.botAccount(nil)
	for _, id := range order {
		l := byID[id]
		if l.Bot && l.AuthorID == "" {
			l.AuthorID = bot
		}
		for i, m := range l.Mentions {
			if m == "bot" {
				l.Mentions[i] = bot
			}
		}
		out = append(out, l)
	}
	return out
}

// resolveReference asks the classifier which of the other conversations' lines the call points at. Any
// failure or doubt keeps them out (fail-closed): a missing line costs a follow-up question, a wrong one
// puts somebody else's topic in the bot's mouth.
func (s *Service) resolveReference(ctx context.Context, in ChatInput, cands []conversation.Line, voice chat.Voice, llm chatLLM) ([]string, float64, string) {
	pack := voice.Pack
	if pack == nil || pack.Conversation.Refer == "" || len(cands) == 0 {
		return nil, 0, "skipped"
	}
	text, labels := conversation.Numbered(cands, voice.BotName, 300)
	msgs := []chat.Msg{
		{Role: "system", Content: lang.Fill(pack.Conversation.ReferSystem, "bot", voice.BotName)},
		{Role: "user", Content: lang.Fill(pack.Conversation.Refer, "lines", text, "speaker", in.Speaker, "message", clip(in.Message, 600))},
	}
	ctx, cancel := context.WithTimeout(ctx, referTimeout)
	defer cancel()
	ctx = withQuickAnswer(withLedgerRole(ctx, "reference"))
	out, err := llm.ChatDetailed(ctx, "planner", msgs, 80, 0)
	if err != nil {
		return nil, out.CostUSD, "error"
	}
	var reply struct {
		RefersTo []string `json:"refers_to"`
	}
	if !decodeJSONObject(out.Text, &reply) {
		return nil, out.CostUSD, "unparsed"
	}
	var ids []string
	for _, label := range reply.RefersTo {
		if id, ok := labels[strings.TrimSpace(label)]; ok {
			ids = append(ids, id)
		}
	}
	if len(ids) == 0 {
		return nil, out.CostUSD, "none"
	}
	return ids, out.CostUSD, "picked"
}

// nameCallGate asks whether a message that only said the bot's name was said to the bot. Anything but a
// clear "to the bot" — about it, unsure, a failed call — keeps the bot quiet (fail-closed): a missed call
// costs the person a tag, a wrong answer makes the bot butt into talk about itself. Every decision is
// logged (conversation_log) as reference data.
func (s *Service) nameCallGate(ctx context.Context, in ChatInput) (banter bool, err error) {
	pol := s.channelPolicy(in.GuildID, in.ChannelID)
	if pol.NameCall != "addressed" {
		return false, nil
	}
	doc := s.Doc()
	voice := s.voiceFor(in.GuildID, pol)
	voice.Pack = lang.For(chat.DetectLanguage(in.Message), in.Locale, voice.Pack.Tag)
	decision, tone, cost := "error", "", 0.0
	defer func() {
		s.logConversation(convDecision{Guild: in.GuildID, Channel: in.ChannelID, Message: in.MessageID, Author: in.SpeakerID,
			Kind: convNameCall, Decision: decision, Mode: pol.NameCall, CostUSD: cost,
			Features: map[string]any{"tone": tone, "runes": len([]rune(in.Message))}})
		if decision != "bot" {
			s.collectMention(in) // chiming-in data, recorded only where that mode is collect
		}
		if errors.Is(err, ErrNotAddressed) {
			err = notAddressedErr{decision: decision, tone: tone} // the turn record shows why
		}
	}()
	routes, err := s.chatRoutes(doc)
	if err != nil {
		return false, ErrNotAddressed
	}
	var before []conversation.Line
	for _, l := range s.liveLines(in.GuildID, in.ChannelID) {
		if l.ID != in.MessageID && strings.TrimSpace(l.Text) != "" {
			before = append(before, l)
		}
	}
	if len(before) > 6 {
		before = before[len(before)-6:]
	}
	text, _ := conversation.Numbered(before, voice.BotName, 200)
	if text == "" {
		text = voice.Pack.Chat.None
	}
	c := voice.Pack.Conversation
	msgs := []chat.Msg{
		{Role: "system", Content: lang.Fill(c.NameCallSystem, "bot", voice.BotName)},
		{Role: "user", Content: lang.Fill(c.NameCall, "lines", text, "speaker", in.Speaker, "message", clip(in.Message, 600), "bot", voice.BotName)},
	}
	cctx, cancel := context.WithTimeout(ctx, referTimeout)
	defer cancel()
	cctx = withQuickAnswer(withLedgerRole(cctx, "name_call"))
	out, err := chatLLM{s: s, routes: routes}.ChatDetailed(cctx, "planner", msgs, 60, 0)
	cost = out.CostUSD
	if err != nil {
		return false, ErrNotAddressed
	}
	var reply struct {
		To   string `json:"to"`
		Tone string `json:"tone"`
	}
	if !decodeJSONObject(out.Text, &reply) {
		decision = "unparsed"
		return false, ErrNotAddressed
	}
	decision, tone = reply.To, reply.Tone
	if !slices.Contains([]string{"playful", "neutral", "annoyed"}, tone) {
		tone = "" // the log keeps closed values only, never model text
	}
	if !slices.Contains([]string{"bot", "about_bot", "unsure"}, decision) {
		decision = "unparsed"
	}
	if reply.To == "about_bot" && reply.Tone == "playful" {
		return s.banterGate(in, pol)
	}
	if reply.To != "bot" {
		return false, ErrNotAddressed
	}
	return false, nil
}

// notAddressedErr is ErrNotAddressed with what the name-call check decided (about_bot, unsure, unparsed,
// error) and the tone it heard; its text stays ErrNotAddressed's.
type notAddressedErr struct{ decision, tone string }

func (e notAddressedErr) Error() string        { return ErrNotAddressed.Error() }
func (e notAddressedErr) Is(target error) bool { return target == ErrNotAddressed }

// banterGate decides the answer to a playful mention of the bot (banter.go): a line (the turn goes on
// with the banter rules), an emoji (ReactOnly), or silence. Every decision is logged, also in shadow.
func (s *Service) banterGate(in ChatInput, pol config.ChannelPolicy) (bool, error) {
	mode := pol.Conversation.Banter
	if mode == "" || mode == "off" {
		return false, ErrNotAddressed
	}
	step := s.banterStep(in.GuildID, in.ChannelID)
	decision := step
	if step == "" {
		decision = "quiet"
	}
	if mode != "on" {
		decision = "would_" + decision
	}
	s.logConversation(convDecision{Guild: in.GuildID, Channel: in.ChannelID, Message: in.MessageID, Author: in.SpeakerID,
		Kind: convBanter, Decision: decision, Mode: mode})
	if mode != "on" || step == "" {
		return false, ErrNotAddressed
	}
	if step == "emoji" {
		return false, ReactOnly{Emoji: s.banterEmoji(pol, in.GuildID), sent: func() {
			s.noteBanter(in.GuildID, in.ChannelID, step, "")
		}}
	}
	return true, nil
}

// CallsSomeoneElse reports whether text calls a member of the channel other than its author by name — a
// person the live window has seen speak there. Rooms where the bot answers everything leave such messages
// to the people they are for (LIVE-13).
func (s *Service) CallsSomeoneElse(guildID, channelID, authorID, text string) bool {
	for _, id := range conversation.NamesFrom(s.liveLines(guildID, channelID)).Called(text) {
		if id != authorID {
			return true
		}
	}
	return false
}

// decodeJSONObject reads the first JSON object in a model's text (models wrap JSON in prose or fences).
func decodeJSONObject(text string, v any) bool {
	start := strings.Index(text, "{")
	end := strings.LastIndex(text, "}")
	if start < 0 || end <= start {
		return false
	}
	return json.Unmarshal([]byte(text[start:end+1]), v) == nil
}
