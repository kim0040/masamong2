package websearch

import (
	"math"
	"regexp"
	"sort"
	"strings"
	"unicode"
)

// Passages cuts the part of a page that answers the query: the page's first paragraph and the
// paragraphs that share the most with the query, in page order, up to maxRunes. Nothing is asked of a
// model. Words are matched as they are written, and Hangul, Han and kana by pairs of characters, so
// a Korean particle ("서울의", "서울에서") does not hide the word under it. Links and images are cut
// down to their text, bare addresses and rows of menu links are dropped.
func Passages(query, text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	blocks := pageBlocks(text)
	if len(blocks) == 0 {
		return ""
	}
	score := scoreBlocks(query, blocks)

	// the lead first, then the best matches; the order of choosing is not the order of printing
	chosen := map[int]bool{0: true}
	used := min(runeLen(blocks[0].text), maxRunes/3)
	order := make([]int, 0, len(blocks))
	for i := 1; i < len(blocks); i++ {
		order = append(order, i)
	}
	matched := false
	for _, s := range score {
		if s > 0 {
			matched = true
			break
		}
	}
	if matched {
		sort.SliceStable(order, func(a, b int) bool { return score[order[a]] > score[order[b]] })
	} // no match anywhere: the page's own order, i.e. its beginning
	clipped := map[int]int{}
	if runeLen(blocks[0].text) > maxRunes/3 {
		clipped[0] = maxRunes / 3
	}
	for _, i := range order {
		if matched && score[i] <= 0 {
			break
		}
		room := maxRunes - used - 2
		n := runeLen(blocks[i].text)
		switch {
		case n <= room:
			chosen[i], used = true, used+n+2
		case len(chosen) == 1 && room >= 100:
			chosen[i], clipped[i], used = true, room, maxRunes // the best match alone is longer than the budget
		}
		if used >= maxRunes-1 {
			break
		}
	}
	var out strings.Builder
	prev := -1
	for i := range blocks {
		if !chosen[i] {
			continue
		}
		t := blocks[i].text
		if n, cut := clipped[i]; cut {
			t = string([]rune(t)[:n]) + "…"
		}
		if prev >= 0 {
			if blocks[i].para == blocks[prev].para {
				out.WriteString(" ") // two parts of one long paragraph
			} else {
				out.WriteString("\n\n")
			}
		}
		out.WriteString(t)
		prev = i
	}
	return out.String()
}

type block struct {
	text string
	para int // the paragraph a long one was split from
}

func runeLen(s string) int { return len([]rune(s)) }

var (
	reImage    = regexp.MustCompile(`!\[([^\]]*)\]\((?:[^()\s]|\([^()]*\))*(?:\s+"[^"]*")?\)`)
	reLink     = regexp.MustCompile(`\[([^\]]*)\]\((?:[^()\s]|\([^()]*\))*(?:\s+"[^"]*")?\)`)
	reAutoLink = regexp.MustCompile(`<https?://[^>\s]+>`)
	reBareURL  = regexp.MustCompile(`(?i)\b(?:https?://|www\.)[^\s)\]>]+`)
	reListMark = regexp.MustCompile(`^\s*(?:[-*+•]\s+|\d+[.)]\s+)`)
	reRuleLine = regexp.MustCompile(`^[\s|:\-_=*]+$`)
	reHeading  = regexp.MustCompile(`^\s{0,3}#{1,6}\s+`)
	reNavOnly  = regexp.MustCompile(`^(?:\s*[|·•/>»-]?\s*!?\[[^\]]*\]\((?:[^()\s]|\([^()]*\))*(?:\s+"[^"]*")?\))+\s*$`)
)

// cleanLine strips markdown down to the words; ok is false for a line that is only menu links, a
// rule or a bare address.
func cleanLine(line string) (string, bool) {
	trim := strings.TrimSpace(line)
	if trim == "" || reRuleLine.MatchString(trim) {
		return "", false
	}
	if reNavOnly.MatchString(reListMark.ReplaceAllString(trim, "")) {
		// a row of links is a menu when the words in it are few; a sentence made of links is not
		if runeLen(reLink.ReplaceAllString(reImage.ReplaceAllString(trim, "$1"), "$1")) < 60 {
			return "", false
		}
	}
	s := reImage.ReplaceAllString(trim, "$1")
	s = reLink.ReplaceAllString(s, "$1")
	s = reAutoLink.ReplaceAllString(s, "")
	s = reBareURL.ReplaceAllString(s, "")
	s = strings.NewReplacer("**", "", "__", "", "`", "").Replace(s)
	s = strings.Join(strings.Fields(s), " ")
	return s, s != ""
}

// pageBlocks splits a page into paragraphs at blank lines and headings (a heading goes with the
// paragraph under it), cleans them, drops menus and labels, and cuts a paragraph longer than about
// 500 characters into sentence-sized parts so a long page of one paragraph can still be picked from.
func pageBlocks(text string) []block {
	var paras []string
	var cur []string
	var heading []string
	flush := func() {
		if len(cur) == 0 {
			return
		}
		body := strings.Join(cur, "\n")
		cur = nil
		if len(heading) > 0 {
			body = strings.Join(heading, "\n") + "\n" + body
			heading = nil
		}
		paras = append(paras, body)
	}
	for _, line := range strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			flush()
			continue
		}
		if reHeading.MatchString(line) {
			flush()
			if h, ok := cleanLine(reHeading.ReplaceAllString(line, "")); ok {
				heading = append(heading, h)
			}
			continue
		}
		if l, ok := cleanLine(line); ok {
			cur = append(cur, l)
		}
	}
	flush()
	if len(heading) > 0 { // a heading with nothing under it
		paras = append(paras, strings.Join(heading, "\n"))
	}
	var out []block
	para := 0
	for _, p := range paras {
		if runeLen(p) < 12 && !strings.ContainsAny(p, "0123456789.!?。！？…다요") {
			continue // a label ("Sign in", "더보기"), not text
		}
		for _, part := range splitLong(p, 500, 220) {
			out = append(out, block{text: part, para: para})
		}
		para++
	}
	return out
}

// splitLong cuts s into parts of about target runes at the ends of sentences when it is longer than
// limit; shorter text is one part.
func splitLong(s string, limit, target int) []string {
	r := []rune(s)
	if len(r) <= limit {
		return []string{s}
	}
	var parts []string
	start := 0
	for i := 0; i < len(r); i++ {
		end := i+1 == len(r)
		stop := false
		switch r[i] {
		case '.', '!', '?', '。', '！', '？', '\n':
			stop = i+1 < len(r) && (unicode.IsSpace(r[i+1]) || r[i] == '。' || r[i] == '！' || r[i] == '？' || r[i] == '\n')
		}
		if end || (stop && i+1-start >= target) || i+1-start >= limit {
			if p := strings.TrimSpace(string(r[start : i+1])); p != "" {
				parts = append(parts, p)
			}
			start = i + 1
		}
	}
	return parts
}

// token classes
func isCJK(r rune) bool {
	return unicode.Is(unicode.Hangul, r) || unicode.Is(unicode.Han, r) || unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r)
}

// tokenize lower-cases s and returns its words (weight 2) and, for each run of Hangul, Han or kana,
// the pairs of neighbouring characters (weight 1; a run of one character is itself).
func tokenize(s string) map[string]float64 {
	out := map[string]float64{}
	var run []rune
	cjk := false
	flush := func() {
		switch {
		case len(run) == 0:
		case cjk && len(run) == 1:
			out[string(run)] = 1
		case cjk:
			for i := 0; i+1 < len(run); i++ {
				out[string(run[i:i+2])] = 1
			}
		case len(run) >= 2 || unicode.IsDigit(run[0]):
			out[string(run)] = 2
		}
		run = run[:0]
	}
	for _, r := range strings.ToLower(s) {
		switch {
		case isCJK(r):
			if !cjk {
				flush()
			}
			cjk = true
			run = append(run, r)
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			if cjk {
				flush()
			}
			cjk = false
			run = append(run, r)
		default:
			flush()
		}
	}
	flush()
	return out
}

// scoreBlocks gives each block the weight of the query tokens it holds, a token counting less the
// more of the page's blocks hold it (what every paragraph says tells nothing), and long blocks a
// little less than short ones.
func scoreBlocks(query string, blocks []block) []float64 {
	q := tokenize(query)
	sets := make([]map[string]float64, len(blocks))
	df := map[string]int{}
	for i, b := range blocks {
		sets[i] = tokenize(b.text)
		for t := range q {
			if _, ok := sets[i][t]; ok {
				df[t]++
			}
		}
	}
	score := make([]float64, len(blocks))
	n := float64(len(blocks))
	for i, b := range blocks {
		var s float64
		for t, w := range q {
			if df[t] > 0 {
				if _, ok := sets[i][t]; ok {
					s += w * math.Log(1+n/float64(df[t]))
				}
			}
		}
		score[i] = s / math.Pow(math.Max(float64(runeLen(b.text)), 100)/100, 0.25)
	}
	return score
}
