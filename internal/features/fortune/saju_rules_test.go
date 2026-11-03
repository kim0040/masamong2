package fortune

import (
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/6tail/lunar-go/calendar"
)

// The rule tables of saju.go written again here as data, in the hanzi a reference book prints them in,
// so a test does not read saju.go's own arrays back.

const zhiOrder = "子丑寅卯辰巳午未申酉戌亥"

func zhi(t *testing.T, s string) int {
	t.Helper()
	i := slices.Index([]rune(zhiOrder), []rune(s)[0])
	if i < 0 {
		t.Fatalf("not a branch: %q", s)
	}
	return i
}

func zhis(t *testing.T, s string) []int {
	t.Helper()
	var out []int
	for _, r := range s {
		out = append(out, zhi(t, string(r)))
	}
	return out
}

func TestStarsEveryStemAndTriad(t *testing.T) {
	// 天乙貴人: 甲戊庚牛羊, 乙己鼠猴鄉, 丙丁豬雞位, 壬癸蛇兔藏, 六辛逢馬虎
	nobleman := map[int]string{0: "丑未", 4: "丑未", 6: "丑未", 1: "子申", 5: "子申", 2: "亥酉", 3: "亥酉", 8: "巳卯", 9: "巳卯", 7: "寅午"}
	// 文昌: 甲巳 乙午 丙申 丁酉 戊申 己酉 庚亥 辛子 壬寅 癸卯
	scholar := "巳午申酉申酉亥子寅卯"
	for stem := 0; stem < 10; stem++ {
		// a natal chart whose year and day branches are both 子 (申子辰): its 역마 寅, 도화 酉, 화개 辰
		// are left out of the check below
		n := Natal{Year: Pillar{0, 0}, Day: Pillar{stem, 0}}
		for b := 0; b < 12; b++ {
			got := stars(n, b)
			wantNoble := slices.Contains(zhis(t, nobleman[stem]), b)
			wantScholar := zhi(t, string([]rune(scholar)[stem])) == b
			if slices.Contains(got, StarNobleman) != wantNoble || slices.Contains(got, StarScholar) != wantScholar {
				t.Fatalf("stem %d on branch %d: %v (noble %v scholar %v)", stem, b, got, wantNoble, wantScholar)
			}
		}
	}
	// 驛馬 / 桃花 / 華蓋 by the triad of the year or day branch: 申子辰 寅酉辰, 巳酉丑 亥午丑,
	// 寅午戌 申卯戌, 亥卯未 巳子未
	triads := []struct{ members, travel, peach, canopy string }{
		{"申子辰", "寅", "酉", "辰"}, {"巳酉丑", "亥", "午", "丑"}, {"寅午戌", "申", "卯", "戌"}, {"亥卯未", "巳", "子", "未"},
	}
	for _, tr := range triads {
		for _, member := range zhis(t, tr.members) {
			// once by the year branch, once by the day branch (the other one a branch of another triad
			// whose own stars are none of these)
			for _, n := range []Natal{{Year: Pillar{0, member}, Day: Pillar{2, member}}} {
				for b := 0; b < 12; b++ {
					got := stars(n, b)
					for _, c := range []struct {
						star int
						at   string
					}{{StarTravel, tr.travel}, {StarPeach, tr.peach}, {StarCanopy, tr.canopy}} {
						if want := zhi(t, c.at) == b; slices.Contains(got, c.star) != want {
							t.Fatalf("triad %s, member %d, branch %d: star %d %v", tr.members, member, b, c.star, got)
						}
					}
				}
			}
			for _, other := range []int{PosYear, PosDay} {
				n := Natal{Year: Pillar{0, member}, Day: Pillar{2, member}}
				if other == PosYear {
					n.Day.Branch = (member + 1) % 12 // the next triad over: its stars fall elsewhere
				} else {
					n.Year.Branch = (member + 1) % 12
				}
				if !slices.Contains(stars(n, zhi(t, tr.travel)), StarTravel) {
					t.Fatalf("triad %s by position %d alone: no 역마", tr.members, other)
				}
			}
		}
	}
}

func TestRelationsEveryPairFromTheRules(t *testing.T) {
	// 반합 (two branches of 申子辰 巳酉丑 寅午戌 亥卯未, one of them the cardinal 子午卯酉), 刑 (寅巳申,
	// 丑戌未, 子卯, and 辰午酉亥 with themselves), 破 (子酉 丑辰 寅亥 卯午 巳申 未戌), 害 (子未 丑午 寅巳
	// 卯辰 申亥 酉戌). 육합 and 충 are lunar-go's (TestRelationsMatchLunarGo).
	half := map[[2]int]bool{}
	for _, tr := range []string{"申子辰", "巳酉丑", "寅午戌", "亥卯未"} {
		m := zhis(t, tr)
		cardinal := m[1] // the middle one of each triad as written is its cardinal
		for _, a := range m {
			for _, b := range m {
				if a != b && (a == cardinal || b == cardinal) {
					half[[2]int{a, b}] = true
				}
			}
		}
	}
	pairs := func(groups ...string) map[[2]int]bool {
		out := map[[2]int]bool{}
		for _, g := range groups {
			p := zhis(t, g)
			out[[2]int{p[0], p[1]}], out[[2]int{p[1], p[0]}] = true, true
		}
		return out
	}
	punish := pairs("寅巳", "巳申", "寅申", "丑戌", "戌未", "丑未", "子卯", "辰辰", "午午", "酉酉", "亥亥")
	brk := pairs("子酉", "丑辰", "寅亥", "卯午", "巳申", "未戌")
	harm := pairs("子未", "丑午", "寅巳", "卯辰", "申亥", "酉戌")
	for a := 0; a < 12; a++ {
		for b := 0; b < 12; b++ {
			rels := Relate(a, b)
			for _, c := range []struct {
				rel  int
				want bool
			}{{RelHalfTriad, half[[2]int{a, b}]}, {RelPunish, punish[[2]int{a, b}]}, {RelBreak, brk[[2]int{a, b}]}, {RelHarm, harm[[2]int{a, b}]}} {
				if slices.Contains(rels, c.rel) != c.want {
					t.Fatalf("%c%c relation %d: got %v", []rune(zhiOrder)[a], []rune(zhiOrder)[b], c.rel, rels)
				}
			}
		}
	}
}

func TestHalfTriadNeedsTheCardinal(t *testing.T) {
	// 인술 사축 신진 해미: two of a triad without its cardinal are no relation
	for _, p := range []string{"寅戌", "巳丑", "申辰", "亥未"} {
		b := zhis(t, p)
		if got := Relate(b[0], b[1]); got != nil {
			t.Fatalf("%s: %v", p, got)
		}
	}
}

func TestEachRelationKindScoresOnce(t *testing.T) {
	// on a 인 day, a chart with 오 at day and month has two 반합 hits; it scores as one, the day's. A chart
	// with 자 at month instead (no relation with 인) has the same scores.
	today := Pillar{0, 2} // 갑인
	month := Pillar{0, 2}
	two := Natal{Year: Pillar{1, 1}, Month: Pillar{2, 6}, Day: Pillar{4, 6}}
	one := Natal{Year: Pillar{1, 1}, Month: Pillar{2, 0}, Day: Pillar{4, 6}}
	a, b := AnalyzeDay(two, month, today, 1), AnalyzeDay(one, month, today, 1)
	if !slices.Contains(a.Hits, Hit{PosMonth, RelHalfTriad}) || !slices.Contains(a.Hits, Hit{PosDay, RelHalfTriad}) {
		t.Fatalf("hits %v", a.Hits)
	}
	if a.Scores != b.Scores {
		t.Fatalf("two hits of one kind %+v, one hit %+v", a.Scores, b.Scores)
	}
}

func TestSameDateSameDay(t *testing.T) {
	// 2026-10-08 is 한로 (15:29 KST): the morning and the evening of that date are one almanac day
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skip("no tzdata")
	}
	n := Natal{Year: Pillar{6, 6}, Month: Pillar{4, 2}, Day: Pillar{0, 0}, Hour: Pillar{2, 2}, HasHour: true}
	for _, zone := range []*time.Location{seoul, kst} {
		morning := Analyze(n, time.Date(2026, 10, 8, 7, 30, 0, 0, zone))
		evening := Analyze(n, time.Date(2026, 10, 8, 18, 0, 0, 0, zone))
		if !reflect.DeepEqual(morning, evening) {
			t.Fatalf("%s:\n07:30 %+v\n18:00 %+v", zone, morning, evening)
		}
		// and it is the new month's day all day (the term date), like lunar-go's almanac
		if morning.TodayMonth.Branch != 10 {
			t.Fatalf("month branch %d", morning.TodayMonth.Branch)
		}
	}
}

func TestAnalyzeMatchesLunarGoAlmanac(t *testing.T) {
	// lunar-go's day almanac (its default sect: the month changes at 00:00 of the term date). lunar-go
	// dates are Beijing dates, so the person's zone here is UTC+8; in another zone the term date is the
	// local date of the same instant.
	cst := time.FixedZone("CST", 8*3600)
	spiritNames := []string{"青龙", "明堂", "天刑", "朱雀", "金匮", "天德", "白虎", "玉堂", "天牢", "玄武", "司命", "勾陈"}
	n := Natal{Year: Pillar{6, 6}, Month: Pillar{4, 2}, Day: Pillar{0, 0}}
	for d := time.Date(2000, 1, 1, 0, 0, 0, 0, cst); d.Year() < 2031; d = d.AddDate(0, 0, 1) {
		l := calendar.NewSolar(d.Year(), int(d.Month()), d.Day(), 12, 0, 0).GetLunar()
		for _, h := range []int{0, 7, 18, 23} {
			at := time.Date(d.Year(), d.Month(), d.Day(), h, 30, 0, 0, cst)
			day := Analyze(n, at)
			if day.TodayMonth != fromHanzi(t, l.GetMonthInGanZhi()) || day.Today != fromHanzi(t, l.GetDayInGanZhi()) {
				t.Fatalf("%s: month %v day %v, lunar-go %s %s", at.Format("2006-01-02 15:04"), day.TodayMonth, day.Today, l.GetMonthInGanZhi(), l.GetDayInGanZhi())
			}
			officer := []string{"建", "除", "满", "平", "定", "执", "破", "危", "成", "收", "开", "闭"}
			if officer[day.Officer] != l.GetZhiXing() || spiritNames[day.Spirit] != l.GetDayTianShen() {
				t.Fatalf("%s: officer %s spirit %s, lunar-go %s %s", at.Format("2006-01-02 15:04"), officer[day.Officer], spiritNames[day.Spirit], l.GetZhiXing(), l.GetDayTianShen())
			}
			yi, ji := Activities(fromHanzi(t, l.GetMonthInGanZhi()), day.Today)
			if !reflect.DeepEqual(day.Yi, yi) || !reflect.DeepEqual(day.Ji, ji) {
				t.Fatalf("%s: yi %v ji %v", at.Format("2006-01-02"), day.Yi, day.Ji)
			}
			// the groups come from lunar-go's own day lists
			for _, g := range day.Yi {
				if !slices.ContainsFunc(names(l.GetDayYi()), func(n string) bool { return activityOf[n] == g }) {
					t.Fatalf("%s: %q not in lunar-go's 宜 %v", at.Format("2006-01-02"), g, strings.Join(names(l.GetDayYi()), " "))
				}
			}
			for _, g := range day.Ji {
				if !slices.ContainsFunc(names(l.GetDayJi()), func(n string) bool { return activityOf[n] == g }) {
					t.Fatalf("%s: %q not in lunar-go's 忌", at.Format("2006-01-02"), g)
				}
			}
		}
	}
}

func TestLuckyDirectionVariesWithTheDay(t *testing.T) {
	// the lucky element is the weakest of the natal characters plus today's two, so it moves with the day
	n := Natal{Year: Pillar{6, 6}, Month: Pillar{4, 2}, Day: Pillar{0, 0}, Hour: Pillar{2, 2}, HasHour: true}
	seen := map[int]bool{}
	for i := 0; i < 30; i++ {
		seen[Analyze(n, time.Date(2026, 11, 1+i, 9, 0, 0, 0, kst)).Lucky.Direction] = true
	}
	if len(seen) < 2 {
		t.Fatalf("one direction all month: %v", seen)
	}
}

func TestLuckyElementOfTheDay(t *testing.T) {
	// natal 경오 무인 갑자 (목2 화1 토1 금1 수1); today 병오 (화화): 목2 화3 토1 금1 수1 → 토 금 수 tie, the
	// tie-break from 갑's 인성 (수) gives 수. Today 임자 (수수): 목2 화1 토1 금1 수3 → 화 토 금 tie; from
	// 수: 목(2) 화(1) → 화.
	n := Natal{Year: Pillar{6, 6}, Month: Pillar{4, 2}, Day: Pillar{0, 0}}
	if d := AnalyzeDay(n, Pillar{4, 6}, Pillar{2, 6}, 2460000); d.Lucky.Direction != 4 || d.Weakest != 4 {
		t.Fatalf("병오: %+v weakest %d", d.Lucky, d.Weakest)
	}
	d := AnalyzeDay(n, Pillar{4, 6}, Pillar{8, 0}, 2460000)
	// k = (2460000 + 8) mod 3 = 2: colour 1*3+2 = 5, item 1*3+0 = 3; number 2 (even day)
	if d.Lucky != (Lucky{Color: 5, Item: 3, Direction: 1, Number: 2}) || d.Weakest != 4 {
		t.Fatalf("임자: %+v weakest %d", d.Lucky, d.Weakest)
	}
}

func TestBirthMomentKeepsTheWallClockInADSTGap(t *testing.T) {
	// 1987-05-10 02:00 KST jumped to 03:00 KDT: a birth "02:30" is read at the offset before the gap
	// (+9), so its hour is still 축 (01:00–03:00) and its date the 10th
	seoul, err := time.LoadLocation("Asia/Seoul")
	if err != nil {
		t.Skip("no tzdata")
	}
	b := BirthMoment(1987, 5, 10, 2, 30, seoul)
	if b.Hour() != 2 || b.Minute() != 30 || b.Day() != 10 {
		t.Fatalf("%v", b)
	}
	if _, off := b.Zone(); off != 9*3600 {
		t.Fatalf("offset %d", off)
	}
	if _, _, _, h, _ := PillarsAt(b, true); h.Branch != 1 {
		t.Fatalf("hour branch %d", h.Branch)
	}
	// a time outside a gap is the zone's own
	if b := BirthMoment(1987, 5, 10, 4, 0, seoul); b.Hour() != 4 || b.Location() != seoul {
		t.Fatalf("%v", b)
	}
	if lon, err := time.LoadLocation("Europe/London"); err == nil {
		if b := BirthMoment(1995, 3, 26, 1, 30, lon); b.Hour() != 1 || b.Minute() != 30 {
			t.Fatalf("London %v", b)
		}
	}
}

func TestTermOn(t *testing.T) {
	// 2026-02-04 is 입춘 (05:02 KST), 2026-10-08 한로 (술월), 2026-10-09 no term
	if b, ok := TermOn(2026, 2, 4, kst); !ok || b != 2 {
		t.Fatalf("입춘 %d %v", b, ok)
	}
	if b, ok := TermOn(2026, 10, 8, kst); !ok || b != 10 {
		t.Fatalf("한로 %d %v", b, ok)
	}
	if _, ok := TermOn(2026, 10, 9, kst); ok {
		t.Fatal("no term on 10-09")
	}
	// 2026-06-06 00:48 KST is 06-05 in Beijing: the term date follows the zone
	if _, ok := TermOn(2026, 6, 6, kst); !ok {
		t.Fatal("망종 in Seoul")
	}
	if _, ok := TermOn(2026, 6, 5, time.FixedZone("CST", 8*3600)); !ok {
		t.Fatal("망종 in Beijing")
	}
}

func TestJDNOutsideTheNanosecondRange(t *testing.T) {
	// Fliegel & Van Flandern's integer formula for the Gregorian calendar
	want := func(y, m, d int) int {
		a := (14 - m) / 12
		yy, mm := y+4800-a, m+12*a-3
		return d + (153*mm+2)/5 + 365*yy + yy/4 - yy/100 + yy/400 - 32045
	}
	for _, c := range [][3]int{{1600, 1, 1}, {1677, 9, 20}, {2000, 1, 1}, {2262, 4, 12}, {2400, 1, 1}, {3000, 6, 15}} {
		if got := JDN(c[0], time.Month(c[1]), c[2]); got != want(c[0], c[1], c[2]) {
			t.Fatalf("%v: %d want %d", c, got, want(c[0], c[1], c[2]))
		}
	}
}

func TestJieCacheIsBounded(t *testing.T) {
	jieOf(2999)
	if _, ok := jieCache.Load(2999); ok {
		t.Fatal("a year outside the cached range was kept")
	}
	jieOf(2026)
	if _, ok := jieCache.Load(2026); !ok {
		t.Fatal("a year in range was not kept")
	}
}

func TestHourSpiritAtLateRat(t *testing.T) {
	// 23:30 is the 자시 that starts the next day: lunar-go names its spirit from the next day's branch
	spiritNames := []string{"青龙", "明堂", "天刑", "朱雀", "金匮", "天德", "白虎", "玉堂", "天牢", "玄武", "司命", "勾陈"}
	for d := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC); d.Year() < 2025; d = d.AddDate(0, 0, 5) {
		lt := calendar.NewSolar(d.Year(), int(d.Month()), d.Day(), 23, 30, 0).GetLunar()
		_, _, day, hp, _ := PillarsAt(time.Date(d.Year(), d.Month(), d.Day(), 23, 30, 0, 0, time.UTC), true)
		if hp.Branch != lt.GetTimeZhiIndex() {
			t.Fatalf("%s: hour branch %d", d.Format("2006-01-02"), hp.Branch)
		}
		next := (day.Branch + 1) % 12
		if spiritNames[Spirit(next, hp.Branch)] != lt.GetTimeTianShen() {
			t.Fatalf("%s 23:30: %s, lunar-go %s", d.Format("2006-01-02"), spiritNames[Spirit(next, hp.Branch)], lt.GetTimeTianShen())
		}
	}
}
