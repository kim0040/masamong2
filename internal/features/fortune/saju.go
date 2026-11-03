package fortune

import "time"

// The day's reading facts, computed the way saju almanacs do (docs/plan-fortune.md §8): the same birth
// data and the same date always give the same result, and the model only writes prose around it.
// Everything is indices; names come from the catalogs (fortune.saju.*, fortune.lucky.*). Each table
// below names its rule; where lunar-go has the same table, saju_test.go checks against it.

// Elements: 0 목 (wood), 1 화 (fire), 2 토 (earth), 3 금 (metal), 4 수 (water).
const Elements = 5

// stemElement: 갑을 목, 병정 화, 무기 토, 경신 금, 임계 수. A stem is yang when its index is even.
func stemElement(s int) int { return s / 2 }

// branchElement: 자 수, 축 토, 인 목, 묘 목, 진 토, 사 화, 오 화, 미 토, 신 금, 유 금, 술 토, 해 수.
var branchElement = [12]int{4, 2, 0, 0, 2, 1, 1, 2, 3, 3, 2, 4}

// mainHidden is a branch's main hidden stem (본기, the first of 지장간): 자 계, 축 기, 인 갑, 묘 을, 진 무,
// 사 병, 오 정, 미 기, 신 경, 유 신, 술 무, 해 임.
var mainHidden = [12]int{9, 5, 0, 1, 4, 2, 3, 5, 6, 7, 4, 8}

// Ten gods (십신) of a stem seen from the day stem: 0 비견, 1 겁재, 2 식신, 3 상관, 4 편재, 5 정재,
// 6 편관, 7 정관, 8 편인, 9 정인. Rule: the element step from the day stem (same, I produce, I control,
// controls me, produces me) picks the pair; same yin/yang picks the first of it.
const TenGods = 10

// TenGod of other seen from the day stem dm.
func TenGod(dm, other int) int {
	step := mod(stemElement(other)-stemElement(dm), 5)
	g := step * 2
	if dm%2 != other%2 {
		g++
	}
	return g
}

// Twelve stages (12운성) of a stem at a branch: 0 장생, 1 목욕, 2 관대, 3 건록, 4 제왕, 5 쇠, 6 병, 7 사,
// 8 묘, 9 절, 10 태, 11 양. Rule: 장생 is at 갑 해, 을 오, 병·무 인, 정·기 유, 경 사, 신 자, 임 신, 계 묘;
// yang stems count forward from there, yin stems backward.
const Stages = 12

var stageStart = [10]int{11, 6, 2, 9, 2, 9, 5, 0, 8, 3}

// Stage of stem s at branch b.
func Stage(s, b int) int {
	if s%2 == 0 {
		return mod(b-stageStart[s], 12)
	}
	return mod(stageStart[s]-b, 12)
}

// Branch relations, in the order a reading names them.
const (
	RelSixHarmony = iota // 육합: 자축 인해 묘술 진유 사신 오미
	RelHalfTriad         // 삼합 (two of 신자진, 해묘미, 인오술, 사유축)
	RelClash             // 충: branches six apart
	RelPunish            // 형: 인사신, 축술미, 자묘, and 진·오·유·해 with themselves
	RelBreak             // 파: 자유 축진 인해 묘오 사신 미술
	RelHarm              // 해: 자미 축오 인사 묘진 신해 유술
	Relations
)

// Relate lists the relations between two branches (none, one or several: 사신 is 육합, 형 and 파).
func Relate(a, b int) []int {
	var out []int
	if mod(a+b, 12) == 1 && a != b { // 자축(0+1) 인해(2+11) 묘술 진유 사신 오미 all sum to 1 (mod 12)
		out = append(out, RelSixHarmony)
	}
	if a != b && a%4 == b%4 { // 신자진 8,0,4 · 사유축 5,9,1 · 인오술 2,6,10 · 해묘미 11,3,7
		out = append(out, RelHalfTriad)
	}
	if mod(a-b, 12) == 6 {
		out = append(out, RelClash)
	}
	if punishes(a, b) {
		out = append(out, RelPunish)
	}
	if pairIn(a, b, breakPairs) {
		out = append(out, RelBreak)
	}
	if pairIn(a, b, harmPairs) {
		out = append(out, RelHarm)
	}
	return out
}

var (
	punishPairs = [][2]int{{2, 5}, {5, 8}, {2, 8}, {1, 10}, {10, 7}, {1, 7}, {0, 3}}
	breakPairs  = [][2]int{{0, 9}, {1, 4}, {2, 11}, {3, 6}, {5, 8}, {7, 10}}
	harmPairs   = [][2]int{{0, 7}, {1, 6}, {2, 5}, {3, 4}, {8, 11}, {9, 10}}
)

func punishes(a, b int) bool {
	if a == b {
		return a == 4 || a == 6 || a == 9 || a == 11 // 자형: 진진 오오 유유 해해
	}
	return pairIn(a, b, punishPairs)
}

func pairIn(a, b int, pairs [][2]int) bool {
	for _, p := range pairs {
		if (p[0] == a && p[1] == b) || (p[0] == b && p[1] == a) {
			return true
		}
	}
	return false
}

// StemsCombine: 천간합 갑기 을경 병신 정임 무계 (five apart).
func StemsCombine(a, b int) bool { return mod(a-b, 10) == 5 }

// StemsClash: 천간충 갑경 을신 병임 정계 (six apart; 무·기 have none).
func StemsClash(a, b int) bool {
	lo, hi := min(a, b), max(a, b)
	return hi-lo == 6 && lo < 4
}

// Stars (신살) a day can carry.
const (
	StarNobleman = iota // 천을귀인: by the natal day stem
	StarScholar         // 문창귀인: by the natal day stem
	StarTravel          // 역마: by the natal year or day branch
	StarPeach           // 도화: by the natal year or day branch
	StarCanopy          // 화개: by the natal year or day branch
	Stars
)

// nobleman: 갑무경 축미, 을기 자신, 병정 해유, 임계 사묘, 신 인오 (甲戊庚牛羊, 乙己鼠猴鄉, 丙丁豬雞位, 壬癸蛇兔藏, 六辛逢馬虎).
var nobleman = [10][2]int{{1, 7}, {0, 8}, {11, 9}, {11, 9}, {1, 7}, {0, 8}, {1, 7}, {2, 6}, {5, 3}, {5, 3}}

// scholar (문창): 갑 사, 을 오, 병 신, 정 유, 무 신, 기 유, 경 해, 신 자, 임 인, 계 묘.
var scholar = [10]int{5, 6, 8, 9, 8, 9, 11, 0, 2, 3}

// By the triad of a branch (b%4: 0 신자진, 1 사유축, 2 인오술, 3 해묘미): 역마 인 해 신 사, 도화 유 오 묘 자,
// 화개 진 축 술 미.
var (
	travelOf = [4]int{2, 11, 8, 5}
	peachOf  = [4]int{9, 6, 3, 0}
	canopyOf = [4]int{4, 1, 10, 7}
)

// Day officers (건제12신): 0 건, 1 제, 2 만, 3 평, 4 정, 5 집, 6 파, 7 위, 8 성, 9 수, 10 개, 11 폐.
// Rule: 건 is the day whose branch is the month's branch, then in order.
func Officer(month, day int) int { return mod(day-month, 12) }

// Day spirits (황흑도 12신): 0 청룡, 1 명당, 2 천형, 3 주작, 4 금궤, 5 천덕, 6 백호, 7 옥당, 8 천뢰,
// 9 현무, 10 사명, 11 구진. Rule: 청룡 falls on 자 in 인·신 months, 인 in 묘·유, 진 in 진·술, 오 in 사·해,
// 신 in 자·오, 술 in 축·미, then in order. The hours of a day follow the same rule with the day branch.
func Spirit(base, b int) int { return mod(b-mod(base-2, 6)*2, 12) }

// Yellow reports a 황도 spirit (청룡 명당 금궤 천덕 옥당 사명); the other six are 흑도.
func Yellow(spirit int) bool {
	switch spirit {
	case 0, 1, 4, 5, 7, 10:
		return true
	}
	return false
}

// Natal is a person's pillars; the hour pillar only when the birth time is known.
type Natal struct {
	Year, Month, Day, Hour Pillar
	HasHour                bool
}

// Branch positions of a natal chart.
const (
	PosYear = iota
	PosMonth
	PosDay
	PosHour
)

// Hit is one relation between today's day branch and a natal branch.
type Hit struct{ Pos, Rel int }

// Scores are 0–100 per area (clamped to ScoreMin…ScoreMax).
type Scores struct{ Overall, Money, Love, Work, Health int }

// Lucky are the day's lucky points: Color and Item are ids (fortune.lucky.color.<id>, .item.<id>),
// Direction an element (fortune.lucky.direction.<e>), Number a number.
type Lucky struct{ Color, Item, Direction, Number int }

// Day is everything computed about one person's day.
type Day struct {
	Today      Pillar // today's day pillar
	TodayMonth Pillar
	StemGod    int // ten god of today's day stem
	BranchGod  int // ten god of today's day branch's main hidden stem
	Stage      int // 12운성 of the natal day stem at today's day branch
	Hits       []Hit
	StemHap    bool // today's day stem combines with the natal day stem
	StemClash  bool
	Stars      []int
	Elements   [Elements]int // natal counts over 6 or 8 characters
	Weakest    int
	Officer    int
	Spirit     int
	Yi, Ji     []string // activity ids (fortune.saju.act.<id>), at most four each
	BestHours  []int    // branches of the day's best hours, at most two
	HasHour    bool
	Scores     Scores
	Lucky      Lucky
}

// Score bounds: a low day still reads as a quiet day, not a disaster (docs/plan-fortune.md D-5).
const (
	ScoreBase = 60
	ScoreMin  = 45
	ScoreMax  = 95
)

// Analyze is the day of the person with natal chart n, for the moment at (read in its own zone: today's
// month pillar is the one in force at that moment, the day pillar the local date's).
func Analyze(n Natal, at time.Time) Day {
	_, month, day, _, _ := PillarsAt(at, false)
	return AnalyzeDay(n, month, day, JDN(at.Year(), at.Month(), at.Day()))
}

// AnalyzeDay is Analyze from today's month and day pillars; jdn (the date's Julian day number) only turns
// the lucky colour and item over from one day to the next.
func AnalyzeDay(n Natal, month, day Pillar, jdn int) Day {
	dm := n.Day.Stem
	d := Day{Today: day, TodayMonth: month, HasHour: n.HasHour,
		StemGod: TenGod(dm, day.Stem), BranchGod: TenGod(dm, mainHidden[day.Branch]), Stage: Stage(dm, day.Branch),
		StemHap: StemsCombine(dm, day.Stem), StemClash: StemsClash(dm, day.Stem),
		Officer: Officer(month.Branch, day.Branch), Spirit: Spirit(month.Branch, day.Branch)}
	natal := []struct {
		pos int
		p   Pillar
	}{{PosDay, n.Day}, {PosHour, n.Hour}, {PosMonth, n.Month}, {PosYear, n.Year}}
	for _, x := range natal {
		if x.pos == PosHour && !n.HasHour {
			continue
		}
		for _, r := range Relate(day.Branch, x.p.Branch) {
			d.Hits = append(d.Hits, Hit{Pos: x.pos, Rel: r})
		}
		d.Elements[stemElement(x.p.Stem)]++
		d.Elements[branchElement[x.p.Branch]]++
	}
	d.Stars = stars(n, day.Branch)
	d.Weakest = weakest(d.Elements, stemElement(dm))
	d.Yi, d.Ji = Activities(month, day)
	d.BestHours = bestHours(day.Branch, n.Day.Branch)
	d.Scores = score(d)
	d.Lucky = lucky(d.Weakest, jdn, dm)
	return d
}

func stars(n Natal, b int) []int {
	var out []int
	if nb := nobleman[n.Day.Stem]; b == nb[0] || b == nb[1] {
		out = append(out, StarNobleman)
	}
	if scholar[n.Day.Stem] == b {
		out = append(out, StarScholar)
	}
	for _, s := range []struct {
		star int
		of   [4]int
	}{{StarTravel, travelOf}, {StarPeach, peachOf}, {StarCanopy, canopyOf}} {
		if s.of[n.Year.Branch%4] == b || s.of[n.Day.Branch%4] == b {
			out = append(out, s.star)
		}
	}
	return out
}

// weakest is the element with the fewest natal characters; a tie goes to the one met first counting from
// the element that produces the day stem's (its 인성), then in the producing order.
func weakest(counts [Elements]int, dmElement int) int {
	best, start := -1, mod(dmElement-1, 5)
	for i := 0; i < Elements; i++ {
		e := (start + i) % Elements
		if best < 0 || counts[e] < counts[best] {
			best = e
		}
	}
	return best
}

// bestHours are up to two daytime hours (묘 05:00 … 술 21:00) under a 황도 spirit, leaving out an hour
// that clashes with the natal day branch; an hour in 육합 with the natal day branch comes first, then one
// in 삼합, then the earlier.
func bestHours(dayBranch, natalDay int) []int {
	type cand struct{ b, rank int }
	var cs []cand
	for b := 3; b <= 10; b++ {
		if !Yellow(Spirit(dayBranch, b)) {
			continue
		}
		rank := 0
		for _, r := range Relate(b, natalDay) {
			switch r {
			case RelClash:
				rank = -100
			case RelSixHarmony:
				rank += 2
			case RelHalfTriad:
				rank++
			}
		}
		if rank >= 0 {
			cs = append(cs, cand{b, rank})
		}
	}
	var out []int
	for _, want := range []int{3, 2, 1, 0} {
		for _, c := range cs {
			if c.rank == want && len(out) < 2 {
				out = append(out, c.b)
			}
		}
	}
	// keep the two in time order
	if len(out) == 2 && out[0] > out[1] {
		out[0], out[1] = out[1], out[0]
	}
	return out
}

// The score formula (units are half points, so a weight of ½ stays an integer). Each area starts at
// ScoreBase and adds:
//   - the ten god of today's day stem (×2) and of today's day branch's main hidden stem (×1), by
//     godArea: wealth → money, officer → work, output → expression and love, resource → study and
//     health, companion → social support but money shared;
//   - the 12운성 of the natal day stem at today's branch (×2) on overall and health;
//   - each relation of today's branch with the natal day branch (×2) or another natal branch (×1), by
//     relArea: 합 up, 충·형·파·해 down;
//   - 천간합 / 천간충 with the natal day stem (×2), each star (×2), and a 황도 / 흑도 day (×2).
//
// The sum is halved (toward zero), added to ScoreBase and clamped to ScoreMin…ScoreMax. Areas are
// overall, money, love, work, health.
var godArea = [TenGods][5]int{
	{1, -2, 1, 0, 2},  // 비견
	{0, -4, 0, -1, 1}, // 겁재
	{3, 2, 4, 1, 3},   // 식신
	{1, 1, 4, -3, -1}, // 상관
	{2, 7, 2, 0, 0},   // 편재
	{3, 6, 2, 1, 0},   // 정재
	{-1, 0, 1, 4, -3}, // 편관
	{3, 0, 1, 6, 0},   // 정관
	{0, -1, -2, 2, 3}, // 편인
	{3, 0, 1, 3, 4},   // 정인
}

// stageStrength: 장생 4, 목욕 0, 관대 3, 건록 5, 제왕 5, 쇠 0, 병 −3, 사 −4, 묘 −2, 절 −4, 태 0, 양 2.
var stageStrength = [Stages]int{4, 0, 3, 5, 5, 0, -3, -4, -2, -4, 0, 2}

var relArea = [Relations][5]int{
	{4, 0, 4, 1, 0},      // 육합
	{3, 1, 1, 3, 0},      // 반합
	{-5, -1, -2, -1, -4}, // 충
	{-3, 0, -1, -1, -2},  // 형
	{-2, -1, -1, -1, 0},  // 파
	{-2, 0, -3, 0, -1},   // 해
}

var (
	stemHapArea   = [5]int{3, 0, 4, 1, 0}
	stemClashArea = [5]int{-3, 0, -1, -2, -1}
	starArea      = [Stars][5]int{
		{5, 2, 2, 3, 1},  // 천을귀인
		{1, 0, 0, 4, 0},  // 문창
		{1, 1, 0, 2, -1}, // 역마
		{1, 0, 5, 0, 0},  // 도화
		{0, 0, -1, 1, 2}, // 화개
	}
	yellowArea = [5]int{2, 1, 1, 1, 1}
)

func score(d Day) Scores {
	var sum [5]int
	add := func(v [5]int, w int) {
		for i := range sum {
			sum[i] += v[i] * w
		}
	}
	add(godArea[d.StemGod], 2)
	add(godArea[d.BranchGod], 1)
	st := stageStrength[d.Stage]
	add([5]int{st, 0, 0, 0, st}, 2)
	for _, h := range d.Hits {
		w := 1
		if h.Pos == PosDay {
			w = 2
		}
		add(relArea[h.Rel], w)
	}
	if d.StemHap {
		add(stemHapArea, 2)
	}
	if d.StemClash {
		add(stemClashArea, 2)
	}
	for _, s := range d.Stars {
		add(starArea[s], 2)
	}
	if Yellow(d.Spirit) {
		add(yellowArea, 2)
	} else {
		add(yellowArea, -2)
	}
	v := func(i int) int { return min(ScoreMax, max(ScoreMin, ScoreBase+sum[i]/2)) }
	return Scores{Overall: v(0), Money: v(1), Love: v(2), Work: v(3), Health: v(4)}
}

// Lucky points from the weakest element (the 오행 colour, direction and numbers: 목 청록·동·3,8, 화 적·남·2,7,
// 토 황·중앙·5,10, 금 백·서·4,9, 수 흑·북·1,6). Each element has three colours and three items (ids
// e*3 … e*3+2); they turn over by one a day, so two days in a row never give the same one.
var luckyNumbers = [Elements][2]int{{3, 8}, {2, 7}, {5, 10}, {4, 9}, {1, 6}}

// LuckyChoices is how many colours (and items) each element has.
const LuckyChoices = 3

func lucky(e, jdn, dayStem int) Lucky {
	k := mod(jdn+dayStem, LuckyChoices)
	return Lucky{Color: e*LuckyChoices + k, Item: e*LuckyChoices + mod(k+1, LuckyChoices), Direction: e,
		Number: luckyNumbers[e][mod(jdn, 2)]}
}
