// Package episodes turns a room's raw messages into the room's episode memory (L2) and keeps its live
// state (L1): docs/plan-memory-conversation.md §5.4, ADR-17.
//
// Everything here is pure: the segmenter cuts a time-ordered message list into conversation segments, the
// extraction side builds the model prompt and validates what the model answers, the status machine moves
// plans between open/decided/done/dropped/expired, and the state side validates and ages the room state.
// Storage, model calls and invalidation live in internal/application (episodes.go) and the memory store.
//
// Nothing here logs or keeps message text beyond the call: callers pass raw lines in and get IDs, person
// references and short validated sentences out.
package episodes

import (
	"sort"
	"strings"
	"time"

	"masamong/internal/memory/lexical"
)

// Message is one raw message of a room, as the segmenter and the extraction prompt see it.
type Message struct {
	ID      string // the observation ID (evidence is saved as these)
	Account string // the platform account; "" when the platform has none (Kakao)
	Author  string // the name people see
	At      time.Time
	ReplyTo string // the ID (same space as ID) of the message this one replies to; "" for none or unknown
	Bot     bool   // the bot's own line: context only, never evidence
	Text    string
}

// Speaker identifies the author for participant sets: the account, or the label where there is none.
func (m Message) Speaker() string {
	if m.Account != "" {
		return m.Account
	}
	return "label:" + m.Author
}

// Sealed is a segment already extracted. A later run that finds a segment starting inside it treats that
// segment as done (no second extraction) and lets nothing written at or after At join it.
type Sealed struct {
	IDs []string  `json:"ids"`
	At  time.Time `json:"at"`
}

// Options are the segmenter's knobs. The zero value of each field takes the plan's default.
type Options struct {
	HardGap     time.Duration  // a silence this long always cuts (45m)
	SoftGapMin  time.Duration  // silences from this long count towards a soft cut (8m)
	DayBoundary string         // "HH:MM" in Location where one day's talk ends ("05:00": overnight chat stays one day)
	Location    *time.Location // the server's zone for DayBoundary (UTC when nil)
	MaxLines    int            // a segment never grows past this many lines (80)
	CloseIdle   time.Duration  // a segment whose last line is this old is closed (10m)
	CloseLines  int            // a segment with this many lines is closed (60)
	CutScore    float64        // a soft score at or above this starts a new segment (0.7)
	BurstGap    time.Duration  // one person's messages this close together are one line (2m)
	Tokenizer   *lexical.Tokenizer
	Sealed      []Sealed
}

func (o Options) withDefaults() Options {
	if o.HardGap <= 0 {
		o.HardGap = 45 * time.Minute
	}
	if o.SoftGapMin <= 0 {
		o.SoftGapMin = 8 * time.Minute
	}
	if o.SoftGapMin >= o.HardGap {
		o.SoftGapMin = o.HardGap / 2
	}
	if o.DayBoundary == "" {
		o.DayBoundary = "05:00"
	}
	if o.Location == nil {
		o.Location = time.UTC
	}
	if o.MaxLines <= 0 {
		o.MaxLines = 80
	}
	if o.CloseIdle <= 0 {
		o.CloseIdle = 10 * time.Minute
	}
	if o.CloseLines <= 0 {
		o.CloseLines = 60
	}
	if o.CloseLines > o.MaxLines {
		o.CloseLines = o.MaxLines
	}
	if o.CutScore <= 0 {
		o.CutScore = 0.7
	}
	if o.BurstGap <= 0 {
		o.BurstGap = 2 * time.Minute
	}
	if o.Tokenizer == nil {
		t := lexical.DefaultTokenizer()
		o.Tokenizer = &t
	}
	return o
}

// Segment is one conversation: a set of message IDs, not necessarily contiguous in the room (interleaved
// talk that reply links show belongs elsewhere is skipped).
type Segment struct {
	IDs     []string  // in time order
	First   time.Time // the first message
	Last    time.Time // the last message
	Lines   int       // lines after one person's bursts are merged
	Authors []string  // speakers (Message.Speaker) of the human lines, in order of first appearance
	Block   int       // the hard block (between hard cuts) it belongs to, counted from 0
	Closed  bool      // ready to extract: idle, long enough, or its block has ended
	Sealed  bool      // already extracted (Options.Sealed)
}

// DayKey is the calendar day a moment belongs to when days end at boundary ("HH:MM") in loc.
func DayKey(t time.Time, boundary string, loc *time.Location) string {
	if loc == nil {
		loc = time.UTC
	}
	off := time.Duration(0)
	if b, err := time.Parse("15:04", boundary); err == nil {
		off = time.Duration(b.Hour())*time.Hour + time.Duration(b.Minute())*time.Minute
	}
	return t.In(loc).Add(-off).Format("2006-01-02")
}

type segState struct {
	Segment
	sealedAt    time.Time // zero: not sealed
	recentLines [][]string
	recentSpk   []string
	lastSpeaker string
	lastReplied bool // the last line replied to a line of this segment
	members     map[string]bool
	authors     map[string]bool
}

// Split cuts msgs into segments. now decides which segments are closed. Messages need not be sorted;
// the result is in order of each segment's first message. The cut is online: a message's segment depends
// only on the messages before it, so running Split again over the same prefix with more messages
// appended assigns the prefix the same way.
func Split(msgs []Message, now time.Time, opt Options) []Segment {
	opt = opt.withDefaults()
	ms := append([]Message(nil), msgs...)
	sort.SliceStable(ms, func(i, j int) bool { return ms[i].At.Before(ms[j].At) })
	sealedOf := map[string]int{}
	for i, s := range opt.Sealed {
		for _, id := range s.IDs {
			sealedOf[id] = i
		}
	}
	var out []Segment
	block := 0
	var open []*segState
	segOf := map[string]*segState{}
	flush := func(ended bool) {
		for _, s := range open {
			seg := s.Segment
			seg.Closed = ended || !s.sealedAt.IsZero() || now.Sub(seg.Last) >= opt.CloseIdle || seg.Lines >= opt.CloseLines
			seg.Sealed = !s.sealedAt.IsZero()
			out = append(out, seg)
		}
		open = nil
	}
	var prev *Message
	var prevSeg *segState
	for i := range ms {
		m := ms[i]
		if prev != nil && (m.At.Sub(prev.At) >= opt.HardGap || DayKey(m.At, opt.DayBoundary, opt.Location) != DayKey(prev.At, opt.DayBoundary, opt.Location)) {
			flush(true)
			block++
			prevSeg = nil
			segOf = map[string]*segState{}
		}
		attachable := func(s *segState) bool {
			return s != nil && s.Lines < opt.MaxLines && (s.sealedAt.IsZero() || m.At.Before(s.sealedAt))
		}
		words := tokens(opt.Tokenizer, m.Text)
		var target *segState
		newLine := true
		replySeg := segOf[m.ReplyTo]
		switch {
		case prev != nil && prevSeg != nil && !m.Bot && !prev.Bot && m.Speaker() == prev.Speaker() && m.At.Sub(prev.At) < opt.BurstGap &&
			(m.ReplyTo == "" || replySeg == prevSeg) && attachable(prevSeg):
			// one person's burst is one line
			target, newLine = prevSeg, false
		case m.ReplyTo != "" && attachable(replySeg):
			target = replySeg
		case m.Bot:
			if attachable(prevSeg) {
				target = prevSeg
			}
		default:
			best, bestScore := (*segState)(nil), 0.0
			for rank := len(open) - 1; rank >= 0; rank-- {
				s := open[rank]
				if !attachable(s) || m.At.Sub(s.Last) >= opt.HardGap {
					continue
				}
				score := cutScore(s, m, words, opt) + 0.1*float64(len(open)-1-rank)
				if best == nil || score < bestScore {
					best, bestScore = s, score
				}
			}
			if best != nil && bestScore < opt.CutScore {
				target = best
			}
		}
		if target == nil {
			target = &segState{Segment: Segment{First: m.At, Block: block}, members: map[string]bool{}, authors: map[string]bool{}}
			if idx, ok := sealedOf[m.ID]; ok {
				target.sealedAt = opt.Sealed[idx].At
				if target.sealedAt.IsZero() {
					target.sealedAt = m.At.Add(time.Nanosecond)
				}
			}
			open = append(open, target)
		}
		target.IDs = append(target.IDs, m.ID)
		target.Last = m.At
		segOf[m.ID] = target
		if newLine {
			target.Lines++
		}
		if !m.Bot {
			spk := m.Speaker()
			if !target.authors[spk] {
				target.authors[spk] = true
				target.Authors = append(target.Authors, spk)
			}
			target.recentSpk = append(target.recentSpk, spk)
			if len(target.recentSpk) > 8 {
				target.recentSpk = target.recentSpk[len(target.recentSpk)-8:]
			}
			target.lastSpeaker = spk
			if len(words) > 0 {
				target.recentLines = append(target.recentLines, words)
				if len(target.recentLines) > 6 {
					target.recentLines = target.recentLines[len(target.recentLines)-6:]
				}
			}
			target.lastReplied = m.ReplyTo != "" && replySeg == target
		}
		prev, prevSeg = &ms[i], target
	}
	flush(false)
	sort.SliceStable(out, func(i, j int) bool { return out[i].First.Before(out[j].First) })
	return out
}

// cutScore is how strongly m starts something new relative to s: the silence (8–45 min), a speaker who
// was not part of it, a reply chain the message does not continue, and how few of its words it shares.
func cutScore(s *segState, m Message, words []string, opt Options) float64 {
	score := 0.0
	gap := m.At.Sub(s.Last)
	if gap >= opt.SoftGapMin {
		score += 0.6 * min(1, float64(gap-opt.SoftGapMin)/float64(opt.HardGap-opt.SoftGapMin))
	}
	spk := m.Speaker()
	recent := false
	for _, r := range s.recentSpk {
		if r == spk {
			recent = true
			break
		}
	}
	switch {
	case recent:
	case len(s.authors) >= 2:
		score += 0.35 // a conversation between people, and someone else speaks
	default:
		score += 0.15 // one person so far: someone answering them is the usual case
	}
	if s.lastReplied && m.ReplyTo == "" {
		score += 0.2
	}
	switch {
	case len(words) < 2:
		score += 0.15 // too short to tell
	default:
		seen := map[string]bool{}
		for _, line := range s.recentLines {
			for _, w := range line {
				seen[w] = true
			}
		}
		shared := 0
		for _, w := range words {
			if seen[w] {
				shared++
			}
		}
		score += 0.5 * (1 - float64(shared)/float64(len(words)))
	}
	if spk == s.lastSpeaker && gap < opt.BurstGap*2 {
		score -= 0.3
	}
	return score
}

// tokens are the language-appropriate n-grams of a message's discriminating words.
func tokens(t *lexical.Tokenizer, text string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range t.QueryWords(text) {
		for _, g := range strings.Fields(t.ToNgrams(strings.ToLower(w))) {
			if !seen[g] {
				seen[g] = true
				out = append(out, g)
			}
		}
	}
	return out
}
