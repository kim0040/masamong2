package application

import (
	"context"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"masamong/internal/chat"
	"masamong/internal/lang"
)

// Implicit signals: what a person does right after one of the bot's replies, noted on that reply's turn in
// turn_feedback as source "implicit:<kind>", beside the ratings people give on purpose (feedback.go).
//
//	recall          the same person calls the bot again within implicitRecallWindow of the reply
//	thanks, laugh   a short message of theirs that says thanks / laughs (the pack's thanks_cues, laugh_cues)
//	correction      a short message that says the reply was off (the pack's correction_cues)
//	retag           they tag the bot right after an untagged message of theirs was left alone (follow-up)
//	thread_declined they pressed "keep it here" on the thread offer that came with the reply
//
// These are hints for picking which turns to look at, nothing more. They are NOT a score and never raise an
// alert, steer the bot, mute a channel or count as good or bad: a recall is mostly a conversation going on
// (51% of replies in the 2026-11 ops log were followed by one within two minutes), a "ㅋㅋ" may laugh at a
// friend, and a word list misses most of what people mean. So every implicit row has rating 0, the console
// shows it apart from ratings people gave (and the summary counts it apart), and nothing reads it except a
// person choosing turns to review.
//
// Stored like the ratings: the turn id, the one-way hash of the person, the kind. No message text and no
// account ID. Kept only for people feedbackAllowed lets through (memory consent given, not blocked), so a
// person who withdrew consent leaves nothing, and what was kept goes with forgetPerson and retention.
//
// The reply path is not slowed: remembering a reply is one map write; Observe does map lookups and
// substring checks only for a person with a reply in the last implicitCueWindow; the database write runs in
// the background.
const (
	implicitSource = "implicit:"
	// recall is the same person calling again soon: the window a reply is read and answered in.
	implicitRecallWindow = 2 * time.Minute
	// cues are looked for in the next few minutes only; a later "thanks" is about something else.
	implicitCueWindow = 5 * time.Minute
	// A long message is a new request, not a reaction.
	implicitCueMaxRunes = 120
	// reply memory is bounded: past this many callers, the oldest are dropped.
	implicitKeptReplies = 512
)

// ImplicitKinds are the kinds a row can have after "implicit:" (the console labels them turns.implicit.*).
var ImplicitKinds = []string{"recall", "thanks", "laugh", "correction", "retag", "thread_declined"}

type implicitReply struct {
	turnID string
	at     time.Time
	fired  map[string]bool // kinds already noted for this reply
}

type implicitState struct {
	mu   sync.Mutex
	last map[string]*implicitReply // guild:channel:person → their latest reply
}

// implicitNoteReply remembers the reply just delivered to a person (the turn the next signals belong to).
func (s *Service) implicitNoteReply(in ChatInput) {
	if in.turnID == "" || in.GuildID == "" || in.ChannelID == "" || in.SpeakerID == "" || in.DryRun {
		return
	}
	now := s.Clock.Now()
	st := &s.implicit
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.last == nil {
		st.last = map[string]*implicitReply{}
	}
	for k, r := range st.last {
		if now.Sub(r.at) > implicitCueWindow {
			delete(st.last, k)
		}
	}
	for len(st.last) >= implicitKeptReplies {
		oldest, at := "", now
		for k, r := range st.last {
			if r.at.Before(at) {
				oldest, at = k, r.at
			}
		}
		if oldest == "" {
			break
		}
		delete(st.last, oldest)
	}
	st.last[followKey(in.GuildID, in.ChannelID, in.SpeakerID)] = &implicitReply{turnID: in.turnID, at: now}
}

// implicitObserve reads one message seen in a channel (Observe) for what it says about the person's
// latest reply. It runs on the gateway's event loop: no I/O here.
func (s *Service) implicitObserve(m ObservedMessage) {
	if m.FromBot || m.AuthorID == "" || m.GuildID == "" {
		return
	}
	key := followKey(m.GuildID, m.ChannelID, m.AuthorID)
	now := s.Clock.Now()
	st := &s.implicit
	st.mu.Lock()
	r := st.last[key]
	if r == nil {
		st.mu.Unlock()
		return
	}
	age := now.Sub(r.at)
	if age > implicitCueWindow {
		delete(st.last, key)
		st.mu.Unlock()
		return
	}
	turn := r.turnID
	var kinds []string
	note := func(kind string) {
		if !r.fired[kind] {
			if r.fired == nil {
				r.fired = map[string]bool{}
			}
			r.fired[kind] = true
			kinds = append(kinds, kind)
		}
	}
	if m.BotCall != "" && age <= implicitRecallWindow {
		note("recall")
	}
	st.mu.Unlock()
	if m.BotCall == "mention" && s.followSkippedJustNow(key, now) {
		st.mu.Lock()
		note("retag")
		st.mu.Unlock()
	}
	if !m.ToSomeone && utf8.RuneCountInString(m.Content) <= implicitCueMaxRunes {
		pack := s.voiceFor(m.GuildID, s.channelPolicy(m.GuildID, m.ChannelID)).Pack
		other := lang.For(chat.DetectLanguage(m.Content), pack.Tag)
		text := strings.ToLower(m.Content)
		for _, c := range []struct {
			kind string
			cues func(*lang.Pack) []string
		}{
			{"thanks", func(p *lang.Pack) []string { return p.Features.ThanksCues }},
			{"laugh", func(p *lang.Pack) []string { return p.Features.LaughCues }},
			{"correction", func(p *lang.Pack) []string { return p.Features.CorrectionCues }},
		} {
			if hasCue(text, c.cues(pack)) || (other != pack && hasCue(text, c.cues(other))) {
				st.mu.Lock()
				note(c.kind)
				st.mu.Unlock()
			}
		}
	}
	for _, kind := range kinds {
		s.implicitWrite(m.GuildID, turn, m.AuthorID, kind)
	}
}

// followSkippedJustNow reads (never changes) whether the follow-up window left this person's untagged
// message alone a moment ago. NoteDirectCall, which settles that skip's outcome, runs after Observe.
func (s *Service) followSkippedJustNow(key string, now time.Time) bool {
	f := &s.follow
	f.mu.Lock()
	defer f.mu.Unlock()
	w := f.wins[key]
	return w != nil && !w.lastSkip.IsZero() && now.Sub(w.lastSkip) <= followRetagWindow
}

// implicitWrite notes a signal on a turn, in the background and only for a person feedback may be kept for.
func (s *Service) implicitWrite(guildID, turnID, userID, kind string) {
	if turnID == "" || userID == "" {
		return
	}
	s.ownedGo("implicit feedback", func(context.Context) {
		if !s.feedbackAllowed(guildID, userID) {
			return
		}
		s.putFeedback(turnID, userID, implicitSource+kind, 0, "")
	})
}

// hasCue reports whether text (lower case) holds one of the cues. A cue that begins and ends with a
// letter or digit ("lol", "thanks") must stand as a word, so "lollipop" is not a laugh.
func hasCue(text string, cues []string) bool {
	for _, c := range cues {
		c = strings.ToLower(strings.TrimSpace(c))
		if c == "" {
			continue
		}
		first, _ := utf8.DecodeRuneInString(c)
		last, _ := utf8.DecodeLastRuneInString(c)
		asWord := isASCIIAlnum(first) && isASCIIAlnum(last)
		for from := 0; ; {
			i := strings.Index(text[from:], c)
			if i < 0 {
				break
			}
			start, end := from+i, from+i+len(c)
			from = start + 1
			if asWord {
				before, _ := utf8.DecodeLastRuneInString(text[:start])
				after, _ := utf8.DecodeRuneInString(text[end:])
				if start > 0 && (unicode.IsLetter(before) || unicode.IsDigit(before)) || end < len(text) && (unicode.IsLetter(after) || unicode.IsDigit(after)) {
					continue
				}
			}
			return true
		}
	}
	return false
}

func isASCIIAlnum(r rune) bool {
	return r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r))
}
