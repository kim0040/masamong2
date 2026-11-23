package peoplebackfill

import (
	"crypto/sha256"
	"encoding/hex"
	"time"

	"masamong/internal/memory/episodes"
	"masamong/internal/memory/search"
)

// MaxWindowLines is the most merged lines one window carries (the contract's 120).
const MaxWindowLines = 120

// Line is one observation of a room, oldest first. A window is built from one room's lines only:
// nothing here crosses rooms.
type Line struct {
	ID   string // the observation id (claims cite these as evidence)
	At   time.Time
	Ref  string // the author's person reference; lines without one merge as one speaker
	Bot  bool   // the bot's own line: context only, never evidence
	Text string
}

// MergedLine is a run of consecutive lines of one speaker, kept as one line of a window.
type MergedLine struct {
	Ref  string    // the run's speaker ("" for the bot and for lines without a reference)
	Bot  bool      // a run of the bot's own lines
	At   time.Time // the run's first line
	End  time.Time // the run's last line
	Text string    // the run's texts joined by "\n"
	IDs  []string  // the run's observation ids, in order
}

// Window is the unit one model call covers: whole segments joined up to MaxWindowLines merged
// lines. Its claims are written with evidence = observation ids (MergedLine.IDs).
type Window struct {
	ID      string // sha256 of room, first and last observation id, prompt version hash
	Room    string
	Lines   []MergedLine
	FirstID string    // the first observation id
	LastID  string    // the last observation id
	First   time.Time // the first observation's time
	Last    time.Time // the last observation's time
	Skipped bool      // no human merged line reaches the meaningful-rune floor: never sent
}

// Options are BuildWindows' knobs; each zero field takes its documented default.
type Options struct {
	// PromptHash identifies the person_extract prompt version and is part of every window id: a
	// prompt change re-keys the windows, so old results never apply to a new prompt.
	PromptHash string
	// MaxLines is the merged-line limit of a window (MaxWindowLines).
	MaxLines int
	// Now is the segmenter's reference time; zero takes the newest line's time.
	Now time.Time
	// Split tunes the episode segmenter; zero is its defaults.
	Split episodes.Options
}

// BuildWindows cuts room's lines into segments (episodes.Split), merges each segment's
// same-speaker runs, and joins whole segments into windows of at most opt.MaxLines merged lines.
// The lines must all belong to room; windows never cross rooms.
//
// A window whose human merged lines all stay under the message-vector floor
// (search.MinMessageRunes meaningful runes) is marked Skipped: it is counted in the dry run and
// never sent — a segment of only bot lines or only laughter carries no claim evidence.
func BuildWindows(room string, lines []Line, opt Options) []Window {
	if len(lines) == 0 {
		return nil
	}
	maxLines := opt.MaxLines
	if maxLines <= 0 {
		maxLines = MaxWindowLines
	}
	now := opt.Now
	msgs := make([]episodes.Message, 0, len(lines))
	byID := make(map[string]Line, len(lines))
	for _, l := range lines {
		byID[l.ID] = l
		if l.At.After(now) {
			now = l.At
		}
		// the author reference stands in for the account, so Speaker() groups runs by it
		msgs = append(msgs, episodes.Message{ID: l.ID, Account: l.Ref, At: l.At, Bot: l.Bot, Text: l.Text})
	}
	var windows []Window
	var cur []MergedLine
	flush := func() {
		if len(cur) == 0 {
			return
		}
		windows = append(windows, newWindow(room, cur, opt.PromptHash))
		cur = nil
	}
	for _, seg := range episodes.Split(msgs, now, opt.Split) {
		merged := mergeRuns(byID, seg.IDs)
		if len(merged) == 0 {
			continue
		}
		// whole segments join: a segment over the limit becomes a window of its own rather than
		// being cut (the defaults cannot produce one — the segmenter itself caps at 80)
		if len(cur) > 0 && len(cur)+len(merged) > maxLines {
			flush()
		}
		cur = append(cur, merged...)
	}
	flush()
	return windows
}

// mergeRuns collapses each maximal run of consecutive same-speaker lines of the segment into one
// merged line. The bot's lines form runs of their own, marked Bot.
func mergeRuns(byID map[string]Line, ids []string) []MergedLine {
	var out []MergedLine
	for _, id := range ids {
		l, ok := byID[id]
		if !ok {
			continue
		}
		key := l.Ref
		if l.Bot {
			key = "" // every bot line is one speaker
		}
		if n := len(out); n > 0 && out[n-1].Ref == key && out[n-1].Bot == l.Bot {
			m := &out[n-1]
			m.Text += "\n" + l.Text
			m.IDs = append(m.IDs, l.ID)
			m.End = l.At
			continue
		}
		out = append(out, MergedLine{Ref: key, Bot: l.Bot, At: l.At, End: l.At, Text: l.Text, IDs: []string{l.ID}})
	}
	return out
}

func newWindow(room string, lines []MergedLine, promptHash string) Window {
	first, last := lines[0], lines[len(lines)-1]
	w := Window{Room: room, Lines: lines, FirstID: first.IDs[0], LastID: last.IDs[len(last.IDs)-1], First: first.At, Last: last.End}
	sum := sha256.Sum256([]byte(room + "\x00" + w.FirstID + "\x00" + w.LastID + "\x00" + promptHash))
	w.ID = hex.EncodeToString(sum[:])
	for _, l := range lines {
		if !l.Bot && search.MeaningfulRunes(l.Text) >= search.MinMessageRunes {
			return w
		}
	}
	w.Skipped = true
	return w
}
