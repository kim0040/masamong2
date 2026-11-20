package conversation

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// cutSnap is the boundary transcript cuts snap to, in lines: the earliest start that fits the budget
// is moved forward to a multiple of it (SnapCut), so consecutive turns of a growing transcript keep
// the same first line until the budget forces the next boundary — the hysteresis a provider's prompt
// cache needs. Select uses it for the rune budget and chat's answer builder for its line cap, so the
// two limits cut the same way.
const cutSnap = 16

// SnapCut snaps an earliest start index forward to a cutSnap boundary. The boundary is a property of
// the index alone, never of the newest line or the clock, so the choice is deterministic and
// stateless: a transcript that grew between two calls cuts at the same boundary until its tail no
// longer fits.
func SnapCut(earliest int) int {
	return (earliest + cutSnap - 1) / cutSnap * cutSnap
}

// Select keeps the newest lines that fit maxChars of text, dropping the oldest first, but never fewer
// than minLines (when there are that many): the context of a short call usually sits in the two to seven
// lines before it, so those survive even a long tail. It returns the kept lines and how many were left
// out. lines must be in time order.
//
// Once the transcript is over budget the earliest start that fits is snapped forward to a cutSnap
// boundary, so two consecutive turns of a growing transcript choose the same first line and the
// prompt built on them keeps a stable prefix the provider's prompt cache can reuse. The boundary may
// leave fewer lines than the budget allowed — that headroom is what the following turns spend before
// the cut must move. When the boundary would leave fewer than the minimum lines, the minimum is kept
// instead.
func Select(lines []Line, maxChars, minLines int) ([]Line, int) {
	if maxChars <= 0 || len(lines) == 0 {
		return lines, 0
	}
	size := func(i int) int {
		return utf8.RuneCountInString(lines[i].Text) + utf8.RuneCountInString(lines[i].Author) + 4
	}
	total := 0
	for i := range lines {
		total += size(i)
	}
	if total <= maxChars {
		return lines, 0
	}
	earliest := len(lines)
	for used, i := 0, len(lines)-1; i >= 0; i-- {
		if used += size(i); used > maxChars {
			earliest = i + 1
			break
		}
	}
	c := min(SnapCut(earliest), len(lines)-minLines)
	if c < 0 {
		c = 0
	}
	return lines[c:], c
}

// Numbered lays lines out for a classifier or an extraction prompt: one line per message as
// "[m3] Name ↩m1: text", where ↩ names the line it replies to when that line is shown too. The arrows
// and labels are the same in every language. It returns the text and the label → message ID map, so the
// model answers with labels and the code maps them back (a model never sees or invents platform IDs).
func Numbered(lines []Line, botName string, maxRunes int) (string, map[string]string) {
	labels := make(map[string]string, len(lines))
	ids := make(map[string]string, len(lines))
	for i, l := range lines {
		label := "m" + strconv.Itoa(i+1)
		labels[l.ID] = label
		ids[label] = l.ID
	}
	var b strings.Builder
	var prev time.Time
	for _, l := range lines {
		name := l.Author
		if l.Bot {
			name = botName
		}
		b.WriteString("[" + labels[l.ID] + "] ")
		if !prev.IsZero() && l.At.Sub(prev) >= 5*time.Minute {
			b.WriteString("(+" + strconv.Itoa(int(l.At.Sub(prev).Minutes())) + "m) ")
		}
		b.WriteString(name)
		if t, ok := labels[l.ReplyTo]; ok {
			b.WriteString(" ↩" + t)
		}
		b.WriteString(": ")
		b.WriteString(clipRunes(strings.ReplaceAll(strings.TrimSpace(l.Text), "\n", " "), maxRunes))
		b.WriteString("\n")
		prev = l.At
	}
	return strings.TrimRight(b.String(), "\n"), ids
}

func clipRunes(s string, n int) string {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n]) + "…"
}
