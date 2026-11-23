package application

import (
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"masamong/internal/conversation"
	"masamong/internal/lang"
	"masamong/internal/memory/store"
)

// The thread rules that need no I/O: the offer track of a caller's conversation, sweeping and trimming the
// in-memory thread table (with threadState.mu held), and the facts a thread offer is decided on.

// offerTrack is one caller's conversation in a channel, for the offer score.
type offerTrack struct {
	turns         []time.Time
	chars         int
	offered       bool
	declinedUntil time.Time
	offerTurn     string // the turn whose reply carried the offer (a "keep it here" is noted on it)
	backedOff     bool   // skip_backoff is logged once per conversation, not once per reply
}

// threadTrackIdle: a track this quiet is a finished conversation, and its "no" has run out too.
const threadTrackIdle = time.Hour

// note adds a turn and returns how many of the track's turns are within threadOfferGap: only those count
// for the score, so only those are kept.
func (tr *offerTrack) note(now time.Time) int {
	kept := tr.turns[:0]
	for _, at := range tr.turns {
		if now.Sub(at) <= threadOfferGap {
			kept = append(kept, at)
		}
	}
	tr.turns = append(kept, now)
	return len(tr.turns)
}

// sweepTracks forgets the tracks of conversations quiet for threadTrackIdle and the offers older than an
// hour (the lock held).
func (t *threadState) sweepTracks(now time.Time) {
	for key, tr := range t.tracks {
		if n := len(tr.turns); (n == 0 || now.Sub(tr.turns[n-1]) > threadTrackIdle) && !now.Before(tr.declinedUntil) {
			delete(t.tracks, key)
		}
	}
	for ch, list := range t.offersAt {
		kept := list[:0]
		for _, at := range list {
			if now.Sub(at) < time.Hour {
				kept = append(kept, at)
			}
		}
		if len(kept) == 0 {
			delete(t.offersAt, ch)
		} else {
			t.offersAt[ch] = kept
		}
	}
}

func callers(attribution map[string]any) int {
	n, _ := attribution["callers"].(int)
	return n
}

// trimKnown brings the thread table down to threadMaxKnown by letting go of the quietest threads that
// are not the bot's own open ones (the lock held). Their rows stay.
func (t *threadState) trimKnown(now time.Time) {
	over := len(t.threads) - threadMaxKnown
	if over <= 0 {
		return
	}
	type quiet struct {
		id string
		at time.Time
	}
	var drop []quiet
	for id, info := range t.threads {
		if !(info.BotOwned && info.Open) {
			drop = append(drop, quiet{id, info.LastAt})
		}
	}
	sort.Slice(drop, func(i, j int) bool { return drop[i].at.Before(drop[j].at) })
	for i := 0; i < over && i < len(drop); i++ {
		delete(t.threads, drop[i].id)
	}
}

// offerSignals are what a caller's conversation is scored on after a reply: the turns within
// threadOfferGap, the characters of the bot's replies in it and of this one, the tools this reply used,
// the lines attribution left out, and the channel's lines in the last minute.
type offerSignals struct {
	turns, chars, answer, tools, excluded, busy int
}

// score is the offer score; threadOfferScore or more makes an offer worth deciding.
func (sig offerSignals) score() int {
	score := 0
	if sig.turns >= 6 {
		score += 2
	}
	if sig.chars >= 1200 || sig.answer >= 600 {
		score += 2
	}
	if sig.tools >= 3 {
		score += 2
	}
	if sig.excluded >= 4 {
		score += 2
	}
	if sig.busy >= 10 {
		score++
	}
	return score
}

// busyLines counts the channel's lines from the last minute.
func busyLines(lines []conversation.Line, now time.Time) int {
	busy := 0
	for _, l := range lines {
		if now.Sub(l.At) <= time.Minute {
			busy++
		}
	}
	return busy
}

// offerFacts are what a thread offer is decided on once the conversation is scored: the channel's thread
// mode, the conversation's track (already offered, declined and still resting, backed off and already
// logged as such), the channel's offers in the last hour, how many people talk to the bot together, and
// the tools the reply used.
type offerFacts struct {
	score                                 int
	mode                                  string
	offered, declined, backoff, backedOff bool
	offersThisHour, callers, tools        int
}

// decide is the offer decision ("" below the score: nothing is logged) and the track's backedOff after it:
// a backed-off channel's skip is logged once per conversation.
func (f offerFacts) decide() (decision string, backedOff bool) {
	backedOff = f.backedOff
	switch {
	case f.score < threadOfferScore:
	case f.mode == "off":
		decision = "would_offer"
	case f.offered:
		decision = "skip_offered"
	case f.declined:
		decision = "skip_declined"
	case f.backoff:
		if !f.backedOff {
			decision = "skip_backoff"
		}
		backedOff = true
	case f.offersThisHour >= threadOffersPerHour:
		decision = "skip_rate"
	case f.callers >= 2:
		decision = "skip_group" // several people talk to the bot together: watching is the point
	case f.mode == "auto_task" && f.tools >= 3:
		decision = "auto"
	default:
		decision = "offer"
	}
	return decision, backedOff
}

// aloneIn reports that nobody but authorID has spoken in a thread.
func aloneIn(humans map[string]bool, authorID string) bool {
	alone := true
	for id := range humans {
		if id != authorID {
			alone = false
		}
	}
	return alone
}

// threadStopText is a message as a stop cue is compared: lower case, without a leading "!" or "/" and
// without trailing punctuation, spaces and symbols.
func threadStopText(text string) string {
	t := strings.ToLower(strings.TrimSpace(text))
	t = strings.TrimLeft(t, "!/")
	return strings.TrimRightFunc(t, func(r rune) bool { return unicode.IsPunct(r) || unicode.IsSpace(r) || unicode.IsSymbol(r) })
}

// threadStopCueIn reports that t (threadStopText) is one of the packs' thread stop cues.
func threadStopCueIn(t string, packs ...*lang.Pack) bool {
	for _, p := range packs {
		for _, c := range p.Thread.StopCues {
			if strings.ToLower(c) == t {
				return true
			}
		}
	}
	return false
}

// threadLines are stored turns as conversation lines for a thread summary, IDs prefix plus the turn's
// index, all at now, the bot's turns under botName. skip (when set) leaves a turn out; the others keep
// their index.
func threadLines(turns []store.ChatTurn, prefix, botName string, now time.Time, skip func(store.ChatTurn) bool) []conversation.Line {
	var lines []conversation.Line
	for i, turn := range turns {
		if skip != nil && skip(turn) {
			continue
		}
		l := conversation.Line{ID: prefix + strconv.Itoa(i), Author: turn.Speaker, Text: turn.Content, At: now}
		if turn.Role == "assistant" {
			l.Bot, l.Author = true, botName
		}
		lines = append(lines, l)
	}
	return lines
}
