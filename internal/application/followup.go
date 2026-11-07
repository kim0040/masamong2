package application

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode"

	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/conversation"
	"masamong/internal/lang"
)

// The follow-up window (docs/plan-memory-conversation.md §5.2): after the bot answers someone, that person
// often carries on without tagging it again ("그럼 내일은?"). For a short while their next untagged
// message is scored from structure only — did the bot just ask them something, has anyone else spoken,
// is the message aimed at another person — and answered when the score is clear, sent to a small
// classifier when it is borderline, and left alone otherwise.
//
// It is an experimental feature, off in every channel by default (decision D7). Where it is off the
// decisions are still computed and logged (conversation_log, decision "would_answer"/"would_skip"), so the
// thresholds below can be re-tuned from what people actually did before anyone turns it on.
const (
	followAnswerScore   = 3 // answer at or above
	followClassifyScore = 2 // ask the classifier at exactly this score
	followQuestionBonus = 60 * time.Second
	followRetagWindow   = 60 * time.Second
)

type followWindow struct {
	botMsgAt time.Time
	until    time.Time
	asked    bool // the bot's reply ended with a question
	botText  string
	untagged int    // untagged follow-ups answered in a row
	lastID   string // conversation_log id of the last decision (its outcome is noted later)
	lastSkip time.Time
	// lastAnswer is when the last decision was to answer: a tag within followRetagWindow after it is noted
	// ("retagged_after_answer") beside the skipped ones ("retagged"), so the two rates can be compared
	lastAnswer time.Time
}

type followState struct {
	mu   sync.Mutex
	wins map[string]*followWindow // guild:channel:author
}

func followKey(guildID, channelID, authorID string) string {
	return guildID + ":" + channelID + ":" + authorID
}

// openFollowWindow starts (or renews) the window after the bot's reply to a person reached the channel.
func (s *Service) openFollowWindow(in ChatInput, answer string, pol config.ChannelPolicy, untagged bool) {
	if in.GuildID == "" || in.ChannelID == "" || in.SpeakerID == "" || in.DryRun || strings.TrimSpace(answer) == "" {
		return
	}
	now := s.Clock.Now()
	d := pol.Conversation.FollowWindow()
	asked := endsWithQuestion(answer)
	if asked {
		d += followQuestionBonus
	}
	key := followKey(in.GuildID, in.ChannelID, in.SpeakerID)
	f := &s.follow
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.wins == nil {
		f.wins = map[string]*followWindow{}
	}
	w := f.wins[key]
	if w == nil {
		w = &followWindow{}
		f.wins[key] = w
	}
	w.botMsgAt, w.until, w.asked, w.botText = now, now.Add(d), asked, clip(answer, 400)
	if untagged {
		w.untagged++
	} else {
		w.untagged = 0
	}
	// forget closed windows of the channel now and then
	for k, old := range f.wins {
		if now.After(old.until.Add(10 * time.Minute)) {
			delete(f.wins, k)
		}
	}
}

// closeFollowWindow ends a person's window (they turned to someone else).
func (s *Service) closeFollowWindow(guildID, channelID, authorID string) {
	f := &s.follow
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.wins, followKey(guildID, channelID, authorID))
}

// FollowWindowOpen is the gateway's cheap check before it asks FollowUp: does this person have an open
// window here?
func (s *Service) FollowWindowOpen(guildID, channelID, authorID string) bool {
	f := &s.follow
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.wins[followKey(guildID, channelID, authorID)]
	return w != nil && s.Clock.Now().Before(w.until)
}

// NoteDirectCall tells the follow-up log that a person tagged the bot: if their untagged message was just
// left alone, that skip was a missed follow-up (outcome "retagged") — the signal the thresholds are tuned by.
func (s *Service) NoteDirectCall(guildID, channelID, authorID string) {
	f := &s.follow
	f.mu.Lock()
	w := f.wins[followKey(guildID, channelID, authorID)]
	id, outcome := "", ""
	switch {
	case w == nil:
	case !w.lastSkip.IsZero() && s.Clock.Now().Sub(w.lastSkip) <= followRetagWindow:
		id, outcome, w.lastSkip = w.lastID, "retagged", time.Time{}
	case !w.lastAnswer.IsZero() && s.Clock.Now().Sub(w.lastAnswer) <= followRetagWindow:
		id, outcome, w.lastAnswer = w.lastID, "retagged_after_answer", time.Time{}
	}
	f.mu.Unlock()
	if id != "" {
		s.ownedGo("follow outcome", func(context.Context) { s.noteConversationOutcome(id, outcome) })
	}
}

// followFeatures are the structural signals of one candidate message.
type followFeatures struct {
	Asked, NobodyElse, Cue, Elsewhere, NameOther, PeopleTook, Empty, Busy bool
	Others                                                                int
}

// scoreFollow is the score table of §5.2. It reads structure only, so it behaves the same in every
// language; the pack's cue words add one point where a language has them and nothing where it does not.
func scoreFollow(f followFeatures) int {
	score := 2 // the same person, inside the window
	if f.Asked {
		score++ // the bot asked them something (3 in total with the base)
	}
	if f.NobodyElse {
		score++
	}
	if f.Cue {
		score++
	}
	if f.Elsewhere {
		score -= 4
	}
	if f.NameOther {
		score -= 3
	}
	if f.PeopleTook {
		score -= 3
	}
	if f.Empty {
		score -= 2
	}
	if f.Busy {
		score -= 2
	}
	return score
}

// FollowUp decides whether an untagged message from a person with an open window is said to the bot. It
// may call the classifier, so the gateway runs it off its event loop. It logs every decision; it returns
// true only when the channel's follow mode is "window" and the message should be answered.
func (s *Service) FollowUp(ctx context.Context, m ObservedMessage) bool {
	pol := s.channelPolicy(m.GuildID, m.ChannelID)
	key := followKey(m.GuildID, m.ChannelID, m.AuthorID)
	f := &s.follow
	f.mu.Lock()
	w := f.wins[key]
	if w == nil || !s.Clock.Now().Before(w.until) {
		f.mu.Unlock()
		return false
	}
	win := *w
	f.mu.Unlock()
	feat := followFeatures{Asked: win.asked}
	lines := s.liveLines(m.GuildID, m.ChannelID)
	var between []conversation.Line
	for _, l := range lines {
		if l.ID == m.MessageID || l.At.Before(win.botMsgAt) || l.Bot {
			continue
		}
		if l.AuthorID != m.AuthorID {
			between = append(between, l)
			// someone answered the caller: the people took the conversation over
			if l.ReplyTo != "" && authorOfLine(lines, l.ReplyTo) == m.AuthorID || containsString(l.Mentions, m.AuthorID) {
				feat.PeopleTook = true
			}
		}
	}
	feat.Others = len(between)
	feat.NobodyElse = len(between) == 0
	feat.Busy = len(between) >= 3
	botID := s.botAccount(lines)
	if m.MentionsOthers {
		feat.Elsewhere = true
	}
	for _, id := range m.Mentions {
		if id != botID && id != m.AuthorID {
			feat.Elsewhere = true
		}
	}
	if m.ReplyTo != "" {
		if a := authorOfLine(lines, m.ReplyTo); a != "" && a != botID && a != m.AuthorID {
			feat.Elsewhere = true
		}
	}
	feat.NameOther = s.CallsSomeoneElse(m.GuildID, m.ChannelID, m.AuthorID, m.Content)
	feat.Empty = !hasWords(m.Content)
	voice := s.voiceFor(m.GuildID, pol)
	voice.Pack = lang.For(chat.DetectLanguage(m.Content), voice.Pack.Tag)
	feat.Cue = startsWithCue(m.Content, voice.Pack.Conversation.FollowupCues)
	score := scoreFollow(feat)
	mode := pol.Conversation.Follow
	if feat.Elsewhere || feat.NameOther || feat.PeopleTook {
		// turning to someone else ends the window; the next message to the bot needs a tag again
		s.closeFollowWindow(m.GuildID, m.ChannelID, m.AuthorID)
	}
	decision, cost := "skip", 0.0
	switch {
	case score >= followAnswerScore:
		decision = "answer"
	case score == followClassifyScore && mode == "window":
		decision, cost = s.classifyFollow(ctx, m, win.botText, between, voice)
	case score == followClassifyScore:
		decision = "classify" // what would have been asked; not spent where the feature is off
	}
	if win.untagged >= pol.Conversation.MaxUntagged() && decision == "answer" {
		decision = "skip_cap" // a long untagged run: the next one needs a tag (a thread offer is decided separately, after each reply)
	}
	logged := decision
	if mode != "window" {
		logged = "would_" + decision
	}
	id := s.logConversation(convDecision{Guild: m.GuildID, Channel: m.ChannelID, Message: m.MessageID, Author: m.AuthorID,
		Kind: convFollow, Decision: logged, Mode: mode, Score: float64(score), CostUSD: cost,
		Features: map[string]any{"asked": feat.Asked, "nobody_else": feat.NobodyElse, "cue": feat.Cue, "elsewhere": feat.Elsewhere,
			"name_other": feat.NameOther, "people_took": feat.PeopleTook, "empty": feat.Empty, "others": feat.Others,
			"since_ms": s.Clock.Now().Sub(win.botMsgAt).Milliseconds(), "untagged": win.untagged}})
	f.mu.Lock()
	if w := f.wins[key]; w != nil {
		w.lastID = id
		w.lastSkip, w.lastAnswer = time.Time{}, time.Time{}
		if decision != "answer" {
			w.lastSkip = s.Clock.Now()
		} else if mode == "window" { // an answer the mode let through (a would_answer was not sent)
			w.lastAnswer = s.Clock.Now()
		}
	}
	f.mu.Unlock()
	return mode == "window" && decision == "answer"
}

// classifyFollow asks whether a borderline message is said to the bot; anything but "bot" is a skip.
func (s *Service) classifyFollow(ctx context.Context, m ObservedMessage, botText string, between []conversation.Line, voice chat.Voice) (string, float64) {
	c := voice.Pack.Conversation
	routes, err := s.chatRoutes(s.Doc())
	if err != nil || c.Follow == "" {
		return "skip_error", 0
	}
	text, _ := conversation.Numbered(between, voice.BotName, 200)
	if text == "" {
		text = voice.Pack.Chat.None
	}
	msgs := []chat.Msg{
		{Role: "system", Content: lang.Fill(c.FollowSystem, "bot", voice.BotName)},
		{Role: "user", Content: lang.Fill(c.Follow, "bot_last", botText, "between", text, "speaker", m.AuthorName, "message", clip(m.Content, 600))},
	}
	cctx, cancel := context.WithTimeout(ctx, referTimeout)
	defer cancel()
	// a reasoning planner given "low" spent the 40 tokens thinking and answered nothing on half the
	// calls (2026-10-29, gpt-6-luna, 18 of 36), so the other chat model was asked every time
	cctx = withQuickAnswer(withLedgerRole(cctx, "follow"))
	out, err := chatLLM{s: s, routes: routes}.ChatDetailed(cctx, "planner", msgs, 40, 0)
	if err != nil {
		return "skip_error", out.CostUSD
	}
	var reply struct {
		To string `json:"to"`
	}
	if !decodeJSONObject(out.Text, &reply) || reply.To != "bot" {
		return "skip_classified", out.CostUSD
	}
	return "answer", out.CostUSD
}

func authorOfLine(lines []conversation.Line, id string) string {
	for _, l := range lines {
		if l.ID == id {
			return l.AuthorID
		}
	}
	return ""
}

func containsString(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// hasWords: the message has a letter or digit, not only emoji, stickers or punctuation.
func hasWords(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return true
		}
	}
	return false
}

// endsWithQuestion: the reply ends by asking something (any script's question mark).
func endsWithQuestion(s string) bool {
	s = strings.TrimSpace(s)
	for len(s) > 0 {
		r := []rune(s)
		last := r[len(r)-1]
		if unicode.IsPunct(last) && last != '?' && last != '？' && last != '¿' && last != '؟' || unicode.IsSymbol(last) || unicode.IsSpace(last) {
			s = strings.TrimSpace(string(r[:len(r)-1]))
			continue
		}
		return last == '?' || last == '？' || last == '؟'
	}
	return false
}

func startsWithCue(text string, cues []string) bool {
	t := strings.ToLower(strings.TrimSpace(text))
	for _, c := range cues {
		if c = strings.ToLower(strings.TrimSpace(c)); c != "" && strings.HasPrefix(t, c) {
			return true
		}
	}
	return false
}
