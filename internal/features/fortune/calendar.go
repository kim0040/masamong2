package fortune

import (
	"math"
	"sort"
	"sync"
	"time"

	"github.com/6tail/lunar-go/calendar"
)

// The solar terms come from lunar-go (github.com/6tail/lunar-go, MIT, a port of 寿星万年历's
// algorithms). lunar-go is a Chinese calendar: its term instants are Beijing time (UTC+8). They are
// turned into absolute moments here (minus eight hours), so a Korean (UTC+9) or any other zone compares
// against the same instant; a term that falls at 23:30 in Beijing is at 00:30 the next day in Seoul.

// jie is one of the twelve 절기 that start a month, and the branch of the month it starts.
type jie struct {
	at     time.Time
	branch int
}

var jieCache sync.Map // year -> []jie

// jieOf are the month-starting terms lunar-go lists for a year: from the previous year's 대설 to the
// next year's 경칩, in time order.
func jieOf(year int) []jie {
	if v, ok := jieCache.Load(year); ok {
		return v.([]jie)
	}
	// lunar-go's table for a year: index 0 is the previous 大雪, then every term from 冬至 to the next
	// year's 惊蛰; the even indices are the 节 (大雪 子, 小寒 丑, 立春 寅, … 大雪 子, 小寒 丑, 立春 寅, 惊蛰 卯).
	jds := calendar.NewLunarYear(year).GetJieQiJulianDays()
	out := make([]jie, 0, len(jds)/2+1)
	for i := 0; i < len(jds); i += 2 {
		out = append(out, jie{at: beijingJD(jds[i]), branch: (i / 2) % 12})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].at.Before(out[b].at) })
	jieCache.Store(year, out)
	return out
}

// beijingJD is the moment of a Julian day given in Beijing time.
func beijingJD(jd float64) time.Time {
	sec := math.Round((jd - 2440587.5) * 86400)
	return time.Unix(int64(sec), 0).Add(-8 * time.Hour).UTC()
}

// solarMonth is the 간지 year (changing at 입춘) and the month branch (changing at each 절기) of a moment.
func solarMonth(t time.Time) (ganzhiYear, branch int) {
	u := t.UTC()
	terms := jieOf(u.Year())
	branch = 0
	for _, j := range terms {
		if j.at.After(u) {
			break
		}
		branch = j.branch
	}
	ganzhiYear = u.Year()
	if u.Before(lichun(u.Year())) {
		ganzhiYear--
	}
	return ganzhiYear, branch
}

// lichun is the moment of 입춘 in a Gregorian year.
func lichun(year int) time.Time {
	for _, j := range jieOf(year) {
		if j.branch == 2 && j.at.Year() == year {
			return j.at
		}
	}
	return time.Date(year, 2, 4, 0, 0, 0, 0, time.UTC) // not reached: every year's table has its 입춘
}

// termMoments are the month-starting term instants that fall in the Gregorian years from…to (for tests).
func termMoments(from, to int) []time.Time {
	var out []time.Time
	for y := from; y <= to; y++ {
		for _, j := range jieOf(y) {
			if j.at.Year() == y {
				out = append(out, j.at)
			}
		}
	}
	return out
}
