// Package fortune computes the astronomical and calendar facts a fortune is written from (V1
// utils/fortune.py, which used ephem and korean-lunar-calendar): the Western sun sign of a birthday,
// the sky's planets by sign for a moment, and the Four Pillars (사주) stems and branches. Everything
// here is indices; names come from the message catalogs (fortune.sign.<i>, fortune.stem.<i>,
// fortune.branch.<i>, fortune.planet.<id>), so no language is built in.
package fortune

import (
	"math"
	"time"
)

// Sign indices: 0 Aries … 11 Pisces.
const Signs = 12

// SunSign is the tropical sign of a birthday (Aries 3/21–4/19 … Pisces 2/19–3/20).
func SunSign(month, day int) int {
	starts := [12][2]int{{3, 21}, {4, 20}, {5, 21}, {6, 22}, {7, 23}, {8, 23}, {9, 23}, {10, 23}, {11, 22}, {12, 22}, {1, 20}, {2, 19}}
	md := month*100 + day
	sign := 9 // Capricorn: 12/22–1/19
	for i, s := range starts {
		if i == 9 {
			continue
		}
		start := s[0]*100 + s[1]
		end := starts[(i+1)%12][0]*100 + starts[(i+1)%12][1]
		if md >= start && md < end {
			sign = i
		}
	}
	return sign
}

// julianDay of a UTC moment.
func julianDay(t time.Time) float64 {
	return float64(t.UTC().UnixNano())/86400e9 + 2440587.5
}

func deg(r float64) float64 { return r * 180 / math.Pi }
func rad(d float64) float64 { return d * math.Pi / 180 }
func norm360(d float64) float64 {
	d = math.Mod(d, 360)
	if d < 0 {
		d += 360
	}
	return d
}

// keplerian elements and their rates per Julian century (E. M. Standish, "Keplerian Elements for
// Approximate Positions of the Major Planets", table 1, valid 1800–2050): a, e, I, L, long. perihelion,
// long. ascending node.
type elements struct{ a, e, i, l, w, n, da, de, di, dl, dw, dn float64 }

var planets = map[string]elements{
	"mercury": {0.38709927, 0.20563593, 7.00497902, 252.25032350, 77.45779628, 48.33076593, 0.00000037, 0.00001906, -0.00594749, 149472.67411175, 0.16047689, -0.12534081},
	"venus":   {0.72333566, 0.00677672, 3.39467605, 181.97909950, 131.60246718, 76.67984255, 0.00000390, -0.00004107, -0.00078890, 58517.81538729, 0.00268329, -0.27769418},
	"earth":   {1.00000261, 0.01671123, -0.00001531, 100.46457166, 102.93768193, 0, 0.00000562, -0.00004392, -0.01294668, 35999.37244981, 0.32327364, 0},
	"mars":    {1.52371034, 0.09339410, 1.84969142, -4.55343205, -23.94362959, 49.55953891, 0.00001847, 0.00007882, -0.00813131, 19140.30268499, 0.44441088, -0.29257343},
	"jupiter": {5.20288700, 0.04838624, 1.30439695, 34.39644051, 14.72847983, 100.47390909, -0.00011607, -0.00013253, -0.00183714, 3034.74612775, 0.21252668, 0.20469106},
	"saturn":  {9.53667594, 0.05386179, 2.48599187, 49.95424423, 92.59887831, 113.66242448, -0.00125060, -0.00050991, 0.00193609, 1222.49362201, -0.41897216, -0.28867794},
}

// heliocentric ecliptic coordinates (AU) of a planet at T centuries from J2000.
func helio(el elements, T float64) (x, y, z float64) {
	a, e := el.a+el.da*T, el.e+el.de*T
	I, L, W, N := rad(el.i+el.di*T), el.l+el.dl*T, el.w+el.dw*T, el.n+el.dn*T
	w := rad(W - N)
	M := rad(norm360(L - W))
	E := M + e*math.Sin(M)
	for k := 0; k < 8; k++ {
		E -= (E - e*math.Sin(E) - M) / (1 - e*math.Cos(E))
	}
	xp, yp := a*(math.Cos(E)-e), a*math.Sqrt(1-e*e)*math.Sin(E)
	Nr := rad(N)
	cw, sw, cn, sn, ci, si := math.Cos(w), math.Sin(w), math.Cos(Nr), math.Sin(Nr), math.Cos(I), math.Sin(I)
	x = (cw*cn-sw*sn*ci)*xp + (-sw*cn-cw*sn*ci)*yp
	y = (cw*sn+sw*cn*ci)*xp + (-sw*sn+cw*cn*ci)*yp
	z = (sw*si)*xp + (cw*si)*yp
	return
}

// Longitude is the geocentric ecliptic longitude (degrees) of a body: sun, moon, mercury, venus,
// mars, jupiter or saturn. Accurate to about a degree between 1800 and 2050 — enough for signs.
func Longitude(body string, t time.Time) float64 {
	jd := julianDay(t)
	d := jd - 2451545.0
	T := d / 36525
	switch body {
	case "moon":
		L := 218.316 + 13.176396*d
		M := rad(134.963 + 13.064993*d)
		return norm360(L + 6.289*math.Sin(M))
	case "sun":
		ex, ey, _ := helio(planets["earth"], T)
		return norm360(deg(math.Atan2(-ey, -ex)))
	}
	el, ok := planets[body]
	if !ok {
		return 0
	}
	px, py, _ := helio(el, T)
	ex, ey, _ := helio(planets["earth"], T)
	return norm360(deg(math.Atan2(py-ey, px-ex)))
}

// Bodies are the sky positions a fortune lists, in order.
var Bodies = []string{"sun", "moon", "mercury", "venus", "mars", "jupiter", "saturn"}

// SkySigns is each body's sign at t.
func SkySigns(t time.Time) map[string]int {
	out := map[string]int{}
	for _, b := range Bodies {
		out[b] = int(Longitude(b, t) / 30)
	}
	return out
}

// Pillar is one of the Four Pillars: a heavenly stem (0 갑 … 9 계) and an earthly branch (0 자 … 11 해).
type Pillar struct{ Stem, Branch int }

func mod(a, n int) int { return ((a % n) + n) % n }

// Pillars are the year, month and day pillars of a local date, and the hour pillar when hour >= 0. The
// date's wall clock is read in its own Location at hour:00 (or noon when the hour is not known), so the
// caller decides the zone; PillarsAt is the same for an exact moment.
func Pillars(date time.Time, hour int) (year, month, day, hourP Pillar, hasHour bool) {
	h := 12
	if hour >= 0 {
		h = hour
	}
	t := time.Date(date.Year(), date.Month(), date.Day(), h, 0, 0, 0, date.Location())
	return PillarsAt(t, hour >= 0)
}

// PillarsAt are the four pillars of a moment, read in t's own Location (the person's zone).
//
//   - Year and month change at the instants of 입춘 and each 절기 (calendar.go, from lunar-go), compared
//     as absolute moments, so the boundary is exact to the minute in any zone.
//   - The day pillar follows the local civil date (Julian day number). It does not roll over at 23:00:
//     23:00–23:59 keeps the date's day pillar (the 야자시 convention, lunar-go's default "sect 2").
//   - The hour pillar is the two-hour 시진 of the local wall clock (자시 23:00–00:59). Its stem follows
//     the day it starts: 23:00–23:59 is the 자시 that begins the next day, so its stem is counted from
//     the next day's stem (a 무 day's 23:30 is 갑자, its 00:30 is 임자). Both 자시 conventions agree on
//     this; they differ only in the day pillar.
//
// Local mean solar time is not corrected for (Seoul's clock runs about 30 minutes ahead of the sun);
// a zone with historical offsets (Asia/Seoul's 1950s UTC+8:30 and summer time) is applied as the
// time package knows it.
func PillarsAt(t time.Time, hasHour bool) (year, month, day, hourP Pillar, ok bool) {
	gy, branch := solarMonth(t)
	year = Pillar{Stem: mod(gy-4, 10), Branch: mod(gy-4, 12)}
	m := mod(branch-2, 12) // 0 = 인월
	month = Pillar{Stem: mod(year.Stem%5*2+2+m, 10), Branch: branch}
	day = dayPillar(t.Year(), t.Month(), t.Day())
	if hasHour {
		b := mod((t.Hour()+1)/2, 12) // 자시 23:00–00:59
		stem := day.Stem
		if t.Hour() == 23 {
			stem = (stem + 1) % 10 // the 자시 that starts the next day
		}
		hourP, ok = Pillar{Stem: mod(stem%5*2+b, 10), Branch: b}, true
	}
	return
}

// dayPillar of a civil date, from its Julian day number (2000-01-01 is 무오).
func dayPillar(y int, m time.Month, d int) Pillar {
	jdn := JDN(y, m, d)
	return Pillar{Stem: mod(jdn+9, 10), Branch: mod(jdn+1, 12)}
}

// JDN is the Julian day number of a civil date; consecutive dates differ by one.
func JDN(y int, m time.Month, d int) int {
	noon := time.Date(y, m, d, 12, 0, 0, 0, time.UTC)
	return int(math.Floor(julianDay(noon) + 0.5))
}
