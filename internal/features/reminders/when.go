// Package reminders reads when people mean ("내일 오후 3시", "in 30 minutes", "다음주 수요일 점심")
// for reminders and plans. The words come from the language packs (lang.Time); forms made only of
// digits (2026-10-03, 10/3, 15:30) are read here for every language. Reading is deterministic and
// never guesses a half of the day: "3시" gives two candidates and the person picks one.
package reminders

import (
	"errors"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"masamong/internal/lang"
)

// Precision is how exact a candidate is.
const (
	Minute  = "minute"   // a clock time or "in N minutes"
	DayPart = "day_part" // "lunch": a usual time of day stands in for the clock
	DayOnly = "date"     // only a day: the default time of day stands in
)

// Candidate is one moment the words may mean.
type Candidate struct {
	At        time.Time `json:"at"`
	Precision string    `json:"precision"`
}

// When is what was read from a text.
type When struct {
	Candidates []Candidate `json:"candidates"` // soonest first; more than one means the person has to pick
	Matched    string      `json:"matched"`    // the time words as written
	Rest       string      `json:"rest"`       // the text without the time words and fillers: what to remind
}

// Ambiguous reports whether the person has to pick.
func (w When) Ambiguous() bool { return len(w.Candidates) > 1 }

// Errors of reading a time.
var (
	ErrNoTime      = errors.New("no time found")
	ErrPast        = errors.New("that time has passed")
	ErrInvalidDate = errors.New("invalid calendar date")
)

var (
	isoDate   = regexp.MustCompile(`\b(\d{4})-(\d{1,2})-(\d{1,2})\b`)
	slashDate = regexp.MustCompile(`(?:^|[^\d/])(\d{1,2})/(\d{1,2})(?:[^\d/]|$)`)
	clock     = regexp.MustCompile(`(?:^|[^\d:])(\d{1,2}):(\d{2})(?:[^\d:]|$)`)
)

var compiled sync.Map // pattern -> *regexp.Regexp (nil when invalid)

func re(expr string) *regexp.Regexp {
	if v, ok := compiled.Load(expr); ok {
		r, _ := v.(*regexp.Regexp)
		return r
	}
	r, err := regexp.Compile(expr)
	if err != nil {
		r = nil
	}
	compiled.Store(expr, r)
	return r
}

type span struct{ from, to int }

// text keeps the lower-cased input and the parts already used, so one word is read once.
type text struct {
	low  string
	used []span
}

func (t *text) free(from, to int) bool {
	for _, u := range t.used {
		if from < u.to && to > u.from {
			return false
		}
	}
	return true
}

func (t *text) take(from, to int) { t.used = append(t.used, span{from, to}) }

// find is the first free match of one of the patterns: its bounds and named groups.
func (t *text) find(patterns []string) (int, int, map[string]string, bool) {
	for _, p := range patterns {
		r := re(p)
		if r == nil {
			continue
		}
		for _, loc := range r.FindAllStringSubmatchIndex(t.low, -1) {
			if !t.free(loc[0], loc[1]) {
				continue
			}
			groups := map[string]string{}
			for i, name := range r.SubexpNames() {
				if name != "" && loc[2*i] >= 0 {
					groups[name] = t.low[loc[2*i]:loc[2*i+1]]
				}
			}
			return loc[0], loc[1], groups, true
		}
	}
	return 0, 0, nil, false
}

// word finds one of words as a word of its own (Latin words on word boundaries; words in scripts
// without spaces, such as Korean, anywhere) in the free text. Longer words are tried first.
func (t *text) word(words []string) (int, int, string, bool) {
	sorted := append([]string(nil), words...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, w := range sorted {
		w = strings.ToLower(strings.TrimSpace(w))
		if w == "" {
			continue
		}
		for from := 0; ; {
			i := strings.Index(t.low[from:], w)
			if i < 0 {
				break
			}
			i += from
			j := i + len(w)
			if t.free(i, j) && boundary(t.low, i, j, w) {
				return i, j, w, true
			}
			from = i + 1
		}
	}
	return 0, 0, "", false
}

func boundary(s string, i, j int, w string) bool {
	latin := func(r rune) bool { return r < 0x80 && (unicode.IsLetter(r) || unicode.IsDigit(r)) }
	first, _ := firstRune(w)
	if !latin(first) {
		return true
	}
	if i > 0 {
		if r, _ := lastRune(s[:i]); latin(r) {
			return false
		}
	}
	if j < len(s) {
		if r, _ := firstRune(s[j:]); latin(r) {
			return false
		}
	}
	return true
}

func firstRune(s string) (rune, int) {
	for _, r := range s {
		return r, 0
	}
	return 0, 0
}

func lastRune(s string) (rune, int) {
	rs := []rune(s)
	if len(rs) == 0 {
		return 0, 0
	}
	return rs[len(rs)-1], 0
}

// Options tune reading.
type Options struct {
	DefaultTime string // a day without a time of day: this clock time ("09:00")
}

// Parse reads when a text means, relative to now in loc.
func Parse(input string, now time.Time, loc *time.Location, words lang.Time, opt Options) (When, error) {
	out, err := parseOne(input, now, loc, words, opt)
	if !errors.Is(err, ErrNoTime) {
		return out, err
	}
	// A server's default language must not make a reminder impossible to understand when its
	// member writes a clear time expression in another bundled language.
	for _, tag := range lang.Available() {
		pack, ok := lang.Get(tag)
		if !ok {
			continue
		}
		candidate, candidateErr := parseOne(input, now, loc, pack.Time, opt)
		if candidateErr == nil || errors.Is(candidateErr, ErrPast) || errors.Is(candidateErr, ErrInvalidDate) {
			return candidate, candidateErr
		}
	}
	return When{}, ErrNoTime
}

func parseOne(input string, now time.Time, loc *time.Location, words lang.Time, opt Options) (When, error) {
	if loc == nil {
		loc = time.UTC
	}
	now = now.In(loc)
	t := &text{low: strings.ToLower(input)}
	if len(t.low) != len(input) {
		input = t.low // a letter whose lower case is longer: keep offsets in one string
	}
	var out When
	var matched []string
	mark := func(from, to int) {
		t.take(from, to)
		matched = append(matched, strings.TrimSpace(input[from:to]))
	}

	// "in 30 minutes": exact, nothing else needed
	if from, to, g, ok := t.find(words.After); ok {
		n, _ := strconv.Atoi(g["n"])
		unit := words.Units[strings.TrimSpace(g["unit"])]
		var at time.Time
		switch unit {
		case "minute":
			at = now.Add(time.Duration(n) * time.Minute)
		case "hour":
			at = now.Add(time.Duration(n) * time.Hour)
		case "day":
			at = now.AddDate(0, 0, n)
		case "week":
			at = now.AddDate(0, 0, 7*n)
		}
		if !at.IsZero() && n > 0 {
			mark(from, to)
			out.Candidates = []Candidate{{At: at.Truncate(time.Minute), Precision: Minute}}
			out.Matched = strings.Join(matched, " ")
			out.Rest = rest(input, t, words.Fillers)
			return out, nil
		}
	}

	// the day
	var day time.Time // midnight in loc, zero when not said
	explicit := false // a calendar date: a past time on it is an error, not tomorrow
	dayOfMonthFallback := false
	date := func(y, m, d int) (time.Time, bool) {
		if m < 1 || m > 12 || d < 1 || d > 31 {
			return time.Time{}, false
		}
		v := time.Date(y, time.Month(m), d, 0, 0, 0, 0, loc)
		return v, v.Day() == d
	}
	if m := isoDate.FindStringSubmatchIndex(t.low); m != nil && t.free(m[0], m[1]) {
		y, _ := strconv.Atoi(t.low[m[2]:m[3]])
		mo, _ := strconv.Atoi(t.low[m[4]:m[5]])
		d, _ := strconv.Atoi(t.low[m[6]:m[7]])
		if v, ok := date(y, mo, d); ok {
			day, explicit = v, true
			mark(m[0], m[1])
		} else {
			return out, ErrInvalidDate
		}
	}
	yearOf := func(mo, d int) (time.Time, bool) {
		v, ok := date(now.Year(), mo, d)
		if ok && v.Before(midnight(now)) {
			v, ok = date(now.Year()+1, mo, d)
		}
		return v, ok
	}
	if day.IsZero() {
		if from, to, g, ok := t.find(words.Dates); ok {
			mo, _ := strconv.Atoi(g["mo"])
			if name := g["mon"]; name != "" {
				mo = monthOf(name, words.Months)
			}
			d, _ := strconv.Atoi(g["d"])
			if mo > 0 {
				if v, ok := yearOf(mo, d); ok {
					day, explicit = v, true
					mark(from, to)
				} else {
					return out, ErrInvalidDate
				}
			}
		}
	}
	if day.IsZero() {
		if m := slashDate.FindStringSubmatchIndex(t.low); m != nil && t.free(m[2], m[5]) {
			mo, _ := strconv.Atoi(t.low[m[2]:m[3]])
			d, _ := strconv.Atoi(t.low[m[4]:m[5]])
			if v, ok := yearOf(mo, d); ok {
				day, explicit = v, true
				mark(m[2], m[5])
			} else {
				return out, ErrInvalidDate
			}
		}
	}
	if day.IsZero() {
		// "next wednesday", "이번주 금요일", "금요일"
		for wd := 0; wd < 7 && day.IsZero(); wd++ {
			from, to, _, ok := t.word(words.Weekdays[wd])
			if !ok {
				continue
			}
			next, this := false, false
			if f2, ok := prefixWord(t, from, words.NextWeek, words.Fillers); ok {
				next, from = true, f2
			} else if f2, ok := prefixWord(t, from, words.ThisWeek, words.Fillers); ok {
				this, from = true, f2
			}
			day = weekday(now, time.Weekday(wd), next, this)
			mark(from, to)
		}
	}
	if day.IsZero() {
		// "tomorrow"
		type rel struct {
			w string
			n int
		}
		var rels []rel
		for w, n := range words.RelativeDays {
			rels = append(rels, rel{w, n})
		}
		sort.Slice(rels, func(i, j int) bool { return len(rels[i].w) > len(rels[j].w) })
		for _, r := range rels {
			if from, to, _, ok := t.word([]string{r.w}); ok {
				day = midnight(now).AddDate(0, 0, r.n)
				mark(from, to)
				break
			}
		}
	}
	if day.IsZero() {
		if from, to, g, ok := t.find(words.DayOfMonth); ok {
			d, _ := strconv.Atoi(g["d"])
			if d < 1 || d > 31 {
				return out, ErrInvalidDate
			}
			first := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, loc)
			v := dayInMonth(first, d)
			if d > daysInMonth(first) || v.Before(midnight(now)) {
				v = dayInMonth(first.AddDate(0, 1, 0), d)
			} else {
				dayOfMonthFallback = true
			}
			day, explicit = v, true
			mark(from, to)
		}
	}

	// the time of day
	hour, minute := -1, 0
	half := "" // am | pm | "" when not said
	precision := Minute
	if from, to, g, ok := t.find(words.Hours); ok {
		hour, _ = strconv.Atoi(g["h"])
		minute, _ = strconv.Atoi(g["m"])
		if g["half"] != "" {
			minute = 30
		}
		if ap := g["ampm"]; ap != "" {
			half = halfOf(ap, words)
		}
		mark(from, to)
	} else if m := clock.FindStringSubmatchIndex(t.low); m != nil && t.free(m[2], m[5]) {
		hour, _ = strconv.Atoi(t.low[m[2]:m[3]])
		minute, _ = strconv.Atoi(t.low[m[4]:m[5]])
		mark(m[2], m[5])
	}
	if hour >= 0 && half == "" {
		// "오후 3시", "3pm", "3 in the afternoon": an am/pm word next to the hour
		if f, to, w, ok := t.word(append(append([]string{}, words.AM...), words.PM...)); ok && near(t, f, to) {
			half = halfOf(w, words)
			mark(f, to)
		}
	}
	if hour < 0 {
		if from, to, w, ok := t.word(keys(words.DayParts)); ok {
			if hh, mm, ok := hhmm(words.DayParts[w]); ok {
				hour, minute, half, precision = hh, mm, "24", DayPart
				mark(from, to)
			}
		}
	}
	if hour > 24 || minute > 59 {
		return out, ErrNoTime
	}
	if hour < 0 && day.IsZero() {
		return out, ErrNoTime
	}
	if hour < 0 {
		hh, mm, ok := hhmm(opt.DefaultTime)
		if !ok {
			hh, mm = 9, 0
		}
		hour, minute, half, precision = hh, mm, "24", DayOnly
	}
	if hour == 24 {
		hour = 0
	}

	// the hours the words may mean: "3시" is 03:00 or 15:00
	var hours []int
	switch {
	case half == "am":
		hours = []int{hour % 12}
	case half == "pm":
		hours = []int{hour%12 + 12}
	case half == "24" || hour == 0 || hour >= 13:
		hours = []int{hour}
	case hour == 12:
		hours = []int{12}
	default:
		hours = []int{hour, hour + 12}
	}
	var cands []Candidate
	for _, h := range hours {
		if !day.IsZero() {
			at := time.Date(day.Year(), day.Month(), day.Day(), h, minute, 0, 0, loc)
			if at.After(now) {
				cands = append(cands, Candidate{At: at, Precision: precision})
			}
			continue
		}
		at := time.Date(now.Year(), now.Month(), now.Day(), h, minute, 0, 0, loc)
		if !at.After(now) {
			at = at.AddDate(0, 0, 1)
		}
		cands = append(cands, Candidate{At: at, Precision: precision})
	}
	if len(cands) == 0 && dayOfMonthFallback {
		day = dayInMonth(time.Date(day.Year(), day.Month()+1, 1, 0, 0, 0, 0, loc), day.Day())
		for _, h := range hours {
			at := time.Date(day.Year(), day.Month(), day.Day(), h, minute, 0, 0, loc)
			if at.After(now) {
				cands = append(cands, Candidate{At: at, Precision: precision})
			}
		}
	}
	sort.Slice(cands, func(i, j int) bool { return cands[i].At.Before(cands[j].At) })
	if len(cands) == 0 {
		if explicit || !day.IsZero() {
			return out, ErrPast
		}
		return out, ErrNoTime
	}
	out.Candidates = cands
	out.Matched = strings.Join(matched, " ")
	out.Rest = rest(input, t, words.Fillers)
	return out, nil
}

func daysInMonth(first time.Time) int {
	return first.AddDate(0, 1, -1).Day()
}

// dayInMonth clamps a day-of-month request to the last day of a shorter month.
func dayInMonth(first time.Time, day int) time.Time {
	if day > daysInMonth(first) {
		day = daysInMonth(first)
	}
	return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, first.Location())
}

// near reports whether the am/pm word at [from,to) sits right next to a used time span.
func near(t *text, from, to int) bool {
	for _, u := range t.used {
		gap := from - u.to
		if to <= u.from {
			gap = u.from - to
		}
		if gap >= 0 && gap <= 3 {
			return true
		}
	}
	return false
}

// prefixWord finds one of words right before position at and returns where it starts. A space, or
// one filler such as the Japanese particle in "来週の水曜日", may sit between.
func prefixWord(t *text, at int, words, fillers []string) (int, bool) {
	before := strings.TrimRight(t.low[:at], " ")
	ends := []string{before}
	for _, f := range fillers {
		f = strings.ToLower(f)
		if f != "" && strings.HasSuffix(before, f) && t.free(len(before)-len(f), len(before)) && boundary(t.low, len(before)-len(f), len(before), f) {
			ends = append(ends, strings.TrimRight(before[:len(before)-len(f)], " "))
		}
	}
	sorted := append([]string(nil), words...)
	sort.Slice(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, end := range ends {
		for _, w := range sorted {
			w = strings.ToLower(w)
			if w != "" && strings.HasSuffix(end, w) && t.free(len(end)-len(w), len(end)) && boundary(t.low, len(end)-len(w), len(end), w) {
				return len(end) - len(w), true
			}
		}
	}
	return 0, false
}

func halfOf(w string, words lang.Time) string {
	w = strings.ToLower(strings.TrimSpace(w))
	norm := func(s string) string { return strings.ReplaceAll(strings.ToLower(s), ".", "") }
	for _, a := range words.AM {
		if norm(a) == norm(w) {
			return "am"
		}
	}
	for _, p := range words.PM {
		if norm(p) == norm(w) {
			return "pm"
		}
	}
	return ""
}

func monthOf(name string, months [12][]string) int {
	for i, ws := range months {
		for _, w := range ws {
			if strings.EqualFold(w, name) {
				return i + 1
			}
		}
	}
	return 0
}

// weekday is the date of wd: this week's, next week's (weeks start on Monday), or the next one.
func weekday(now time.Time, wd time.Weekday, next, this bool) time.Time {
	today := midnight(now)
	if next || this {
		offset := (int(now.Weekday()) + 6) % 7 // days since Monday
		monday := today.AddDate(0, 0, -offset)
		d := monday.AddDate(0, 0, (int(wd)+6)%7)
		if next {
			d = d.AddDate(0, 0, 7)
		}
		return d
	}
	diff := (int(wd) - int(now.Weekday()) + 7) % 7
	return today.AddDate(0, 0, diff)
}

func midnight(t time.Time) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), 0, 0, 0, 0, t.Location())
}

func hhmm(v string) (int, int, bool) {
	h, m, ok := strings.Cut(strings.TrimSpace(v), ":")
	if !ok {
		return 0, 0, false
	}
	hh, err1 := strconv.Atoi(h)
	mm, err2 := strconv.Atoi(m)
	return hh, mm, err1 == nil && err2 == nil && hh >= 0 && hh < 24 && mm >= 0 && mm < 60
}

func keys(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// rest is the input without the time words, with filler words and stray punctuation dropped. A
// filler goes only where it belongs to the time or the request: next to a time word, at the start or
// at the end, or next to another filler that went ("remind me to …", "… 알려줘"). The same word in
// the middle of the reminder stays ("pagar el alquiler").
func rest(input string, t *text, fillers []string) string {
	const cut = "\x00"
	sort.Slice(t.used, func(i, j int) bool { return t.used[i].from > t.used[j].from })
	b := input
	for _, u := range t.used {
		if u.from >= 0 && u.to <= len(b) {
			b = b[:u.from] + cut + b[u.to:]
		}
	}
	type filler struct {
		words []string
		raw   string
	}
	var fs []filler
	var glued []string // fillers of scripts without spaces, which stick to the time word ("3時に会議")
	for _, f := range fillers {
		lf := strings.ToLower(strings.TrimSpace(f))
		if lf == "" {
			continue
		}
		fs = append(fs, filler{strings.Fields(lf), lf})
		if r, _ := firstRune(lf); r >= 0x80 && !strings.Contains(lf, " ") {
			glued = append(glued, lf)
		}
	}
	sort.Slice(fs, func(i, j int) bool {
		return len(fs[i].words) > len(fs[j].words) || len(fs[i].words) == len(fs[j].words) && len(fs[i].raw) > len(fs[j].raw)
	})
	sort.Slice(glued, func(i, j int) bool { return len(glued[i]) > len(glued[j]) })
	// one glued filler right after and right before each time word comes off
	parts := strings.Split(b, cut)
	for i := range parts {
		if i > 0 {
			for _, g := range glued {
				if strings.HasPrefix(strings.ToLower(parts[i]), g) {
					parts[i] = parts[i][len(g):]
					break
				}
			}
		}
		if i < len(parts)-1 {
			for _, g := range glued {
				if strings.HasSuffix(strings.ToLower(parts[i]), g) {
					parts[i] = parts[i][:len(parts[i])-len(g)]
					break
				}
			}
		}
	}
	tokens := strings.Fields(strings.Join(parts, " "+cut+" "))
	norm := func(w string) string { return strings.ToLower(strings.Trim(w, ",.!?~")) }
	// the filler spans, longest first at each place
	type span struct{ from, to int }
	var spans []span
	for i := 0; i < len(tokens); {
		if tokens[i] == cut {
			i++
			continue
		}
		n := 0
		for _, f := range fs {
			if i+len(f.words) > len(tokens) {
				continue
			}
			ok := true
			for k, w := range f.words {
				if norm(tokens[i+k]) != w {
					ok = false
					break
				}
			}
			if ok {
				n = len(f.words)
				break
			}
		}
		if n == 0 {
			if norm(tokens[i]) == "" { // stray punctuation goes with its neighbours
				n = 1
			} else {
				i++
				continue
			}
		}
		spans = append(spans, span{i, i + n})
		i += n
	}
	drop := make([]bool, len(tokens))
	for i, tk := range tokens {
		drop[i] = tk == cut
	}
	// a span goes when the token before or after it is the edge of the text or already gone
	gone := func(i int) bool { return i < 0 || i >= len(tokens) || drop[i] }
	for changed := true; changed; {
		changed = false
		for _, sp := range spans {
			if drop[sp.from] {
				continue
			}
			if gone(sp.from-1) || gone(sp.to) {
				for k := sp.from; k < sp.to; k++ {
					drop[k] = true
				}
				changed = true
			}
		}
	}
	var kept []string
	for i, tk := range tokens {
		if !drop[i] {
			kept = append(kept, tk)
		}
	}
	return strings.Join(strings.Fields(strings.Trim(strings.Join(kept, " "), " ,.!?~")), " ")
}
