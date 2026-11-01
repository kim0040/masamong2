package notices

import (
	"regexp"
	"slices"
	"sort"
	"strings"
	"time"
)

// Signals are what a notice is about, from the catalog vocabulary.
type Signals struct {
	Audiences []string `json:"audiences,omitempty"`
	Topics    []string `json:"topics,omitempty"`
	Actions   []string `json:"actions,omitempty"`
	Dates     []string `json:"dates,omitempty"` // YYYY-MM-DD, or MM-DD without a year
}

var (
	fullDateRE = regexp.MustCompile(`(20\d{2})\s*[./년-]\s*(\d{1,2})\s*[./월-]\s*(\d{1,2})`)
	monthDayRE = regexp.MustCompile(`(?:^|[^\d.년/-])(\d{1,2})\s*[./월]\s*(\d{1,2})\s*일?`)
)

func matched(text string, terms []Term) []string {
	lower := strings.ToLower(text)
	var out []string
	for _, t := range terms {
		for _, w := range t.Terms {
			if strings.Contains(lower, strings.ToLower(w)) {
				out = append(out, t.ID)
				break
			}
		}
	}
	return out
}

// Signals tags a notice from its title and body.
func (c *Catalog) Signals(title, body string) Signals {
	text := title + "\n" + body
	s := Signals{Audiences: matched(text, c.Vocabulary.Audiences), Topics: matched(text, c.Vocabulary.Topics), Actions: matched(text, c.Vocabulary.Actions)}
	seen := map[string]bool{}
	for _, m := range fullDateRE.FindAllStringSubmatch(text, 24) {
		if d := validDate(m[1], m[2], m[3]); d != "" && !seen[d] {
			seen[d] = true
			s.Dates = append(s.Dates, d)
		}
	}
	if len(s.Dates) == 0 {
		for _, m := range monthDayRE.FindAllStringSubmatch(text, 12) {
			if d := validDate("2000", m[1], m[2]); d != "" && !seen[d[5:]] {
				seen[d[5:]] = true
				s.Dates = append(s.Dates, d[5:])
			}
		}
	}
	return s
}

// Profile is what a person told the bot about themselves for school notices.
type Profile struct {
	School     string   `json:"school"`
	Degree     string   `json:"degree"` // undergraduate | master | doctorate | integrated
	Grade      int      `json:"grade,omitempty"`
	Department string   `json:"department,omitempty"`
	Campus     string   `json:"campus,omitempty"`
	Interests  []string `json:"interests,omitempty"` // topic ids
	Muted      []string `json:"muted,omitempty"`     // topic ids
}

// Reason explains a score, as a message key and its argument (the caller words it).
type Reason struct {
	Key string `json:"key"`
	Arg string `json:"arg,omitempty"`
}

// Scored is a notice ranked for a profile.
type Scored struct {
	Score    float64  `json:"score"`
	Band     string   `json:"band"` // action | opportunity | reference | hidden
	Deadline string   `json:"deadline,omitempty"`
	Reasons  []Reason `json:"reasons"`
}

// Score ranks one notice of the person's school for their profile. It is a rule list, not a model:
// what the notice is for (degree), whose board it is (department, campus), what they asked to see or
// not (interests, muted topics) and whether something is due soon.
func (c *Catalog) Score(p Profile, src Source, it Item, sig Signals, body string, today time.Time) Scored {
	sc := Scored{Score: 20}
	add := func(v float64, key, arg string) {
		sc.Score += v
		sc.Reasons = append(sc.Reasons, Reason{Key: key, Arg: arg})
	}
	for _, t := range sig.Topics {
		if slices.Contains(p.Muted, t) {
			sc.Band, sc.Score = "hidden", 0
			sc.Reasons = []Reason{{Key: "muted", Arg: t}}
			return sc
		}
	}
	mine := c.Vocabulary.DegreeAudiences[p.Degree]
	var others []string
	for d, auds := range c.Vocabulary.DegreeAudiences {
		if d != p.Degree {
			for _, a := range auds {
				if !slices.Contains(mine, a) && !slices.Contains(others, a) {
					others = append(others, a)
				}
			}
		}
	}
	forMe, forOthers := false, false
	for _, a := range sig.Audiences {
		forMe = forMe || slices.Contains(mine, a)
		forOthers = forOthers || slices.Contains(others, a)
	}
	switch {
	case forMe:
		add(20, "for_degree", p.Degree)
	case forOthers:
		add(-40, "other_degree", "")
	}
	if slices.Contains(sig.Audiences, "enrolled") {
		add(10, "for_enrolled", "")
	}
	text := strings.ToLower(it.Title + "\n" + body + "\n" + it.Category)
	if deps := src.Tags["department"]; len(deps) > 0 {
		if p.Department != "" && !containsFold(deps, p.Department) && !strings.Contains(text, strings.ToLower(p.Department)) {
			sc.Band, sc.Score = "hidden", 0
			sc.Reasons = []Reason{{Key: "other_department", Arg: strings.Join(deps, ", ")}}
			return sc
		}
		if p.Department != "" {
			add(22, "my_department", p.Department)
		}
	} else if p.Department != "" && strings.Contains(text, strings.ToLower(p.Department)) {
		add(22, "my_department", p.Department)
	}
	if camps := src.Tags["campus"]; len(camps) > 0 && p.Campus != "" && !containsFold(camps, p.Campus) {
		add(-30, "other_campus", strings.Join(camps, ", "))
	}
	liked := 0
	for _, t := range sig.Topics {
		if slices.Contains(p.Interests, t) && liked < 2 {
			liked++
			add(15, "interest", t)
		}
	}
	if it.Pinned {
		add(5, "pinned", "")
	}
	if dl := nextDate(sig.Dates, today); dl != "" {
		sc.Deadline = dl
		if d, err := time.Parse("2006-01-02", dl); err == nil && d.Sub(today) <= 14*24*time.Hour && len(sig.Actions) > 0 {
			add(15, "due_soon", dl)
		}
	}
	switch {
	case len(sig.Actions) > 0 && sc.Deadline != "":
		sc.Band = "action"
	case hasAny(sig.Topics, c.Vocabulary.OpportunityTopics):
		sc.Band = "opportunity"
	default:
		sc.Band = "reference"
	}
	sort.SliceStable(sc.Reasons, func(i, j int) bool { return sc.Reasons[i].Key < sc.Reasons[j].Key })
	return sc
}

// nextDate is the first date on or after today (a month-day gets today's year, or next year's).
func nextDate(dates []string, today time.Time) string {
	best := ""
	day := today.Format("2006-01-02")
	for _, d := range dates {
		if len(d) == 5 {
			full := today.Format("2006") + "-" + d
			if full < day {
				full = today.AddDate(1, 0, 0).Format("2006") + "-" + d
			}
			d = full
		}
		if d >= day && (best == "" || d < best) {
			best = d
		}
	}
	return best
}

func containsFold(list []string, v string) bool {
	for _, x := range list {
		if strings.EqualFold(strings.TrimSpace(x), strings.TrimSpace(v)) || strings.Contains(strings.ToLower(x), strings.ToLower(v)) {
			return true
		}
	}
	return false
}

func hasAny(a, b []string) bool {
	for _, x := range a {
		if slices.Contains(b, x) {
			return true
		}
	}
	return false
}

// TopicID maps a topic label or id (as people type it) to its id.
func (c *Catalog) TopicID(word string) (string, bool) {
	w := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(word), " ", ""))
	for _, t := range c.Vocabulary.Topics {
		if w == strings.ToLower(t.ID) || w == strings.ToLower(strings.ReplaceAll(t.Label, " ", "")) {
			return t.ID, true
		}
	}
	return "", false
}

// TopicLabel is a topic's label.
func (c *Catalog) TopicLabel(id string) string {
	for _, t := range c.Vocabulary.Topics {
		if t.ID == id {
			return t.Label
		}
	}
	return id
}
