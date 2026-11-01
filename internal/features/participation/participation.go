// Package participation holds the cheap, rule-based half of joining a conversation unasked (12,
// masamong-v2-design/09 §2–3): which messages are worth a closer look at all. It never decides that
// the bot speaks; a candidate only earns one bounded model judgement after a quiet wait, and silence is
// the normal result. Words come from the language packs (lang.Time), never from code.
package participation

import (
	"strings"
	"time"

	"masamong/internal/features/reminders"
	"masamong/internal/lang"
)

// Kinds of candidate.
const (
	Question = "question" // an information question nobody answered
	Plan     = "plan"     // people making a plan with a date or time: the bot may offer a reminder
)

// Signals are what the words of one message suggest.
type Signals struct {
	Question bool `json:"question"`
	Recall   bool `json:"recall"` // asks the group's memory ("when did we ...")
	Plan     bool `json:"plan"`
	HasTime  bool `json:"has_time"`
}

// Kind is the candidate kind the signals make ("" = not a candidate).
func (s Signals) Kind() string {
	switch {
	case s.Plan && s.HasTime:
		return Plan
	case s.Question:
		return Question
	}
	return ""
}

// Read looks at one message. now and loc anchor the time words of a plan.
func Read(text string, now time.Time, loc *time.Location, words lang.Time) Signals {
	t := strings.ToLower(strings.TrimSpace(text))
	var s Signals
	if t == "" || len([]rune(t)) < 2 {
		return s
	}
	s.Question = strings.HasSuffix(t, "?") || strings.HasSuffix(t, "？") || containsAny(t, words.Question)
	s.Recall = containsAny(t, words.Recall)
	s.Plan = containsAny(t, words.PlanWords)
	if s.Plan {
		if w, err := reminders.Parse(text, now, loc, words, reminders.Options{}); err == nil && len(w.Candidates) > 0 {
			s.HasTime = true
		}
	}
	return s
}

func containsAny(t string, words []string) bool {
	for _, w := range words {
		if w = strings.ToLower(strings.TrimSpace(w)); w != "" && strings.Contains(t, w) {
			return true
		}
	}
	return false
}

// Budget is how often a channel may hear from the bot unasked.
type Budget struct {
	Cooldown time.Duration
	PerHour  int
	PerDay   int
}

// Allows reports whether one more remark at now keeps within the budget, given the times of the
// earlier ones (any order), and the reason when it does not.
func (b Budget) Allows(spoken []time.Time, now time.Time) (bool, string) {
	hour, day := 0, 0
	for _, t := range spoken {
		if now.Sub(t) < b.Cooldown {
			return false, "cooldown"
		}
		if now.Sub(t) < time.Hour {
			hour++
		}
		if now.Sub(t) < 24*time.Hour {
			day++
		}
	}
	switch {
	case b.PerHour > 0 && hour >= b.PerHour:
		return false, "hourly_limit"
	case b.PerDay > 0 && day >= b.PerDay:
		return false, "daily_limit"
	}
	return true, ""
}

// Quiet reports whether local time t falls in quiet hours start–end (HH:MM; overnight windows allowed).
func Quiet(t time.Time, start, end string) bool {
	if start == "" || end == "" || start == end {
		return false
	}
	now := t.Format("15:04")
	if start < end {
		return now >= start && now < end
	}
	return now >= start || now < end
}
