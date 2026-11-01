package chat

import (
	"strings"
	"unicode/utf8"
)

// A cheap model can fall into a loop and write the same passage over and over until the output limit
// (Hermes Agent delivered one such 60k-character turn as 31 Discord messages; its repetition_guard.py
// is where these thresholds come from). Asking such a model to continue only buys more of the loop,
// so a looping answer is cut after its first round instead.
const (
	loopMinRunes = 400 // shorter answers are never checked
	loopWindow   = 60  // the passage compared, in runes
	loopRepeats  = 5   // how often it must come back
	// loopDistinct exempts short-period repeats ("ㅋㅋㅋㅋ", "!!!!"): a window needs this many different
	// characters to count as a passage
	loopDistinct = 10
)

// cutLoop returns text up to where it starts repeating itself, and whether it did. A passage counts as
// a loop when it comes back loopRepeats times in a row, one copy right after another, and the copies
// span at least half the text. Copies apart from each other (the same link under several headings, rows
// of a table that share a long prefix) are not a loop.
func cutLoop(text string) (string, bool) {
	r := []rune(text)
	if len(r) < loopMinRunes {
		return text, false
	}
	for i := 0; i+loopWindow <= len(r); i += loopWindow / 2 {
		w := string(r[i : i+loopWindow])
		if distinctRunes(w) < loopDistinct || countOverlapping(text, w) < loopRepeats {
			continue
		}
		first, last := strings.Index(text, w), strings.LastIndex(text, w)
		if utf8.RuneCountInString(text[first:last+len(w)])*2 < len(r) {
			continue
		}
		// the loop's period is the distance to the window's next copy; its first round starts where the
		// text stops matching itself one period later
		_, size := utf8.DecodeRuneInString(text[first:])
		start := utf8.RuneCountInString(text[:first])
		period := utf8.RuneCountInString(text[first : first+size+strings.Index(text[first+size:], w)])
		for start > 0 && r[start-1] == r[start-1+period] {
			start--
		}
		if !periodic(r, start, period, loopRepeats) {
			continue
		}
		// the round may end mid-sentence (the match slid back over a shared ". "): finish that sentence
		end := start + period
		for k := end; !endsSentence(r[:end]) && k < len(r) && k < end+loopWindow; k++ {
			if strings.ContainsRune(".!?。！？\n", r[k]) {
				end = k + 1
				break
			}
		}
		return strings.TrimSpace(string(r[:end])), true
	}
	return text, false
}

// countOverlapping counts w in text, overlaps included: a passage shorter than the window repeats with
// each copy overlapping the next one, which strings.Count would count only every other time.
func countOverlapping(text, w string) int {
	n := 0
	for i := strings.Index(text, w); i >= 0; {
		n++
		_, size := utf8.DecodeRuneInString(text[i:])
		j := strings.Index(text[i+size:], w)
		if j < 0 {
			break
		}
		i += size + j
	}
	return n
}

// periodic reports whether r from start holds n copies of one period-long passage, one after another.
func periodic(r []rune, start, period, n int) bool {
	if start+period*n > len(r) {
		return false
	}
	for k := start; k < start+period*(n-1); k++ {
		if r[k] != r[k+period] {
			return false
		}
	}
	return true
}

// endsSentence reports whether r, trailing spaces aside, ends a sentence.
func endsSentence(r []rune) bool {
	s := strings.TrimRight(string(r), " \t")
	last, _ := utf8.DecodeLastRuneInString(s)
	return s != "" && strings.ContainsRune(".!?。！？\n", last)
}

func distinctRunes(s string) int {
	seen := map[rune]bool{}
	for _, c := range s {
		seen[c] = true
	}
	return len(seen)
}
