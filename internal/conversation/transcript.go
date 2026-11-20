package conversation

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// cutGap is the silence that opens a new stretch of talk — the same gap the answer prompt marks a
// line's clock time on (chat's lineAgeAfter). A transcript cut only ever lands on such a boundary.
const cutGap = 5 * time.Minute

// Select keeps the newest lines that fit maxChars of text, dropping the oldest first, but never fewer
// than minLines (when there are that many): the context of a short call usually sits in the two to seven
// lines before it, so those survive even a long tail. It returns the kept lines and how many were left
// out. lines must be in time order.
//
// Once the transcript is over budget the cut is made with hysteresis so two consecutive turns of a
// growing transcript choose the same first line and the prompt built on them keeps a stable prefix the
// provider's prompt cache can reuse. The tentative cut keeps about 70% of the budget — headroom the
// following turns spend before the cut has to move. It is then snapped onto a stretch boundary (a line
// that comes cutGap or more after the line before it): forward to the next stretch start while the
// minimum lines survive, else back to the start of the stretch the cut is in while its whole rest fits
// the budget — the kept tail then refills the budget before the cut must move. The boundary is decided
// from the lines alone, never from the clock or from the last cut, so the choice is deterministic and
// stateless; when neither boundary fits, the tentative cut stands.
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
	used := 0
	keep := len(lines)
	limit := maxChars * 7 / 10
	for i := len(lines) - 1; i >= 0; i-- {
		n := size(i)
		if used+n > limit && len(lines)-i > minLines {
			break
		}
		used += n
		keep = i
	}
	c := keep
	for c < len(lines)-minLines && !stretchStart(lines, c) {
		c++
	}
	if !stretchStart(lines, c) {
		for c = keep; c > 0 && !stretchStart(lines, c); c-- {
		}
		for n, s := 0, c; s < len(lines); s++ {
			if n += size(s); n > maxChars {
				c = keep
				break
			}
		}
	}
	return lines[c:], c
}

// stretchStart reports whether line i opens a stretch of talk: it is the first line or it comes cutGap
// or more after the line before it.
func stretchStart(lines []Line, i int) bool {
	if i <= 0 {
		return true
	}
	return lines[i].At.Sub(lines[i-1].At) >= cutGap
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
