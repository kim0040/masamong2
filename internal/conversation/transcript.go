package conversation

import (
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Select keeps the newest lines that fit maxChars of text, dropping the oldest first, but never fewer
// than minLines (when there are that many): the context of a short call usually sits in the two to seven
// lines before it, so those survive even a long tail. It returns the kept lines and how many were left
// out. lines must be in time order.
func Select(lines []Line, maxChars, minLines int) ([]Line, int) {
	if maxChars <= 0 || len(lines) == 0 {
		return lines, 0
	}
	used := 0
	keep := len(lines)
	for i := len(lines) - 1; i >= 0; i-- {
		n := utf8.RuneCountInString(lines[i].Text) + utf8.RuneCountInString(lines[i].Author) + 4
		if used+n > maxChars && len(lines)-i > minLines {
			break
		}
		used += n
		keep = i
	}
	return lines[keep:], keep
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
