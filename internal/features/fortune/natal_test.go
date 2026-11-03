package fortune

import (
	"testing"
	"time"

	"github.com/6tail/lunar-go/calendar"
)

func seoulZone(t *testing.T) *time.Location {
	t.Helper()
	z, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skip("no tzdata")
	}
	return z
}

// wallOracle is lunar-go's eight characters for a wall clock as given (lunar-go reads no zone).
func wallOracle(t time.Time) *calendar.EightChar {
	return calendar.NewSolar(t.Year(), int(t.Month()), t.Day(), t.Hour(), t.Minute(), t.Second()).GetLunar().GetEightChar()
}

func TestSolarHourZone(t *testing.T) {
	seoul := seoulZone(t)
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	for _, c := range []struct {
		loc  *time.Location
		corr time.Duration
		want int
	}{
		{seoul, 30 * time.Minute, 8*3600 + 1800}, // KST +9 less 30 minutes
		{seoul, 0, 9 * 3600},
		{ny, 30 * time.Minute, -5*3600 - 1800}, // the standard -5, not summer time's -4
		{time.UTC, -15 * time.Minute, 900},
	} {
		if _, off := time.Date(2026, 7, 1, 0, 0, 0, 0, SolarHourZone(c.loc, c.corr)).Zone(); off != c.want {
			t.Errorf("%s %v: %d, want %d", c.loc, c.corr, off, c.want)
		}
	}
}

// With the usual 30 minutes, a modern Seoul birth's hour pillar is that of the wall clock 30 minutes
// earlier (the expected pillars are lunar-go's for that wall clock); the year, month and day are those of
// the birth itself.
func TestNatalHourCorrection(t *testing.T) {
	seoul := seoulZone(t)
	for _, c := range []struct {
		wall, solar string // the birth's KST wall clock and its solar wall clock
		branch      int
	}{
		{"2000-03-15 01:10", "2000-03-15 00:40", 0},  // 축시 on the clock, 자시 in solar time
		{"2000-03-15 23:20", "2000-03-15 22:50", 11}, // 자시 on the clock, 해시 in solar time
		{"2000-03-15 23:40", "2000-03-15 23:10", 0},  // 자시 either way (the next day's stem)
		{"2000-03-15 00:20", "2000-03-14 23:50", 0},  // the 자시 that begins the 15th: its stem is the 15th's
		{"2000-03-15 13:29", "2000-03-15 12:59", 6},  // 미시 on the clock, 오시 in solar time
		{"2000-03-15 13:31", "2000-03-15 13:01", 7},
	} {
		wall, _ := time.ParseInLocation("2006-01-02 15:04", c.wall, seoul)
		solar, _ := time.Parse("2006-01-02 15:04", c.solar)
		n := NatalAt(wall, seoul, true, 30*time.Minute)
		want := fromHanzi(t, wallOracle(solar).GetTime())
		if !n.HasHour || n.Hour != want || n.Hour.Branch != c.branch {
			t.Errorf("%s: hour %v, want %v (lunar-go at %s)", c.wall, n.Hour, want, c.solar)
		}
		y, m, d, _, _ := PillarsAt(wall, false)
		if n.Year != y || n.Month != m || n.Day != d {
			t.Errorf("%s: year/month/day moved: %+v", c.wall, n)
		}
		// the day pillar is the birth date's (lunar-go at noon of that date)
		noon := time.Date(wall.Year(), wall.Month(), wall.Day(), 12, 0, 0, 0, time.UTC)
		if n.Day != fromHanzi(t, wallOracle(noon).GetDay()) {
			t.Errorf("%s: day %v", c.wall, n.Day)
		}
		// no correction: the wall clock's own 시진
		if n0 := NatalAt(wall, seoul, true, 0); n0.Hour != fromHanzi(t, wallOracle(wall).GetTime()) {
			t.Errorf("%s uncorrected: %v", c.wall, n0.Hour)
		}
	}
	if n := NatalAt(time.Date(2000, 3, 15, 12, 0, 0, 0, seoul), seoul, false, 30*time.Minute); n.HasHour || n.Hour != (Pillar{}) {
		t.Fatalf("no birth time, no hour pillar: %+v", n)
	}
}

// Seoul's historical offsets come from tzdata: 1955 was UTC+8:30 with summer time at UTC+9:30, 1987 had
// summer time at UTC+10. The year and month are compared with lunar-go on the instant converted to
// Beijing time, a minute either side of a 절기; the hour with lunar-go on the solar wall clock (UTC+8:30),
// which is what 만세력 tables give once summer time is taken off.
func TestNatalHistoricalSeoulOffsets(t *testing.T) {
	seoul := seoulZone(t)
	if _, off := time.Date(1955, 7, 20, 12, 0, 0, 0, seoul).Zone(); off != 9*3600+1800 {
		t.Skipf("tzdata without Seoul's 1955 summer time (%d)", off)
	}
	if _, off := time.Date(1987, 7, 20, 12, 0, 0, 0, seoul).Zone(); off != 10*3600 {
		t.Skipf("tzdata without Seoul's 1987 summer time (%d)", off)
	}
	bj := time.FixedZone("CST", 8*3600)
	solar := time.FixedZone("solar", 8*3600+1800)
	for _, c := range []struct {
		name       string
		at         time.Time // wall clock in Seoul
		hourBranch int       // in solar time
	}{
		// 소서 1955-07-08 06:05:52 Beijing = 07:35:52 KDT (+9:30); solar 06:30 and 06:40: 묘시
		{"1955 before 소서", time.Date(1955, 7, 8, 7, 30, 0, 0, seoul), 3},
		{"1955 after 소서", time.Date(1955, 7, 8, 7, 40, 0, 0, seoul), 3},
		// 13:10 KDT is 12:10 solar: 오시, where the wall clock says 미시
		{"1955 summer afternoon", time.Date(1955, 7, 20, 13, 10, 0, 0, seoul), 6},
		// 1955 winter: the clock was UTC+8:30 itself, so 13:10 stays 미시
		{"1955 winter", time.Date(1955, 1, 10, 13, 10, 0, 0, seoul), 7},
		// 소서 1987-07-07 23:38:39 Beijing = 07-08 01:38:39 KDT (+10); solar 00:08 and 00:15: 자시
		{"1987 before 소서", time.Date(1987, 7, 8, 1, 38, 0, 0, seoul), 0},
		{"1987 after 소서", time.Date(1987, 7, 8, 1, 45, 0, 0, seoul), 0},
		// 08:20 KDT is 06:50 solar: 묘시, where the clock less 30 minutes would say 진시
		{"1987 summer morning", time.Date(1987, 8, 1, 8, 20, 0, 0, seoul), 3},
	} {
		n := NatalAt(c.at, seoul, true, 30*time.Minute)
		ec := wallOracle(c.at.In(bj))
		if n.Year != fromHanzi(t, ec.GetYear()) || n.Month != fromHanzi(t, ec.GetMonth()) {
			t.Errorf("%s: %v %v, lunar-go %s %s", c.name, n.Year, n.Month, ec.GetYear(), ec.GetMonth())
		}
		hour := fromHanzi(t, wallOracle(c.at.In(solar)).GetTime())
		if n.Hour != hour || n.Hour.Branch != c.hourBranch {
			t.Errorf("%s: hour %v, lunar-go %v at %s", c.name, n.Hour, hour, c.at.In(solar).Format("15:04"))
		}
	}
	// the two 소서 pairs straddle the term: 오월 before, 미월 after
	for _, pair := range [][2]time.Time{
		{time.Date(1955, 7, 8, 7, 30, 0, 0, seoul), time.Date(1955, 7, 8, 7, 40, 0, 0, seoul)},
		{time.Date(1987, 7, 8, 1, 38, 0, 0, seoul), time.Date(1987, 7, 8, 1, 45, 0, 0, seoul)},
	} {
		_, a, _, _, _ := PillarsAt(pair[0], false)
		_, b, _, _, _ := PillarsAt(pair[1], false)
		if a.Branch != 6 || b.Branch != 7 {
			t.Errorf("%s: months %v %v", pair[0], a, b)
		}
	}
}

// Natal charts checked without lunar-go. Each pillar rests on published data:
//   - day pillars: the 갑자일 (甲子日) dates listed in Korean Wikipedia's "갑자" article (they agree with
//     KASI's 일진; 2024-01-01 is 갑자 in KASI's 2024 역서 and in newspapers' daily almanacs);
//   - years: 1984 갑자년, 1988 무진년, 2023 계묘년 (2024-01-01 is before 입춘 2024-02-04 17:27 KST);
//   - months: 년상기월법 (갑·기 years start 병인, 무·계 years start 갑인), for dates at least two days from a 절기;
//   - hours: 시두법 (갑·기 days start 갑자), from the solar wall clock (KST less 30 minutes).
func TestPublishedNatalCharts(t *testing.T) {
	seoul := seoulZone(t)
	p := func(stem, branch int) Pillar { return Pillar{Stem: stem, Branch: branch} }
	for _, c := range []struct {
		name                   string
		at                     time.Time
		year, month, day, hour Pillar
	}{
		// 무진년 정사월 갑자일 무진시: 1988-05-09 (입하 May 5, 망종 June 5). Seoul kept summer time from May 8,
		// 1988, so 10:00 on the clock (UTC+10) is 08:30 solar: 진시, not the 사시 of the clock
		{"1988-05-09 10:00 KDT", time.Date(1988, 5, 9, 10, 0, 0, 0, seoul), p(4, 4), p(3, 5), p(0, 0), p(4, 4)},
		// 1988's last 갑자일, after summer time ended (Oct 9): 1988-11-05 10:00 KST =
		// 09:30 solar, 사시 기사 (입동 Nov 7 is two days on: still 임술월, 무 year 갑인 + 8)
		{"1988-11-05 10:00 KST", time.Date(1988, 11, 5, 10, 0, 0, 0, seoul), p(4, 4), p(8, 10), p(0, 0), p(5, 5)},
		// 계묘년 갑자월 갑자일 갑자시: 2024-01-01 (대설 Dec 7, 소한 Jan 6), 01:10 KST = 00:40 solar (자시, not 축시)
		{"2024-01-01 01:10", time.Date(2024, 1, 1, 1, 10, 0, 0, seoul), p(9, 3), p(0, 0), p(0, 0), p(0, 0)},
		// 갑자년 정묘월 갑자일 을해시: 1984-03-31 (경칩 Mar 5, 청명 Apr 4), 23:20 KST = 22:50 solar (해시, not 자시)
		{"1984-03-31 23:20", time.Date(1984, 3, 31, 23, 20, 0, 0, seoul), p(0, 0), p(3, 3), p(0, 0), p(1, 11)},
		// the same day at 23:40 = 23:10 solar: 자시, counted from the next day's 을 (병자); the day stays 갑자 (야자시)
		{"1984-03-31 23:40", time.Date(1984, 3, 31, 23, 40, 0, 0, seoul), p(0, 0), p(3, 3), p(0, 0), p(2, 0)},
	} {
		n := NatalAt(c.at, seoul, true, 30*time.Minute)
		if n.Year != c.year || n.Month != c.month || n.Day != c.day || n.Hour != c.hour {
			t.Errorf("%s: %v %v %v %v, want %v %v %v %v", c.name, n.Year, n.Month, n.Day, n.Hour, c.year, c.month, c.day, c.hour)
		}
	}
}

// 입춘 to the minute, as KASI publishes it (the reviewer's check): 2024-02-04 17:27, 2025-02-03 23:10,
// 2026-02-04 05:02 KST. A minute before it is still the old 간지 year.
func TestLichunInstants(t *testing.T) {
	for year, want := range map[int]string{2024: "2024-02-04 17:27", 2025: "2025-02-03 23:10", 2026: "2026-02-04 05:02"} {
		at := lichun(year).In(kst)
		if got := at.Truncate(time.Minute).Format("2006-01-02 15:04"); got != want {
			t.Errorf("%d: %s, want %s", year, got, want)
		}
		before, _, _, _, _ := PillarsAt(at.Add(-time.Minute), false)
		after, _, _, _, _ := PillarsAt(at.Add(time.Minute), false)
		if before.Branch != mod(year-1-4, 12) || after.Branch != mod(year-4, 12) {
			t.Errorf("%d: years %v %v", year, before, after)
		}
	}
}
