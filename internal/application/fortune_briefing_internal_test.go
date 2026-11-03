package application

import (
	"strings"
	"testing"
	"time"
)

// The slot boundaries of briefingSlot (its comment): 04:00 and 10:59 morning, 11:00 and 16:59 day,
// 17:00 and 23:59 evening (tomorrow's date), 00:00 and 03:59 night (the same date).
func TestBriefingSlotBoundaries(t *testing.T) {
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skip("no tzdata")
	}
	for _, c := range []struct {
		at, slot, target string
	}{
		{"2026-10-15 04:00", "morning", "2026-10-15"},
		{"2026-10-15 10:59", "morning", "2026-10-15"},
		{"2026-10-15 11:00", "day", "2026-10-15"},
		{"2026-10-15 16:59", "day", "2026-10-15"},
		{"2026-10-15 17:00", "evening", "2026-10-16"},
		{"2026-10-15 23:59", "evening", "2026-10-16"},
		{"2026-10-15 00:00", "night", "2026-10-15"},
		{"2026-10-15 03:59", "night", "2026-10-15"},
		{"2026-10-31 23:59", "evening", "2026-11-01"},
		{"2026-12-31 23:59", "evening", "2027-01-01"},
		{"2028-02-28 23:59", "evening", "2028-02-29"},
	} {
		at, _ := time.ParseInLocation("2006-01-02 15:04", c.at, seoul)
		slot, target := briefingSlot(at)
		if slot != c.slot || target.Format("2006-01-02") != c.target {
			t.Errorf("%s: %s %s, want %s %s", c.at, slot, target.Format("2006-01-02"), c.slot, c.target)
		}
	}
}

// The fact sheet is read for any date, tomorrow's included: no "today" or "now" in its labels, in any
// of the five languages.
func TestFortuneDataLabelsNameTheReadingDate(t *testing.T) {
	s := privacyService(t)
	at := time.Date(2026, 10, 16, 21, 0, 0, 0, time.UTC)
	p := fortuneProfile{BirthDate: "1995-06-15", BirthTime: "14:30"}
	c := s.fortuneChart(p, at, true)
	for loc, words := range map[string][]string{
		"ko":      {"오늘", "현재"},
		"en":      {"Today", "today", "now"},
		"ja":      {"今日", "現在"},
		"es":      {"hoy", "actual"},
		"zh-Hans": {"今日", "当前"},
	} {
		data := s.fortuneData(loc, p, at, c)
		for _, w := range words {
			if strings.Contains(data, w) {
				t.Errorf("%s: %q in\n%s", loc, w, data)
			}
		}
	}
}
