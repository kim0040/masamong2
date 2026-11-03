package application

import (
	"strings"
	"testing"
	"time"

	"github.com/6tail/lunar-go/calendar"

	"masamong/internal/clock"
	"masamong/internal/features/fortune"
	"masamong/internal/tools"
)

// lunarHour is lunar-go's hour pillar for a wall clock as given.
func lunarHour(t *testing.T, y, m, d, h, mi int) fortune.Pillar {
	t.Helper()
	gz := []rune(calendar.NewSolar(y, m, d, h, mi, 0).GetLunar().GetEightChar().GetTime())
	stems, branches := []rune("甲乙丙丁戊己庚辛壬癸"), []rune("子丑寅卯辰巳午未申酉戌亥")
	p := fortune.Pillar{Stem: -1, Branch: -1}
	for i, r := range stems {
		if r == gz[0] {
			p.Stem = i
		}
	}
	for i, r := range branches {
		if r == gz[1] {
			p.Branch = i
		}
	}
	return p
}

// A Seoul birth at 01:10 is 자시 in solar time (the 30-minute default): the hour pillar is lunar-go's for
// 00:40, and the fortune data says the correction was applied. With hour_correction_minutes 0 the clock's
// 축시 comes back and the note goes.
func TestNatalHourPillarIsInSolarTime(t *testing.T) {
	s := privacyService(t)
	withZone(t, s, "Asia/Seoul")
	at := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	p := fortuneProfile{BirthDate: "2000-03-15", BirthTime: "01:10"}
	c := s.fortuneChart(p, at, false)
	if want := lunarHour(t, 2000, 3, 15, 0, 40); !c.natal.HasHour || c.natal.Hour != want || want.Branch != 0 {
		t.Fatalf("hour %+v, lunar-go at 00:40 %+v", c.natal.Hour, want)
	}
	if data := s.fortuneData("ko", p, at, c); !strings.Contains(data, "지역 태양시(UTC+8:30)") {
		t.Fatalf("no correction note:\n%s", data)
	}
	if data := s.fortuneData("en", p, at, c); !strings.Contains(data, "local solar time (UTC+8:30)") {
		t.Fatalf("no correction note in English:\n%s", data)
	}
	// no birth time: no hour pillar and no note
	if data := s.fortuneData("ko", fortuneProfile{BirthDate: "2000-03-15"}, at, s.fortuneChart(fortuneProfile{BirthDate: "2000-03-15"}, at, false)); strings.Contains(data, "태양시") {
		t.Fatalf("a note without a birth time:\n%s", data)
	}
	doc := s.Doc()
	doc.Tools = map[string]tools.Settings{"fortune.query": {Defaults: map[string]any{"hour_correction_minutes": 0}}}
	s.SetDocForTest(doc)
	c = s.fortuneChart(p, at, false)
	if want := lunarHour(t, 2000, 3, 15, 1, 10); c.natal.Hour != want || want.Branch != 1 {
		t.Fatalf("uncorrected hour %+v, lunar-go at 01:10 %+v", c.natal.Hour, want)
	}
	if data := s.fortuneData("ko", p, at, c); strings.Contains(data, "태양시") {
		t.Fatalf("a note with no correction:\n%s", data)
	}
}

// The knob is an operator default of fortune.query, so the console's tool page shows it in its defaults
// form (operator inputs are never offered to the chat model).
func TestHourCorrectionIsAConsoleDefault(t *testing.T) {
	var inputs []tools.Input
	for _, b := range tools.Builtins() {
		if b.ID == "fortune.query" {
			inputs = b.Inputs
		}
	}
	for _, in := range inputs {
		if in.Name == "hour_correction_minutes" {
			if !in.Operator || in.Type != "number" || in.Min != -120 || in.Max != 120 {
				t.Fatalf("%+v", in)
			}
			return
		}
	}
	t.Fatalf("fortune.query has no hour_correction_minutes input: %+v", inputs)
}

// One birth-date check serves registration, a stored profile and a V1 import: a calendar date from 1900
// on and not in the future.
func TestBirthDateValidator(t *testing.T) {
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	for v, want := range map[string]bool{"2000-01-01": true, "1900-01-01": true, "2026-11-03": true,
		"1899-12-31": false, "0000-05-15": false, "1990-02-30": false, "1990-2-3": false, "2026-11-04": false, "": false} {
		if _, ok := validBirthDate(v, now); ok != want {
			t.Errorf("%q: %v", v, ok)
		}
	}
	s := privacyService(t)
	if c := s.fortuneChart(fortuneProfile{BirthDate: "2999-01-01"}, now, true); c.valid || c.day != nil {
		t.Fatalf("a future birth date got a chart: %+v", c)
	}
}

// Unset, the correction is Seoul's 30 minutes only in Seoul's own zone; anywhere else it is 0 (the 30
// minutes is Seoul's distance from its meridian). A value the operator set wins in every zone.
func TestHourCorrectionDefaultFollowsTheBirthZone(t *testing.T) {
	s := privacyService(t)
	for zone, want := range map[string]int{"Asia/Seoul": 30, "Asia/Pyongyang": 30, "America/New_York": 0, "Asia/Tokyo": 0, "Australia/Sydney": 0, "": 30} {
		withZone(t, s, zone)
		if got := s.hourCorrection(); got != want {
			t.Errorf("zone %q: default %d, want %d", zone, got, want)
		}
	}
	// a chart follows: 01:10 in New York is the clock's 축시, with no solar shift and no note
	withZone(t, s, "America/New_York")
	at := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	p := fortuneProfile{BirthDate: "2000-03-15", BirthTime: "01:10"}
	c := s.fortuneChart(p, at, false)
	if want := lunarHour(t, 2000, 3, 15, 1, 10); c.natal.Hour != want || want.Branch != 1 {
		t.Fatalf("New York hour %+v, lunar-go at 01:10 %+v", c.natal.Hour, want)
	}
	if c.solar != "" {
		t.Fatalf("a note without a shift: %q", c.solar)
	}
	for zone, want := range map[string]int{"America/New_York": 45, "Asia/Seoul": 0, "Asia/Tokyo": -20} {
		withZone(t, s, zone)
		doc := s.Doc()
		doc.Tools = map[string]tools.Settings{"fortune.query": {Defaults: map[string]any{"hour_correction_minutes": want}}}
		s.SetDocForTest(doc)
		if got := s.hourCorrection(); got != want {
			t.Errorf("zone %q: set %d, got %d", zone, want, got)
		}
	}
}

// The data line names the offset of the clock the hour was read on, and only when that clock is not the
// one the birth was given on: a 1955 winter birth (the clock was already UTC+8:30) needs none.
func TestHourCorrectedLineOnlyWhenTheClockMoved(t *testing.T) {
	s := privacyService(t)
	withZone(t, s, "Asia/Seoul")
	if _, off := time.Date(1955, 7, 20, 12, 0, 0, 0, s.birthZone()).Zone(); off != 9*3600+1800 {
		t.Skipf("tzdata without Seoul's 1955 summer time (%d)", off)
	}
	at := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	line := func(date, tm string, corr *int) string {
		doc := s.Doc()
		doc.Tools = nil
		if corr != nil {
			doc.Tools = map[string]tools.Settings{"fortune.query": {Defaults: map[string]any{"hour_correction_minutes": *corr}}}
		}
		s.SetDocForTest(doc)
		p := fortuneProfile{BirthDate: date, BirthTime: tm}
		data := s.fortuneData("en", p, at, s.fortuneChart(p, at, false))
		for _, l := range strings.Split(data, "\n") {
			if strings.Contains(l, "local solar time") {
				return l
			}
		}
		return ""
	}
	zero, neg := 0, -30
	for _, c := range []struct {
		name, date, tm string
		corr           *int
		want           string // "" = no line
	}{
		{"modern Seoul", "2000-03-15", "01:10", nil, "The hour pillar uses local solar time (UTC+8:30)."},
		{"1955 winter, the clock was UTC+8:30", "1955-01-10", "13:10", nil, ""},
		{"1955 summer time UTC+9:30", "1955-07-20", "13:10", nil, "The hour pillar uses local solar time (UTC+8:30)."},
		{"1987 summer time UTC+10", "1987-08-01", "08:20", nil, "The hour pillar uses local solar time (UTC+8:30)."},
		{"correction 0, standard time", "2000-03-15", "01:10", &zero, ""},
		{"correction 0, summer time taken off", "1987-08-01", "08:20", &zero, "The hour pillar uses local solar time (UTC+9:00)."},
		{"negative correction", "2000-03-15", "01:10", &neg, "The hour pillar uses local solar time (UTC+9:30)."},
		{"no birth time", "2000-03-15", "", nil, ""},
	} {
		if got := line(c.date, c.tm, c.corr); got != c.want {
			t.Errorf("%s: %q, want %q", c.name, got, c.want)
		}
	}
	withZone(t, s, "America/New_York")
	thirty := 30
	if got := line("2000-03-15", "01:10", &thirty); got != "The hour pillar uses local solar time (UTC-5:30)." {
		t.Errorf("New York, set 30: %q", got)
	}
	if got := line("2000-03-15", "01:10", nil); got != "" {
		t.Errorf("New York, unset: %q", got)
	}
}

// The birth date is compared with today's date in the birth zone, not with the UTC instant: a baby born
// this morning in Seoul is registered at 05:00 KST (still the evening before in UTC), and a New York
// date that is still tomorrow there is not.
func TestBirthDateIsComparedInTheBirthZone(t *testing.T) {
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skip("no tzdata")
	}
	ny, _ := time.LoadLocation("America/New_York")
	for _, c := range []struct {
		name string
		now  time.Time
		date string
		want bool
	}{
		{"Seoul, today before 09:00", time.Date(2026, 11, 3, 5, 0, 0, 0, seoul), "2026-11-03", true},
		{"Seoul, tomorrow", time.Date(2026, 11, 3, 5, 0, 0, 0, seoul), "2026-11-04", false},
		{"New York, 'today' of UTC is tomorrow there", time.Date(2026, 11, 3, 2, 0, 0, 0, time.UTC).In(ny), "2026-11-03", false},
		{"New York, its own today", time.Date(2026, 11, 3, 2, 0, 0, 0, time.UTC).In(ny), "2026-11-02", true},
	} {
		if _, ok := validBirthDate(c.date, c.now); ok != c.want {
			t.Errorf("%s: %v, want %v", c.name, ok, c.want)
		}
	}
	// the stored-profile path (fortuneChart) and registration read the clock in the birth zone
	s := privacyService(t)
	withZone(t, s, "Asia/Seoul")
	s.Clock = clock.Fixed{T: time.Date(2026, 11, 2, 20, 0, 0, 0, time.UTC)} // 05:00 on the 3rd in Seoul
	if c := s.fortuneChart(fortuneProfile{BirthDate: "2026-11-03"}, s.Clock.Now(), false); !c.valid {
		t.Fatal("a birth on the zone's today got no chart")
	}
	if c := s.fortuneChart(fortuneProfile{BirthDate: "2026-11-04"}, s.Clock.Now(), false); c.valid {
		t.Fatal("tomorrow got a chart")
	}
	withZone(t, s, "America/New_York")
	s.Clock = clock.Fixed{T: time.Date(2026, 11, 3, 2, 0, 0, 0, time.UTC)} // 21:00 on the 2nd in New York
	if c := s.fortuneChart(fortuneProfile{BirthDate: "2026-11-03"}, s.Clock.Now(), false); c.valid {
		t.Fatal("a date that is still tomorrow in the birth zone got a chart")
	}
}
