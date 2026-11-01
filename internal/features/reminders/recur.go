package reminders

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Repeating reminders: a small rule set, computed on the wall clock of the reminder's own time zone
// (so "every day at 08:00" stays 08:00 across a daylight-saving change). The rules are
//
//	daily    at HH:MM
//	weekly   on a set of weekdays at HH:MM
//	monthly  on day N at HH:MM (a month without day N uses its last day)
//	hours    every N hours from a first moment (absolute time, no wall clock)
//
// Next never repeats a moment: it is the first occurrence strictly after the time it is given, which
// is what lets the scheduler advance a series and send each occurrence at most once.
const (
	RepeatDaily   = "daily"
	RepeatWeekly  = "weekly"
	RepeatMonthly = "monthly"
	RepeatHours   = "hours"
)

// MaxEveryHours bounds the interval of an "every N hours" rule.
const MaxEveryHours = 48

// ErrBadRule is returned for a rule that cannot be computed.
var ErrBadRule = errors.New("invalid repeat rule")

// Rule is how a reminder repeats.
type Rule struct {
	Kind     string    `json:"kind"`
	Time     string    `json:"time,omitempty"`     // HH:MM on the wall clock of the reminder's zone (not for hours)
	Weekdays []int     `json:"weekdays,omitempty"` // weekly: 0 = Sunday … 6 = Saturday
	Day      int       `json:"day,omitempty"`      // monthly: 1–31
	Every    int       `json:"every,omitempty"`    // hours: 1–MaxEveryHours
	Anchor   time.Time `json:"anchor,omitzero"`    // hours: the first occurrence; the grid goes on from it
	// Derived marks a weekday or day of the month that was not asked for: it follows the time chosen
	// (and is taken from the new time again when the series is rescheduled).
	Derived bool `json:"derived,omitempty"`
}

// Normalize checks a rule and puts it in its canonical form (sorted, unique weekdays).
func (r Rule) Normalize() (Rule, error) {
	switch r.Kind {
	case RepeatDaily, RepeatWeekly, RepeatMonthly:
		if _, _, ok := ParseClock(r.Time); !ok {
			return r, ErrBadRule
		}
		r.Every, r.Anchor = 0, time.Time{}
	case RepeatHours:
		if r.Every < 1 || r.Every > MaxEveryHours || r.Anchor.IsZero() {
			return r, ErrBadRule
		}
		r.Time, r.Weekdays, r.Day = "", nil, 0
		return r, nil
	default:
		return r, ErrBadRule
	}
	if r.Kind == RepeatWeekly {
		seen := map[int]bool{}
		var days []int
		for _, d := range r.Weekdays {
			if d < 0 || d > 6 {
				return r, ErrBadRule
			}
			if !seen[d] {
				seen[d] = true
				days = append(days, d)
			}
		}
		if len(days) == 0 {
			return r, ErrBadRule
		}
		sort.Ints(days)
		r.Weekdays = days
	} else {
		r.Weekdays = nil
	}
	if r.Kind == RepeatMonthly {
		if r.Day < 1 || r.Day > 31 {
			return r, ErrBadRule
		}
	} else {
		r.Day = 0
	}
	return r, nil
}

// ParseClock reads "HH:MM" (24 hours).
func ParseClock(s string) (hour, minute int, ok bool) {
	h, m, found := strings.Cut(strings.TrimSpace(s), ":")
	if !found || len(m) != 2 {
		return 0, 0, false
	}
	hv, err1 := strconv.Atoi(h)
	mv, err2 := strconv.Atoi(m)
	if err1 != nil || err2 != nil || hv < 0 || hv > 23 || mv < 0 || mv > 59 {
		return 0, 0, false
	}
	return hv, mv, true
}

func daysIn(y int, mo time.Month) int {
	return time.Date(y, mo+1, 0, 0, 0, 0, 0, time.UTC).Day()
}

// Next is the first occurrence strictly after the given moment. A wall-clock time that a daylight-saving
// change skips falls on the instant Go gives for it (an hour later in the same day); one it repeats is
// the first of the two.
func (r Rule) Next(after time.Time, loc *time.Location) (time.Time, bool) {
	r, err := r.Normalize()
	if err != nil {
		return time.Time{}, false
	}
	if r.Kind == RepeatHours {
		if after.Before(r.Anchor) {
			return r.Anchor, true
		}
		step := time.Duration(r.Every) * time.Hour
		k := after.Sub(r.Anchor)/step + 1
		return r.Anchor.Add(k * step), true
	}
	h, mi, _ := ParseClock(r.Time)
	local := after.In(loc)
	y, mo, d := local.Date()
	switch r.Kind {
	case RepeatDaily, RepeatWeekly:
		for i := 0; i <= 8; i++ {
			c := time.Date(y, mo, d+i, h, mi, 0, 0, loc)
			if r.Kind == RepeatWeekly && !hasDay(r.Weekdays, int(time.Date(y, mo, d+i, 12, 0, 0, 0, time.UTC).Weekday())) {
				continue
			}
			if c.After(after) {
				return c, true
			}
		}
	case RepeatMonthly:
		for i := 0; i <= 25; i++ {
			first := time.Date(y, mo+time.Month(i), 1, 0, 0, 0, 0, time.UTC)
			dd := min(r.Day, daysIn(first.Year(), first.Month()))
			c := time.Date(first.Year(), first.Month(), dd, h, mi, 0, 0, loc)
			if c.After(after) {
				return c, true
			}
		}
	}
	return time.Time{}, false
}

func hasDay(days []int, d int) bool {
	for _, x := range days {
		if x == d {
			return true
		}
	}
	return false
}

// Latest is the most recent occurrence in [now-grace, now]: what a scheduler that was down delivers.
// Older occurrences are skipped, never sent in a burst. ok is false when none falls in the window.
func (r Rule) Latest(now time.Time, grace time.Duration, loc *time.Location) (time.Time, bool) {
	o, ok := r.Next(now.Add(-grace).Add(-time.Nanosecond), loc)
	if !ok || o.After(now) {
		return time.Time{}, false
	}
	for i := 0; i < 2000; i++ { // a window holds a handful of occurrences at most
		n, ok := r.Next(o, loc)
		if !ok || n.After(now) {
			break
		}
		o = n
	}
	return o, true
}

var weekdayWords = map[string]int{
	"sun": 0, "sunday": 0, "mon": 1, "monday": 1, "tue": 2, "tues": 2, "tuesday": 2, "wed": 3, "wednesday": 3,
	"thu": 4, "thur": 4, "thurs": 4, "thursday": 4, "fri": 5, "friday": 5, "sat": 6, "saturday": 6,
	"일": 0, "월": 1, "화": 2, "수": 3, "목": 4, "금": 5, "토": 6,
	"日": 0, "月": 1, "火": 2, "水": 3, "木": 4, "金": 5, "土": 6,
	"一": 1, "二": 2, "三": 3, "四": 4, "五": 5, "六": 6, "天": 0,
	"domingo": 0, "lunes": 1, "martes": 2, "miercoles": 3, "miércoles": 3, "jueves": 4, "viernes": 5, "sabado": 6, "sábado": 6,
}

// ParseWeekdays reads a list of weekdays as a model or a person writes it ("mon,wed", "월 수 금",
// "weekdays"); the numbers 0–6 (Sunday = 0) are accepted too. ok is false for a word it does not know.
func ParseWeekdays(s string) ([]int, bool) {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return r == ',' || r == ';' || r == '/' || r == '|' || r == '·' || r == '、' || r == ' ' || r == '\t'
	})
	var out []int
	for _, f := range fields {
		switch f {
		case "weekdays", "weekday", "평일", "平日", "工作日":
			out = append(out, 1, 2, 3, 4, 5)
			continue
		case "weekend", "weekends", "주말", "週末", "周末":
			out = append(out, 0, 6)
			continue
		}
		f = strings.TrimSuffix(f, "요일")
		f = strings.TrimSuffix(f, "曜日")
		f = strings.TrimSuffix(f, "曜")
		f = strings.TrimPrefix(strings.TrimPrefix(f, "星期"), "周")
		if n, err := strconv.Atoi(f); err == nil && n >= 0 && n <= 6 {
			out = append(out, n)
			continue
		}
		d, ok := weekdayWords[f]
		if !ok {
			return nil, false
		}
		out = append(out, d)
	}
	return out, len(out) > 0
}
