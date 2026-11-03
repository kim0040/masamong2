package application

import (
	"strings"
	"testing"
	"time"

	"github.com/6tail/lunar-go/calendar"

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
	if data := s.fortuneData("ko", p, at, c); !strings.Contains(data, "태양시 기준(표준시에서 30분 보정)") {
		t.Fatalf("no correction note:\n%s", data)
	}
	if data := s.fortuneData("en", p, at, c); !strings.Contains(data, "standard time less 30 minutes") {
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
