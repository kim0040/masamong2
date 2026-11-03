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

var jieCache sync.Map // year -> []jie, for jieCacheFrom…jieCacheTo only

// The years whose terms are kept once computed: every birth date a profile accepts and every day the
// bot runs. A year outside them (a library caller's) is computed each time instead of growing the cache.
const (
	jieCacheFrom = 1900
	jieCacheTo   = 2200
)

// jieOf are the month-starting terms lunar-go lists for a year: from the previous year's 대설 to the
// next year's 경칩, in time order.
func jieOf(year int) []jie {
	cached := year >= jieCacheFrom && year <= jieCacheTo
	if v, ok := jieCache.Load(year); cached && ok {
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
	if cached {
		jieCache.Store(year, out)
	}
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

// DatePillars are the year, month and day pillars of a whole civil date in loc, the way an almanac
// reads a day (lunar-go's default sect): the year and month are the ones in force at the end of the
// date, so a term date is the new month from 00:00, and every moment of one date gives the same pillars.
// A natal chart uses PillarsAt, the exact moment, instead.
func DatePillars(y int, m time.Month, d int, loc *time.Location) (year, month, day Pillar) {
	end := time.Date(y, m, d+1, 0, 0, 0, 0, loc).Add(-time.Nanosecond)
	year, month, _, _, _ = PillarsAt(end, false)
	return year, month, dayPillar(y, m, d)
}

// TermOn reports whether a month-starting term (절기) falls on the civil date in loc, and the branch
// of the month it starts (2 is 입춘, which also starts the year). Without a birth time, a birth on such a
// date has an uncertain month pillar (and on 입춘 an uncertain year pillar).
func TermOn(y int, m time.Month, d int, loc *time.Location) (branch int, ok bool) {
	from := time.Date(y, m, d, 0, 0, 0, 0, loc)
	to := time.Date(y, m, d+1, 0, 0, 0, 0, loc)
	for _, year := range []int{from.UTC().Year(), to.UTC().Year()} {
		for _, j := range jieOf(year) {
			if !j.at.Before(from) && j.at.Before(to) {
				return j.branch, true
			}
		}
	}
	return 0, false
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
