package fortune

import (
	"container/list"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/6tail/lunar-go/LunarUtil"
	"github.com/6tail/lunar-go/calendar"
)

// Expected values come from lunar-go's tables (the oracle) or, where it has none, from the rule written
// next to the table in saju.go.

func ganIndex(t *testing.T, g string) int {
	t.Helper()
	i := slices.Index(LunarUtil.GAN, g)
	if i < 1 {
		t.Fatalf("not a stem: %q", g)
	}
	return i - 1
}

func zhiIndex(t *testing.T, z string) int {
	t.Helper()
	i := slices.Index(LunarUtil.ZHI, z)
	if i < 1 {
		t.Fatalf("not a branch: %q", z)
	}
	return i - 1
}

func TestTenGodsMatchLunarGo(t *testing.T) {
	names := []string{"比肩", "劫财", "食神", "伤官", "偏财", "正财", "七杀", "正官", "偏印", "正印"}
	for dm := 0; dm < 10; dm++ {
		for o := 0; o < 10; o++ {
			want := LunarUtil.SHI_SHEN[LunarUtil.GAN[dm+1]+LunarUtil.GAN[o+1]]
			if got := names[TenGod(dm, o)]; got != want {
				t.Fatalf("%d/%d: %s, lunar-go %s", dm, o, got, want)
			}
		}
	}
	for b := 0; b < 12; b++ {
		if want := ganIndex(t, LunarUtil.ZHI_HIDE_GAN[LunarUtil.ZHI[b+1]][0]); mainHidden[b] != want {
			t.Fatalf("main hidden stem of %d: %d, lunar-go %d", b, mainHidden[b], want)
		}
	}
}

func TestElementsMatchLunarGo(t *testing.T) {
	el := map[string]int{"木": 0, "火": 1, "土": 2, "金": 3, "水": 4}
	for s := 0; s < 10; s++ {
		if stemElement(s) != el[LunarUtil.WU_XING_GAN[LunarUtil.GAN[s+1]]] {
			t.Fatalf("stem %d", s)
		}
	}
	for b := 0; b < 12; b++ {
		if branchElement[b] != el[LunarUtil.WU_XING_ZHI[LunarUtil.ZHI[b+1]]] {
			t.Fatalf("branch %d", b)
		}
	}
}

func TestStagesMatchLunarGo(t *testing.T) {
	// lunar-go's 地势 is the day stem's stage at each pillar's branch; sixty days of twelve hours cover
	// every stem at every branch
	seen := map[[2]int]bool{}
	for d := 0; d < 60; d++ {
		for h := 0; h < 24; h += 2 {
			day := time.Date(2024, 1, 1, h, 30, 0, 0, time.UTC).AddDate(0, 0, d)
			l := calendar.NewSolar(day.Year(), int(day.Month()), day.Day(), h, 30, 0).GetLunar()
			ec := l.GetEightChar()
			dm := ganIndex(t, ec.GetDayGan())
			for _, c := range []struct{ zhi, stage string }{{ec.GetTimeZhi(), ec.GetTimeDiShi()}, {ec.GetDayZhi(), ec.GetDayDiShi()}} {
				b := zhiIndex(t, c.zhi)
				if got := calendar.CHANG_SHENG[Stage(dm, b)]; got != c.stage {
					t.Fatalf("stem %d at %d: %s, lunar-go %s", dm, b, got, c.stage)
				}
				seen[[2]int{dm, b}] = true
			}
		}
	}
	if len(seen) != 120 {
		t.Fatalf("covered %d of 120 pairs", len(seen))
	}
}

func TestRelationsMatchLunarGo(t *testing.T) {
	for a := 0; a < 12; a++ {
		for b := 0; b < 12; b++ {
			rels := Relate(a, b)
			if want := LunarUtil.HE_ZHI_6[a] == LunarUtil.ZHI[b+1]; slices.Contains(rels, RelSixHarmony) != want {
				t.Fatalf("육합 %d %d", a, b)
			}
			if want := LunarUtil.CHONG[a] == LunarUtil.ZHI[b+1]; slices.Contains(rels, RelClash) != want {
				t.Fatalf("충 %d %d", a, b)
			}
		}
	}
	for a := 0; a < 10; a++ {
		for b := 0; b < 10; b++ {
			if want := LunarUtil.HE_GAN_5[a] == LunarUtil.GAN[b+1]; StemsCombine(a, b) != want {
				t.Fatalf("천간합 %d %d", a, b)
			}
			if want := LunarUtil.CHONG_GAN_4[a] == LunarUtil.GAN[b+1]; StemsClash(a, b) != want {
				t.Fatalf("천간충 %d %d", a, b)
			}
		}
	}
}

func TestRelationsWithoutAnOracle(t *testing.T) {
	// the rules in saju.go's Rel* comments, case by case (자0 축1 인2 묘3 진4 사5 오6 미7 신8 유9 술10 해11)
	cases := []struct {
		a, b int
		want []int
	}{
		{8, 0, []int{RelHalfTriad}},                       // 신자 (신자진)
		{11, 3, []int{RelHalfTriad}},                      // 해묘 (해묘미, with the cardinal 묘)
		{11, 7, nil},                                      // 해미: no cardinal, no 반합
		{2, 5, []int{RelPunish, RelHarm}},                 // 인사: 형, 해
		{5, 8, []int{RelSixHarmony, RelPunish, RelBreak}}, // 사신: 육합, 형, 파
		{2, 8, []int{RelClash, RelPunish}},                // 인신: 충, 형
		{1, 7, []int{RelClash, RelPunish}},                // 축미: 충, 형
		{0, 3, []int{RelPunish}},                          // 자묘 상형
		{6, 6, []int{RelPunish}},                          // 오오 자형
		{0, 0, nil},                                       // 자자: nothing
		{2, 11, []int{RelSixHarmony, RelBreak}},           // 인해: 육합, 파
		{0, 9, []int{RelBreak}},                           // 자유 파
		{9, 10, []int{RelHarm}},                           // 유술 해
		{2, 6, []int{RelHalfTriad}},                       // 인오 (인오술)
		{3, 4, []int{RelHarm}},                            // 묘진 해
	}
	for _, c := range cases {
		if got := Relate(c.a, c.b); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%d %d: %v want %v", c.a, c.b, got, c.want)
		}
		if got := Relate(c.b, c.a); !reflect.DeepEqual(got, c.want) {
			t.Fatalf("%d %d not symmetric: %v", c.b, c.a, got)
		}
	}
}

func TestOfficerAndSpiritMatchLunarGo(t *testing.T) {
	names := []string{"青龙", "明堂", "天刑", "朱雀", "金匮", "天德", "白虎", "玉堂", "天牢", "玄武", "司命", "勾陈"}
	for d := time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC); d.Year() < 2031; d = d.AddDate(0, 0, 1) {
		l := calendar.NewSolar(d.Year(), int(d.Month()), d.Day(), 12, 0, 0).GetLunar()
		m, b := l.GetMonthZhiIndex(), l.GetDayZhiIndex()
		if got := LunarUtil.ZHI_XING[Officer(m, b)+1]; got != l.GetZhiXing() {
			t.Fatalf("%s officer %s, lunar-go %s", d.Format("2006-01-02"), got, l.GetZhiXing())
		}
		sp := Spirit(m, b)
		if names[sp] != l.GetDayTianShen() || Yellow(sp) != (l.GetDayTianShenType() == "黄道") {
			t.Fatalf("%s spirit %s, lunar-go %s %s", d.Format("2006-01-02"), names[sp], l.GetDayTianShen(), l.GetDayTianShenType())
		}
		if d.Day() == 1 { // the hours of the day follow the same rule from the day branch
			for h := 0; h < 23; h += 2 {
				lt := calendar.NewSolar(d.Year(), int(d.Month()), d.Day(), h, 30, 0).GetLunar()
				hb := lt.GetTimeZhiIndex()
				if names[Spirit(b, hb)] != lt.GetTimeTianShen() {
					t.Fatalf("%s %02d:30 hour spirit %s, lunar-go %s", d.Format("2006-01-02"), h, names[Spirit(b, hb)], lt.GetTimeTianShen())
				}
			}
		}
	}
}

func TestEveryAlmanacItemIsSortedOut(t *testing.T) {
	// every item lunar-go can list for any month and day is either in a group or deliberately dropped
	seen := map[string]bool{}
	for _, m := range LunarUtil.JIA_ZI {
		for _, d := range LunarUtil.JIA_ZI {
			for _, l := range []func(string, string) []string{yiNames, jiNames} {
				for _, n := range l(m, d) {
					seen[n] = true
				}
			}
		}
	}
	if len(seen) < 100 {
		t.Fatalf("only %d items", len(seen))
	}
	for n := range seen {
		if _, ok := activityOf[n]; !ok && !activityDropped[n] {
			t.Errorf("lunar-go item %q is neither grouped nor dropped", n)
		}
	}
	for n := range activityDropped {
		if _, ok := activityOf[n]; ok {
			t.Errorf("%q is both grouped and dropped", n)
		}
	}
}

func yiNames(m, d string) []string { return names(LunarUtil.GetDayYi(m, d)) }
func jiNames(m, d string) []string { return names(LunarUtil.GetDayJi(m, d)) }

func names(l *list.List) []string {
	var out []string
	for e := l.Front(); e != nil; e = e.Next() {
		out = append(out, e.Value.(string))
	}
	return out
}

func TestActivitiesAreBoundedAndOneSided(t *testing.T) {
	for m := 0; m < 60; m++ {
		for d := 0; d < 60; d++ {
			mp, dp := Pillar{m % 10, m % 12}, Pillar{d % 10, d % 12}
			yi, ji := Activities(mp, dp)
			if len(yi) > ActivityLimit || len(ji) > ActivityLimit {
				t.Fatalf("%v %v: %v %v", mp, dp, yi, ji)
			}
			for _, g := range yi {
				if slices.Contains(ji, g) || !slices.Contains(ActivityIDs, g) {
					t.Fatalf("%v %v: %q", mp, dp, g)
				}
			}
			// each listed group comes from one of lunar-go's items of that side
			for _, g := range yi {
				if !slices.ContainsFunc(yiNames(hanzi(mp), hanzi(dp)), func(n string) bool { return activityOf[n] == g }) {
					t.Fatalf("%v %v: %q not from lunar-go's 宜", mp, dp, g)
				}
			}
			for _, g := range ji {
				if !slices.ContainsFunc(jiNames(hanzi(mp), hanzi(dp)), func(n string) bool { return activityOf[n] == g }) {
					t.Fatalf("%v %v: %q not from lunar-go's 忌", mp, dp, g)
				}
			}
		}
	}
}

func TestStars(t *testing.T) {
	// rules from saju.go: 천을귀인 갑 축미 / 신 인오, 문창 경 해, 역마 인오술 신, 도화 신자진 유, 화개 사유축 축
	n := Natal{Year: Pillar{6, 6}, Day: Pillar{0, 0}} // 경오년 갑자일
	if got := stars(n, 1); !reflect.DeepEqual(got, []int{StarNobleman}) {
		t.Fatalf("갑 day on 축: %v", got)
	}
	if got := stars(n, 8); !reflect.DeepEqual(got, []int{StarTravel}) { // year 오 (인오술) → 신
		t.Fatalf("신 day: %v", got)
	}
	if got := stars(n, 9); !reflect.DeepEqual(got, []int{StarPeach}) { // day 자 (신자진) → 유
		t.Fatalf("유 day: %v", got)
	}
	n = Natal{Year: Pillar{1, 9}, Day: Pillar{7, 5}}                      // 을유년 신사일
	if got := stars(n, 2); !reflect.DeepEqual(got, []int{StarNobleman}) { // 신 → 인오
		t.Fatalf("신 stem on 인: %v", got)
	}
	if got := stars(n, 1); !reflect.DeepEqual(got, []int{StarCanopy}) { // 사유축 → 축
		t.Fatalf("축: %v", got)
	}
	if got := stars(Natal{Day: Pillar{6, 0}}, 11); !slices.Contains(got, StarScholar) { // 경 → 해
		t.Fatalf("경 on 해: %v", got)
	}
}

func TestWeakestTieBreak(t *testing.T) {
	// a tie goes to the element that produces the day stem's first (갑 목 → 수), then 목 화 토 금
	if w := weakest([5]int{2, 1, 1, 1, 1}, 0); w != 4 {
		t.Fatalf("%d", w)
	}
	if w := weakest([5]int{2, 0, 2, 0, 2}, 0); w != 1 { // 수 has 2: 목 화 → 화
		t.Fatalf("%d", w)
	}
	if w := weakest([5]int{1, 2, 2, 2, 1}, 2); w != 4 { // 토: from 화(2), 토, 금, 수(1) …
		t.Fatalf("%d", w)
	}
}

// TestWorkedExample follows the formula in saju.go by hand (comments show each step).
func TestWorkedExample(t *testing.T) {
	// natal 경오년 무인월 갑자일, no hour; today 정축월 기축일
	n := Natal{Year: Pillar{6, 6}, Month: Pillar{4, 2}, Day: Pillar{0, 0}}
	d := AnalyzeDay(n, Pillar{3, 1}, Pillar{5, 1}, 2460000)
	// 갑 sees 기 (토, yin): 정재; 축's main stem 기: 정재; 갑 at 축: 관대 (장생 해 → 자 목욕 → 축 관대)
	if d.StemGod != 5 || d.BranchGod != 5 || d.Stage != 2 {
		t.Fatalf("gods/stage %d %d %d", d.StemGod, d.BranchGod, d.Stage)
	}
	// 축 with 자 (day): 육합; with 인 (month): nothing; with 오 (year): 해. 갑기 합. 천을귀인 (갑 → 축미).
	if !reflect.DeepEqual(d.Hits, []Hit{{PosDay, RelSixHarmony}, {PosYear, RelHarm}}) || !d.StemHap || d.StemClash {
		t.Fatalf("hits %v hap %v clash %v", d.Hits, d.StemHap, d.StemClash)
	}
	if !reflect.DeepEqual(d.Stars, []int{StarNobleman}) {
		t.Fatalf("stars %v", d.Stars)
	}
	// 축월 축일: 건; 축월's 청룡 is on 술, so 축 is 주작 (흑도)
	if d.Officer != 0 || d.Spirit != 3 || Yellow(d.Spirit) {
		t.Fatalf("officer %d spirit %d", d.Officer, d.Spirit)
	}
	// 갑자 무인 경오: 목2 화1 토1 금1 수1 → 수 (the tie goes to 갑's 인성 first)
	if d.Elements != [5]int{2, 1, 1, 1, 1} || d.Weakest != 4 {
		t.Fatalf("elements %v weakest %d", d.Elements, d.Weakest)
	}
	// half points, areas overall/money/love/work/health:
	//   정재 ×2 {6,12,4,2,0} + 정재 ×1 {3,6,2,1,0}          = {9,18,6,3,0}
	//   관대 3 ×2 on overall and health                     = {15,18,6,3,6}
	//   육합 with the day branch ×2 {8,0,8,2,0}             = {23,18,14,5,6}
	//   해 with the year branch ×1 {-2,0,-3,0,-1}           = {21,18,11,5,5}
	//   천간합 ×2 {6,0,8,2,0}                               = {27,18,19,7,5}
	//   천을귀인 ×2 {10,4,4,6,2}                            = {37,22,23,13,7}
	//   흑도 ×−2 {-4,-2,-2,-2,-2}                           = {33,20,21,11,5}
	//   halved {16,10,10,5,2} + 60
	if d.Scores != (Scores{Overall: 76, Money: 70, Love: 70, Work: 65, Health: 62}) {
		t.Fatalf("scores %+v", d.Scores)
	}
	// lucky element: natal 목2 화1 토1 금1 수1 plus today's 기 (토) and 축 (토) = 목2 화1 토3 금1 수1; 화 금 수
	// tie and the tie-break from 갑's 인성 gives 수. k = (2460000 + 5, today's stem 기) mod 3 = 2 → colour
	// 4*3+2 = 14, item 4*3+0 = 12, north, number 1 (2460000 is even)
	if d.LuckyElem != 4 || d.Lucky != (Lucky{Color: 14, Item: 12, Direction: 4, Number: 1}) {
		t.Fatalf("lucky %+v", d.Lucky)
	}
	// 축 day hours 묘…술 under 황도: 묘 천덕, 사 옥당, 신 사명, 술 청룡; 신 is in 삼합 with 자 (rank 1),
	// the others 0 → 신 and 묘, shown in time order
	if !reflect.DeepEqual(d.BestHours, []int{3, 8}) {
		t.Fatalf("best hours %v", d.BestHours)
	}
}

func TestHourIsOptional(t *testing.T) {
	n := Natal{Year: Pillar{6, 6}, Month: Pillar{4, 2}, Day: Pillar{0, 0}, Hour: Pillar{0, 7}}
	without := AnalyzeDay(n, Pillar{3, 1}, Pillar{5, 1}, 1)
	n.HasHour = true
	with := AnalyzeDay(n, Pillar{3, 1}, Pillar{5, 1}, 1)
	sum := func(e [5]int) (s int) {
		for _, v := range e {
			s += v
		}
		return
	}
	if sum(without.Elements) != 6 || sum(with.Elements) != 8 || without.HasHour || !with.HasHour {
		t.Fatalf("elements %v %v", without.Elements, with.Elements)
	}
	// the hour branch 미 is in 충 with today's 축 and adds that relation
	if !slices.Contains(with.Hits, Hit{PosHour, RelClash}) || slices.ContainsFunc(without.Hits, func(h Hit) bool { return h.Pos == PosHour }) {
		t.Fatalf("hits %v / %v", with.Hits, without.Hits)
	}
}

func TestAnalyzeIsDeterministicAndBounded(t *testing.T) {
	n := Natal{Year: Pillar{6, 6}, Month: Pillar{4, 2}, Day: Pillar{0, 0}, Hour: Pillar{2, 2}, HasHour: true}
	at := time.Date(2026, 11, 3, 8, 0, 0, 0, kst)
	if a, b := Analyze(n, at), Analyze(n, at); !reflect.DeepEqual(a, b) {
		t.Fatalf("same input, different output:\n%+v\n%+v", a, b)
	}
	prev := Lucky{Color: -1, Item: -1, Number: -1}
	for i := 0; i < 400; i++ {
		day := at.AddDate(0, 0, i)
		d := Analyze(n, day)
		for _, s := range []int{d.Scores.Overall, d.Scores.Money, d.Scores.Love, d.Scores.Work, d.Scores.Health} {
			if s < ScoreMin || s > ScoreMax {
				t.Fatalf("%s: score %d", day.Format("2006-01-02"), s)
			}
		}
		if d.Lucky.Color == prev.Color || d.Lucky.Item == prev.Item || d.Lucky.Number == prev.Number {
			t.Fatalf("%s: lucky %+v repeats %+v", day.Format("2006-01-02"), d.Lucky, prev)
		}
		if d.Lucky.Color/LuckyChoices != d.LuckyElem || d.Lucky.Item/LuckyChoices != d.LuckyElem || d.Lucky.Direction != d.LuckyElem {
			t.Fatalf("lucky %+v not of element %d", d.Lucky, d.LuckyElem)
		}
		prev = d.Lucky
	}
}
