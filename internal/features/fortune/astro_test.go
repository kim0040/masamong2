package fortune

import (
	"math"
	"testing"
	"time"
)

func TestSunSignBoundaries(t *testing.T) {
	for _, c := range []struct{ m, d, want int }{
		{3, 21, 0}, {4, 19, 0}, {4, 20, 1}, {12, 21, 8}, {12, 22, 9}, {1, 19, 9}, {1, 20, 10}, {2, 18, 10}, {2, 19, 11}, {3, 20, 11}, {9, 23, 6}, {10, 22, 6}, {10, 23, 7},
	} {
		if got := SunSign(c.m, c.d); got != c.want {
			t.Fatalf("%d/%d: %d want %d", c.m, c.d, got, c.want)
		}
	}
}

func angle(a, b float64) float64 {
	d := math.Abs(math.Mod(a-b+540, 360) - 180)
	return d
}

func TestLongitudesAgainstKnownSky(t *testing.T) {
	// the March 2026 equinox (2026-03-20 14:46 UTC): the sun at 0°
	if l := Longitude("sun", time.Date(2026, 3, 20, 14, 46, 0, 0, time.UTC)); angle(l, 0) > 0.5 {
		t.Fatalf("sun at the equinox: %.2f", l)
	}
	// the June 2026 solstice (2026-06-21 08:24 UTC): 90°
	if l := Longitude("sun", time.Date(2026, 6, 21, 8, 24, 0, 0, time.UTC)); angle(l, 90) > 0.5 {
		t.Fatalf("sun at the solstice: %.2f", l)
	}
	// Jupiter was in Cancer from mid-2025 to mid-2026; Saturn entered Aries in 2026
	if s := SkySigns(time.Date(2026, 1, 15, 0, 0, 0, 0, time.UTC)); s["jupiter"] != 3 {
		t.Fatalf("jupiter sign %d", s["jupiter"])
	}
	// full moon 2026-01-03 10:03 UTC: moon opposite the sun
	at := time.Date(2026, 1, 3, 10, 3, 0, 0, time.UTC)
	if d := angle(Longitude("moon", at), Longitude("sun", at)+180); d > 3 {
		t.Fatalf("moon %.1f° off opposition", d)
	}
}

func TestPillars(t *testing.T) {
	// 2000-01-01 is a 무오 day in the year 기묘 (before 입춘) and the month 병자
	y, m, d, _, _ := Pillars(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), -1)
	if d != (Pillar{4, 6}) {
		t.Fatalf("day %+v", d)
	}
	if y != (Pillar{5, 3}) {
		t.Fatalf("year %+v", y)
	}
	if m != (Pillar{2, 0}) {
		t.Fatalf("month %+v", m)
	}
	// 2024-02-10 (after 입춘): 갑진년 병인월
	y, m, _, _, _ = Pillars(time.Date(2024, 2, 10, 0, 0, 0, 0, time.UTC), -1)
	if y != (Pillar{0, 4}) || m != (Pillar{2, 2}) {
		t.Fatalf("2024-02-10: %+v %+v", y, m)
	}
	// hour: 23:30 is 자시; a 무 day's 자시 is 임자
	_, _, _, h, ok := Pillars(time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC), 23)
	if !ok || h != (Pillar{8, 0}) {
		t.Fatalf("hour %+v", h)
	}
}
