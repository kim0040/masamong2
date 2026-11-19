package application

import (
	"context"
	"database/sql"
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
	"masamong/internal/memory/store"
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

// The caller's own earlier exchanges with the bot reach further back than the room's talk around them: the last
// ownExchanges of them within ownWindow come in even when the room has moved on (conversation.Options), read from
// the stored conversation like every other line. An older answer of the bot is clipped to ownClipRunes and counts
// in the transcript budget like any line, so it is the first to go when the conversation is long.
const (
	ownWindow    = 6 * time.Hour
	ownExchanges = 3
	ownClipRunes = 400
	// chatTurnsKept is how many rows of a channel's stored conversation are kept (chat_turns); a turn reads them all
	chatTurnsKept = 40
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
	lines, derived, stats := s.channelLines(in, eng)
	botID := s.botAccount(lines)
	found := false
	for _, l := range lines {
		if l.ID == in.MessageID {
			found = true
			break
		}
	}
	if !found {
		// An unobserved call still has its arrival clock. Using the time after
		// admission here would place it after answers published while it waited.
		lines = append(lines, conversation.Line{ID: in.MessageID, AuthorID: in.SpeakerID, Author: in.Speaker, At: s.callCutoff(in),
			Text: in.Message, Mentions: []string{botID}})
	}
	att := conversation.Attribute(lines, []string{in.MessageID}, in.SpeakerID,
		conversation.Options{BotID: botID, Names: conversation.NamesFrom(lines), OwnWindow: ownWindow, OwnExchanges: ownExchanges})
	out := convContext{ledger: map[string]any{"mode": mode, "included": len(att.Included), "excluded": att.Excluded,
		"candidates": len(att.Candidates)}}
	stats.note(out.ledger, att)
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
	pastTarget := false // picked is in delivery order, including ties
	for _, l := range picked {
		if l.ID == att.CurrentAnswer {
			pastTarget = true
		}
		if l.ID != in.MessageID && strings.TrimSpace(l.Text) != "" {
			current := l.ID == att.CurrentAnswer || (l.ReplyTo != "" && l.ReplyTo == att.CurrentRequest)
			if l.Bot && !current && (!pastTarget || l.At.Before(att.WindowStart)) {
				l.Text = clip(l.Text, ownClipRunes)
			} else if req := derived[l.ID]; req != nil && att.Reasons[l.ID] == "own" && l.RevisedAt.IsZero() {
				// Outside the room window only the last fragment is selected. Use the
				// complete request the turn reader cleared, with its direct sources.
				l.Text = req.content
				req.used = true
			}
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
			if req := derived[id]; req != nil && req.used {
				out.sources = append(out.sources, req.sources...)
			}
		}
	}
	return out, true
}

// lineStats counts, body-free, where a turn's lines came from, for the attribution ledger: a conversation that
// came out short can be traced to the source that was missing (a live window emptied by a restart, stored turns
// held back while a change is pending, a bot message that was not joined to its stored answer).
type lineStats struct {
	stored          int  // stored room messages
	turnUser        int  // stored requests with no stored message or live line of their own
	turnUserMerged  int  // stored requests that marked a line already there as said to the bot
	turnBot         int  // stored answers
	turnsRead       int  // chat_turns rows read
	turnsGated      bool // no stored turns: a change of a source is pending
	live            int  // live window lines put
	liveBot         int  // the bot's live messages kept as lines of their own
	liveBotAnswered int  // the bot's live messages left out because their stored answer stands for them
	liveAge         time.Duration
	replyLinks      int // replies to a posted message joined to the stored answer it was part of
	postedErrors    int
}

func (st lineStats) note(ledger map[string]any, att conversation.Attribution) {
	ledger["sources"] = map[string]int{"stored": st.stored, "turn_user": st.turnUser, "turn_user_merged": st.turnUserMerged,
		"turn_bot": st.turnBot, "live": st.live, "live_bot": st.liveBot, "live_bot_answered": st.liveBotAnswered}
	ledger["turns_read"] = st.turnsRead
	if st.turnsGated {
		ledger["turns_gated"] = true
	}
	ledger["live_age_s"] = int(st.liveAge.Seconds())
	ledger["reply_links"] = st.replyLinks
	if st.postedErrors != 0 {
		ledger["posted_lookup_errors"] = st.postedErrors
	}
	ledger["before_window"], ledger["after_call"], ledger["caller_earlier"] = att.Before, att.After, att.CallerEarlier
}

// channelLines merges what is known of the channel's recent messages: stored messages (capture on), the
// stored conversation with the bot (its answers keep exactly what reached the channel) and the live window
// (reply and mention links, the bot's own posts), one line per message. Derived requests keep the
// gated reader's text and provenance together (a burst, a picture's description).
func (s *Service) channelLines(in ChatInput, eng *engine.Engine) (lines []conversation.Line, derived map[string]*derivedRequest, st lineStats) {
	derived = map[string]*derivedRequest{}
	byID := map[string]conversation.Line{}
	var order []string
	put := func(l conversation.Line) {
		if l.ID == "" {
			return
		}
		if old, ok := byID[l.ID]; !ok {
			order = append(order, l.ID)
		} else if l.RevisedAt.IsZero() {
			l.ToBot = l.ToBot || old.ToBot // what one source knows of the call, another may not
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
			st.stored++
		}
	}
	answered := map[string]string{} // user messages the stored conversation has the bot's answer to: its line
	byDelivery, legacy := map[string]string{}, map[string]string{}
	full := map[string]string{} // gated answer text for replies to a particular continuation span
	cutoff := s.callCutoff(in)
	requests := map[string]bool{}
	if turns, err := eng.Store.RecentChatTurns(blocks.ChannelKey(in.GuildID, in.ChannelID), chatTurnsKept); err == nil {
		if err := s.timeChatTurns(in, turns); err != nil {
			st.postedErrors++
		}
		st.turnsRead = len(turns)
		st.turnsGated = len(turns) == 0 && eng.Store.DB.HasPendingEvents("source-change")
		for _, t := range turns {
			if t.NativeID == "" {
				continue
			}
			at, _ := time.Parse(time.RFC3339Nano, t.CreatedAt)
			if spoken, err := time.Parse(time.RFC3339Nano, t.SpokenAt); err == nil {
				at = spoken
			}
			switch t.Role {
			case "user":
				requests[t.NativeID] = true
				// a request the bot answered was said to it, however it was called
				if len(t.DerivedFrom) > 0 {
					_, raw := byID[t.NativeID]
					derived[t.NativeID] = &derivedRequest{content: t.Content, sources: t.SourceIDs, used: !raw}
				}
				l, exists := byID[t.NativeID]
				if !exists {
					l = conversation.Line{ID: t.NativeID, AuthorID: t.SpeakerID, Author: t.Speaker, At: at, Text: t.Content, Mentions: []string{"bot"}}
					st.turnUser++
				}
				wasToBot := l.ToBot
				l.ReplyToBot = t.CallAddress == store.CallReply
				l.ToBot = true
				if t.CallAddress == store.CallEdited {
					editedAddress(&l, t.Content, s.botAccount(nil))
				}
				if exists {
					byID[l.ID] = l
					if !wasToBot && l.ToBot {
						st.turnUserMerged++
					}
				} else {
					put(l)
				}
			case "assistant":
				// named by its own link, not by the request's ID: reading the reply is not reading the request
				answered[t.NativeID] = "turn:" + t.SourceIDs[0]
				full[answered[t.NativeID]] = t.Content
				if t.DeliveryTurn == "" {
					legacy[t.NativeID] = answered[t.NativeID]
				} else {
					byDelivery[t.DeliveryTurn] = answered[t.NativeID]
				}
				visible := visibleDelivery(t.Content, t.DeliverySpans, cutoff)
				put(conversation.Line{ID: answered[t.NativeID], Bot: true, ReplyTo: t.NativeID, At: at, Text: visible, Withdrawn: visible == "" && t.Content != ""})
				st.turnBot++
			}
		}
	}
	live := s.liveLines(in.GuildID, in.ChannelID)
	if len(live) > 0 {
		st.liveAge = s.Clock.Now().Sub(live[0].At)
	}
	// The bot's answer was posted as one or more messages (bubbles, parts of a long reply, its rest), and only
	// the first replies to the request. Each of them is the stored answer's line: a person who replies to any of
	// them replies to that line. The posted-message record names its request.
	var probe []string
	for _, l := range live {
		if l.Bot {
			probe = append(probe, l.ID)
		}
		if l.ReplyTo != "" {
			probe = append(probe, l.ReplyTo)
		}
	}
	for _, id := range order {
		if r := byID[id].ReplyTo; r != "" {
			probe = append(probe, r)
		}
	}
	posted, err := s.postedDetails(probe)
	if err != nil {
		st.postedErrors++
	}
	alias := map[string]string{} // a posted message → the stored answer's line
	for id, post := range posted {
		line := byDelivery[post.turn]
		if line == "" {
			line = legacy[post.request]
		}
		if line != "" && post.request != "" && byID[line].ReplyTo == post.request {
			alias[id] = line
		}
	}
	for _, l := range live {
		if l.Bot {
			if err != nil {
				continue // an unreadable registry cannot clear a raw post for replay
			}
			request := l.ReplyTo
			post, registered := posted[l.ID]
			if l.RegistryGap && !registered {
				continue
			}
			if registered {
				if alias[l.ID] != "" {
					st.liveBotAnswered++
					continue
				}
				if post.request == "" || requests[post.request] || answered[post.request] != "" {
					continue // not cleared, refused, or belongs to a different surviving delivery
				}
				// A cleared exchange can age out of the 40-row store while its post
				// is still live. The usual redaction check still governs its words.
				l.ReplyTo = post.request
			} else if line, ok := answered[request]; ok {
				alias[l.ID] = line
				st.liveBotAnswered++
				continue // the stored answer has the whole reply; its posted parts would repeat it
			} else if requests[request] {
				continue // only the user turn was kept; a raw bot reply cannot restore its missing answer
			}
		}
		if l.Bot && !l.Withdrawn && s.replyWithdrawn(l.ID) {
			l.Text, l.Withdrawn = "", true
		}
		if old, ok := byID[l.ID]; ok && l.Text == "" && old.Text != "" && !l.Bot {
			l.Text = old.Text // consent is checked when the window is filled; the store checks its own
		} else if req := derived[l.ID]; req != nil {
			req.used = false // the live line supplies its own words
		}
		put(l)
		st.live++
		if l.Bot {
			st.liveBot++
		}
	}
	target := byID[in.MessageID].ReplyTo
	if post, ok := posted[target]; ok && post.start > 0 {
		answer := byID[alias[target]]
		content := full[answer.ID]
		if answer.ID != "" && post.start < post.end && post.end <= len(content) {
			// A reply itself proves its target was visible even with no transport
			// clock (registration may be later). Never expose a later part this way.
			prefix := content[:post.end]
			if len([]rune(prefix)) <= ownClipRunes {
				answer.Text = prefix
			} else {
				answer.Text = content[post.start:post.end]
			}
			byID[answer.ID] = answer
		}
	}
	out := make([]conversation.Line, 0, len(order))
	bot := s.botAccount(nil)
	for _, id := range order {
		l := byID[id]
		if line, ok := alias[l.ReplyTo]; ok {
			l.ReplyTo = line
			st.replyLinks++
		}
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
	return out, derived, st
}

type derivedRequest struct {
	content string
	sources []string
	used    bool // this line got its words from the stored turn
}

// postedFor maps the bot's posted messages among ids to the request their turn answered (the turn's own message),
// or "" when the delivery did not clear the answer for replay. Unregistered messages are left out.
func (s *Service) postedFor(ids []string) (map[string]string, error) {
	details, err := s.postedDetails(ids)
	out := make(map[string]string, len(details))
	for id, post := range details {
		out[id] = post.request
	}
	return out, err
}

type postedDetail struct {
	request, turn string
	start, end    int
}

func (s *Service) postedDetails(ids []string) (map[string]postedDetail, error) {
	ids = uniqueStrings(ids)
	if len(ids) == 0 {
		return nil, nil
	}
	db, err := s.adminDB()
	if err != nil {
		return nil, err
	}
	args := make([]any, len(ids))
	for i, id := range ids {
		args[i] = id
	}
	// Only posts from before request_id existed consult the diagnostic record.
	rows, err := db.Reader().Query(`SELECT r.message_id, `+postedRequestSQL(db.IsSQLite())+`,r.turn_id,r.rest_start,r.part_end FROM reply_messages r
		LEFT JOIN turn_log t ON r.request_id IS NULL AND t.id=r.turn_id
		WHERE r.message_id IN (`+strings.TrimSuffix(strings.Repeat("?,", len(ids)), ",")+`)`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]postedDetail{}
	for rows.Next() {
		var id string
		var post postedDetail
		if err := rows.Scan(&id, &post.request, &post.turn, &post.start, &post.end); err != nil {
			return nil, err
		}
		if post.request != id {
			out[id] = post
		}
	}
	return out, rows.Err()
}

// replyWithdrawn reports whether a bot message posted a reply that is not to be built on: its turn's record
// was emptied because what the reply read changed, or there is no record yet to say otherwise (a reply posted
// is recorded just after its turn gives up its place). liveBlankReplies cannot reach a post that arrives after
// the change. A message that is not a reply is not this function's to judge.
func (s *Service) replyWithdrawn(messageID string) bool {
	db, err := s.adminDB()
	if err != nil {
		return true
	}
	var withdrawn sql.NullBool
	err = db.Reader().QueryRow(`SELECT t.id IS NULL OR COALESCE(json_extract(t.body,'$.evidence_redacted'),0) OR COALESCE(json_extract(t.body,'$.message_deleted'),0)
		FROM reply_messages r LEFT JOIN turn_log t ON t.id=r.turn_id WHERE r.message_id=?`, messageID).Scan(&withdrawn)
	if errors.Is(err, sql.ErrNoRows) {
		return false
	}
	return err != nil || withdrawn.Bool
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
