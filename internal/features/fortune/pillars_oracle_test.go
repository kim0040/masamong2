package fortune

import (
	"testing"
	"time"

	"github.com/6tail/lunar-go/LunarUtil"
	"github.com/6tail/lunar-go/calendar"
)

// lunar-go (MIT) is the oracle for the calendar: every expected value here is its EightChar. lunar-go
// reads a wall clock as Beijing time (UTC+8), so a Korean moment is handed to it one hour earlier.
// Its day pillar is by the Beijing date, which for 00:00–00:59 KST is the day before; the day is
// therefore compared at the same local date (noon), and the hour on the same wall clock.

var kst = time.FixedZone("KST", 9*3600)

// fromHanzi turns lunar-go's "甲子" into a Pillar.
func fromHanzi(t *testing.T, gz string) Pillar {
	t.Helper()
	r := []rune(gz)
	p := Pillar{-1, -1}
	for i, g := range LunarUtil.GAN {
		if i > 0 && g == string(r[0]) {
			p.Stem = i - 1
		}
	}
	for i, z := range LunarUtil.ZHI {
		if i > 0 && z == string(r[1]) {
			p.Branch = i - 1
		}
	}
	if p.Stem < 0 || p.Branch < 0 {
		t.Fatalf("not a 간지: %q", gz)
	}
	return p
}

// oracle is lunar-go's eight characters for a moment (as Beijing time).
func oracle(at time.Time) *calendar.EightChar {
	bj := at.In(time.FixedZone("CST", 8*3600))
	return calendar.NewSolar(bj.Year(), int(bj.Month()), bj.Day(), bj.Hour(), bj.Minute(), bj.Second()).GetLunar().GetEightChar()
}

func TestPillarsMatchLunarGoEveryDay(t *testing.T) {
	days := 0
	for d := time.Date(1930, 1, 1, 12, 0, 0, 0, kst); d.Year() <= 2040; d = d.AddDate(0, 0, 1) {
		days++
		ec := oracle(d)
		y, m, dd, _, _ := Pillars(d, -1)
		if y != fromHanzi(t, ec.GetYear()) || m != fromHanzi(t, ec.GetMonth()) || dd != fromHanzi(t, ec.GetDay()) {
			t.Fatalf("%s: %v %v %v, lunar-go %s %s %s", d.Format("2006-01-02"), y, m, dd, ec.GetYear(), ec.GetMonth(), ec.GetDay())
		}
	}
	if days < 40000 {
		t.Fatalf("only %d days", days)
	}
}

func TestPillarsAtTermBoundaries(t *testing.T) {
	terms := termMoments(1930, 2040)
	if len(terms) != 12*111 {
		t.Fatalf("%d terms", len(terms))
	}
	for _, term := range terms {
		at := term.In(kst)
		for _, off := range []time.Duration{-time.Minute, time.Minute, -24 * time.Hour, 24 * time.Hour} {
			m := at.Add(off)
			ec := oracle(m)
			y, mo, _, _, _ := PillarsAt(m, false)
			if y != fromHanzi(t, ec.GetYear()) || mo != fromHanzi(t, ec.GetMonth()) {
				t.Fatalf("%s: %v %v, lunar-go %s %s", m.Format(time.RFC3339), y, mo, ec.GetYear(), ec.GetMonth())
			}
		}
		// the minute before and after a term are different months
		_, before, _, _, _ := PillarsAt(at.Add(-time.Minute), false)
		_, after, _, _, _ := PillarsAt(at.Add(time.Minute), false)
		if before == after {
			t.Fatalf("%s: no month change", at)
		}
	}
}

func TestPillarsAtSameInstantInAnyZone(t *testing.T) {
	// a term is one instant: a minute after it is the new month in Seoul and in Madrid alike
	term := termMoments(2026, 2026)[0]
	madrid, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		t.Skip("no tzdata")
	}
	_, a, _, _, _ := PillarsAt(term.Add(time.Minute).In(kst), false)
	_, b, _, _, _ := PillarsAt(term.Add(time.Minute).In(madrid), false)
	if a != b {
		t.Fatalf("%v vs %v", a, b)
	}
}

func TestHourPillarMatchesLunarGo(t *testing.T) {
	// the hour pillar is a function of the local wall clock; lunar-go's default (sect 2) keeps the
	// date's day pillar at 23:00 and counts that 자시's stem from the next day, as PillarsAt does
	for d := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC); d.Year() < 2022; d = d.AddDate(0, 0, 7) {
		for h := 0; h < 24; h++ {
			wall := time.Date(d.Year(), d.Month(), d.Day(), h, 30, 0, 0, time.UTC)
			ec := calendar.NewSolar(wall.Year(), int(wall.Month()), wall.Day(), h, 30, 0).GetLunar().GetEightChar()
			_, _, day, hp, ok := PillarsAt(wall, true)
			if !ok || hp != fromHanzi(t, ec.GetTime()) || day != fromHanzi(t, ec.GetDay()) {
				t.Fatalf("%s: day %v hour %v, lunar-go %s %s", wall.Format("2006-01-02 15:04"), day, hp, ec.GetDay(), ec.GetTime())
			}
		}
	}
}
