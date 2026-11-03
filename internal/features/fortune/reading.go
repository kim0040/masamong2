package fortune

import (
	"regexp"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A reading is what the model wrote for a fortune, in "KEY: value" lines (docs/plan-discord-experience.md
// §3.4). The keys are fixed English words so one parser serves every language; the values are in the
// reader's language. The card is built from the keys, so the layout does not depend on how the model
// felt like formatting that day. A text that is not in this shape is shown as written (Parsed is false).

// Keys a reading may carry. A line without a key continues the value above it. A line that looks like
// a key this list does not have ("NOTE: …", two or more capitals) is dropped, and so are the lines that
// continue it: it never runs into the value above.
var readingKeys = map[string]bool{
	"SCORE": true, "GREETING": true, "SUMMARY": true, "ONELINE": true,
	"WORK": true, "MONEY": true, "LOVE": true, "HEALTH": true,
	"MORNING": true, "AFTERNOON": true, "EVENING": true,
	"EARLY": true, "MID": true, "LATE": true,
	"Q1": true, "Q2": true, "Q3": true, "Q4": true,
	"BEST_TIME": true, "CAUTION_TIME": true, "DATES": true,
	"DO": true, "AVOID": true, "CHECKLIST": true,
	"LUCKY_COLOR": true, "LUCKY_ITEM": true, "LUCKY_TIME": true, "LUCKY_NUMBER": true, "LUCKY_DIRECTION": true,
	"RANK": true,
}

// valueMax caps one value (runes): a model that runs on in one key does not crowd out the rest.
var valueMax = map[string]int{"SUMMARY": 400, "GREETING": 120, "ONELINE": 160, "RANK": 240}

const defaultValueMax = 300

// Reading is a parsed fortune.
type Reading struct {
	Score  int               // 0–100, or -1 when the text had none
	Values map[string]string // key -> value, cleaned
	Ranks  []Rank            // RANK lines, in rank order
}

// Rank is one line of the twelve-sign ranking: "RANK: 1 | Aries | comment | color | number".
type Rank struct {
	Pos                          int
	Sign, Comment, Color, Number string
}

var (
	keyLine   = regexp.MustCompile(`^[*_\s]*([A-Z][A-Z0-9_]*)[*_\s]*[:：]\s*(.*)$`)
	firstInt  = regexp.MustCompile(`-?\d+`)
	listMark  = regexp.MustCompile(`^\s*(?:[-*•·]|\d+[.)])\s+`)
	boldMarks = strings.NewReplacer("**", "", "__", "")
)

// ParseReading reads a fortune text. Whether it is usable is for the caller to ask (Structured, Ranked):
// a text that is neither is shown as written.
func ParseReading(text string) (r Reading) {
	r = Reading{Score: -1, Values: map[string]string{}}
	last := ""
	for _, raw := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "```") {
			continue
		}
		line = listMark.ReplaceAllString(line, "")
		m := keyLine.FindStringSubmatch(line)
		if m != nil && !readingKeys[m[1]] && len(m[1]) >= 2 {
			last = "" // an unknown key: the line and its continuation go nowhere
			continue
		}
		if m != nil && readingKeys[m[1]] {
			key, value := m[1], clean(m[2])
			if key == "RANK" {
				if rk, good := parseRank(value); good {
					r.Ranks = append(r.Ranks, rk)
				}
				last = ""
				continue
			}
			if _, seen := r.Values[key]; seen {
				last = "" // a repeated key keeps the first value; its continuation lines go nowhere
				continue
			}
			r.Values[key] = value
			last = key
			continue
		}
		if last != "" { // a value the model wrapped onto the next line
			r.Values[last] = strings.TrimSpace(r.Values[last] + " " + clean(line))
		}
	}
	for k, v := range r.Values {
		limit := defaultValueMax
		if n, ok := valueMax[k]; ok {
			limit = n
		}
		if v == "" {
			delete(r.Values, k)
			continue
		}
		r.Values[k] = clip(v, limit)
	}
	if s, has := r.Values["SCORE"]; has {
		if n, err := strconv.Atoi(firstInt.FindString(s)); err == nil && n >= 0 && n <= 100 {
			r.Score = n
		}
		delete(r.Values, "SCORE")
	}
	r.Ranks = orderRanks(r.Ranks)
	return r
}

// WithoutKeys is text without its lines that start with one of keys ("SCORE: 85"), for a text shown as
// written whose points are computed elsewhere.
func WithoutKeys(text string, keys ...string) string {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	out := lines[:0]
	for _, raw := range lines {
		line := listMark.ReplaceAllString(strings.TrimSpace(raw), "")
		if m := keyLine.FindStringSubmatch(line); m != nil && slices.Contains(keys, m[1]) {
			continue
		}
		out = append(out, raw)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// Structured reports whether the text was a reading: a SUMMARY and at least one more known key.
func (r Reading) Structured() bool {
	known := len(r.Values)
	if r.Score >= 0 {
		known++
	}
	return known >= 2 && r.Values["SUMMARY"] != ""
}

// Ranked reports whether the text was a ranking: at least half of the twelve RANK lines.
func (r Reading) Ranked() bool { return len(r.Ranks) >= Signs/2 }

func parseRank(v string) (Rank, bool) {
	parts := strings.Split(v, "|")
	for i := range parts {
		parts[i] = strings.TrimSpace(parts[i])
	}
	if len(parts) < 3 {
		return Rank{}, false
	}
	pos, err := strconv.Atoi(firstInt.FindString(parts[0]))
	if err != nil || pos < 1 || pos > Signs || parts[1] == "" {
		return Rank{}, false
	}
	rk := Rank{Pos: pos, Sign: clip(parts[1], 40), Comment: clip(parts[2], 160)}
	if len(parts) > 3 {
		rk.Color = clip(parts[3], 30)
	}
	if len(parts) > 4 {
		rk.Number = clip(parts[4], 10)
	}
	return rk, true
}

// orderRanks sorts by position and keeps the first line of each position.
func orderRanks(in []Rank) []Rank {
	var out []Rank
	for pos := 1; pos <= Signs; pos++ {
		for _, rk := range in {
			if rk.Pos == pos {
				out = append(out, rk)
				break
			}
		}
	}
	return out
}

// Items splits a list value ("a | b | c") into its items.
func Items(v string) []string {
	var out []string
	for _, p := range strings.Split(v, "|") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// ScoreBar is a ten-step bar for a 0–100 score ("▰▰▰▰▰▰▰▰▱▱"); it is decoration, the number is always
// shown next to it.
func ScoreBar(score int) string {
	if score < 0 {
		return ""
	}
	full := min(10, max(0, (score+5)/10))
	return strings.Repeat("▰", full) + strings.Repeat("▱", 10-full)
}

// clean drops the formatting a model wraps around a value: bold marks and a leading list mark.
func clean(v string) string {
	v = strings.TrimSpace(boldMarks.Replace(v))
	return strings.TrimSpace(listMark.ReplaceAllString(v, ""))
}

func clip(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return strings.TrimSpace(string(r[:n-1])) + "…"
}
