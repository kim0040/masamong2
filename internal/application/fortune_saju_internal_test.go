package application

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/6tail/lunar-go/calendar"

	"masamong/internal/action"
	"masamong/internal/features/fortune"
)

// exampleDay is fortune's worked example (saju_test.go TestWorkedExample): natal 경오년 무인월 갑자일 without
// an hour, today 정축월 기축일. Its computed values: scores 76/70/70/65/62, lucky colour 14 (차콜), item 12
// (텀블러), north, number 1, best hours 묘 and 신; 정재 day, 육합 with the day branch, 천을귀인.
func exampleDay() *fortune.Day {
	d := fortune.AnalyzeDay(fortune.Natal{Year: fortune.Pillar{Stem: 6, Branch: 6}, Month: fortune.Pillar{Stem: 4, Branch: 2}, Day: fortune.Pillar{Stem: 0, Branch: 0}},
		fortune.Pillar{Stem: 3, Branch: 1}, fortune.Pillar{Stem: 5, Branch: 1}, 2460000)
	return &d
}

func TestFortuneCardShowsComputedValues(t *testing.T) {
	s := privacyService(t)
	base := action.Card{Title: "🔮", Text: tagReading, Tone: "fortune"} // the model wrote 82, 파랑, 손목시계, 오후 3시
	c := s.fortuneCardFor("ko", "today", base, "head", "foot", exampleDay())
	for _, want := range []string{"**76**/100", "💼 일·학업 65 · 💰 재물 70 · 💞 연애·관계 70 · 🌿 컨디션 62", "🧭 정재일 · 일지와 육합 · 천을귀인", "차분히 준비한 일이"} {
		if !strings.Contains(c.Text, want) {
			t.Errorf("text misses %q:\n%s", want, c.Text)
		}
	}
	fields := ""
	for _, f := range c.Fields {
		fields += f.Name + "=" + f.Value + "\n"
	}
	for _, want := range []string{"🎨 차콜", "🎁 텀블러", "🔢 1", "🧭 북쪽", "⏰ 묘시(05~07시)"} {
		if !strings.Contains(fields, want) {
			t.Errorf("lucky misses %q:\n%s", want, fields)
		}
	}
	for _, gone := range []string{"82", "파랑", "손목시계", "오후 3시"} {
		if strings.Contains(c.Text+fields, gone) {
			t.Errorf("the model's %q is still on the card:\n%s\n%s", gone, c.Text, fields)
		}
	}
	// a reading that is not a day's (month, year, a sign) keeps what the model wrote
	if m := s.fortuneCardFor("ko", "month", base, "head", "foot", exampleDay()); !strings.Contains(m.Text, "**82**/100") || strings.Contains(m.Text, "🧭") {
		t.Fatalf("month reading changed: %s", m.Text)
	}
}

func TestFortuneCardComputedWhenTheModelIgnoredTheFormat(t *testing.T) {
	s := privacyService(t)
	base := action.Card{Title: "t", Text: "오늘은 85점! 행운의 색은 파랑.", Tone: "fortune", Footer: "model footer"}
	c := s.fortuneCardFor("ko", "morning", base, "head", "basis · foot", exampleDay())
	if c.Text != base.Text || len(c.Fields) != 2 || !strings.Contains(c.Fields[0].Value, "**76**/100") ||
		!strings.Contains(c.Fields[0].Value, "🧭 정재일") || !strings.Contains(c.Fields[1].Value, "🎨 차콜") {
		t.Fatalf("free text keeps its words and gets the computed points: %+v", c)
	}
	if c.Footer != "basis · foot" {
		t.Fatalf("free text gets the basis footer too: %q", c.Footer)
	}
	// keys without a SUMMARY (not a reading): the model's SCORE and LUCKY_* lines go, the rest stays; a
	// key in Korean ("총점") cannot be told from prose and stays
	base.Text = "SCORE: 90\nWORK: 집중\n- LUCKY_COLOR: 파랑\n총점: 85"
	c = s.fortuneCardFor("ko", "today", base, "head", "foot", exampleDay())
	if strings.Contains(c.Text, "90") || strings.Contains(c.Text, "파랑") || !strings.Contains(c.Text, "WORK: 집중") || !strings.Contains(c.Text, "총점: 85") ||
		c.Footer != "foot" || !strings.Contains(c.Fields[0].Value, "**76**/100") {
		t.Fatalf("keys without a summary: %+v", c)
	}
	// a month reading in free text is shown as written
	if m := s.fortuneCardFor("ko", "month", base, "head", "foot", exampleDay()); m.Text != base.Text || m.Footer != base.Footer {
		t.Fatalf("month free text changed: %+v", m)
	}
}

func TestFortuneDataCarriesTheComputedFacts(t *testing.T) {
	s := privacyService(t)
	seoul, _ := time.LoadLocation("Asia/Seoul")
	at := time.Date(2026, 11, 3, 8, 0, 0, 0, seoul)
	p := fortuneProfile{BirthDate: "1990-01-01"}
	chart := s.fortuneChart(p, at, true)
	day := chart.day
	data := s.fortuneData("ko", p, at, chart)
	for _, want := range []string{"[계산된 사실", "점수: 총운 ", "행운: 색 ", "태어난 시각을 몰라", "타고난 오행: "} {
		if !strings.Contains(data, want) {
			t.Errorf("data misses %q:\n%s", want, data)
		}
	}
	if plain := s.fortuneData("ko", p, at, s.fortuneChart(p, at, false)); strings.Contains(plain, "계산된 사실") {
		t.Fatalf("month and year readings get no day facts:\n%s", plain)
	}
	// a known birth time adds the hour pillar and drops the note
	p.BirthTime = "14:30"
	if with := s.fortuneData("ko", p, at, s.fortuneChart(p, at, true)); strings.Contains(with, "태어난 시각을 몰라") || !strings.Contains(with, "시") {
		t.Fatalf("with a birth time:\n%s", with)
	}
	// the same profile and date give the same facts
	if again := s.fortuneChart(fortuneProfile{BirthDate: "1990-01-01"}, at, true).day; !reflect.DeepEqual(again, day) {
		t.Fatal("not deterministic")
	}
	// the facts block stays small (about a hundred tokens; a rough rune bound for Korean)
	facts := s.factsText("ko", *day)
	if n := len([]rune(facts)); n > 420 {
		t.Fatalf("facts block is %d runes:\n%s", n, facts)
	}
}

func TestHourLabel(t *testing.T) {
	s := privacyService(t)
	for b, want := range map[int]string{0: "자시(23~01시)", 3: "묘시(05~07시)", 11: "해시(21~23시)"} {
		if got := s.hourLabel("ko", b); got != want {
			t.Fatalf("%d: %q", b, got)
		}
	}
}

func TestSajuBasisNamesAHarmonyFirst(t *testing.T) {
	s := privacyService(t)
	// a day with 형 and 해 on the day branch and a 천간합: the basis names the 합
	d := fortune.Day{StemGod: 5, StemHap: true, Hits: []fortune.Hit{{Pos: fortune.PosDay, Rel: fortune.RelPunish}, {Pos: fortune.PosDay, Rel: fortune.RelHarm}}}
	if got := s.sajuBasis("ko", d); got != "정재일 · 일간과 천간합" {
		t.Fatalf("%q", got)
	}
	// only clashes: the first one is named
	d.StemHap = false
	if got := s.sajuBasis("ko", d); got != "정재일 · 일지와 형" {
		t.Fatalf("%q", got)
	}
}

// withZone sets the instance's configured zone (i18n.timezone).
func withZone(t *testing.T, s *Service, zone string) {
	t.Helper()
	doc := s.Doc()
	doc.I18n.Timezone = zone
	s.SetDocForTest(doc)
}

func TestNatalIgnoresTheDisplayZone(t *testing.T) {
	// born 1990-02-04 09:30 in Seoul, 44 minutes before 입춘 (10:14 KST): 기사년 정축월 (lunar-go, at 08:30
	// Beijing time). Moving one's display zone to New York must not move the birth past 입춘.
	s := privacyService(t)
	withZone(t, s, "")
	if got := s.birthZone().String(); got != "Asia/Seoul" {
		t.Fatalf("no configured zone: births are read in %s", got)
	}
	ec := calendar.NewSolar(1990, 2, 4, 8, 30, 0).GetLunar().GetEightChar()
	if ec.GetYear() != "己巳" || ec.GetMonth() != "丁丑" {
		t.Fatalf("lunar-go %s %s", ec.GetYear(), ec.GetMonth())
	}
	want := fortune.Natal{Year: fortune.Pillar{Stem: 5, Branch: 5}, Month: fortune.Pillar{Stem: 3, Branch: 1}}
	p := fortuneProfile{BirthDate: "1990-02-04", BirthTime: "09:30"}
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("no tzdata")
	}
	seoul, _ := time.LoadLocation("Asia/Seoul")
	for _, at := range []time.Time{time.Date(2026, 11, 3, 8, 0, 0, 0, seoul), time.Date(2026, 11, 3, 8, 0, 0, 0, ny)} {
		c := s.fortuneChart(p, at, true)
		if c.natal.Year != want.Year || c.natal.Month != want.Month || !c.natal.HasHour {
			t.Fatalf("display zone %s: natal %+v", at.Location(), c.natal)
		}
	}
	// a configured instance zone is the birth zone
	withZone(t, s, "America/New_York")
	if got := s.birthZone().String(); got != "America/New_York" {
		t.Fatalf("configured zone: %s", got)
	}
}

func TestInvalidBirthDateGetsNoFacts(t *testing.T) {
	s := privacyService(t)
	at := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	for _, date := range []string{"0000-05-15", "1990-02-30", "1899-12-31", ""} {
		p := fortuneProfile{BirthDate: date}
		c := s.fortuneChart(p, at, true)
		if c.valid || c.day != nil {
			t.Fatalf("%q: %+v", date, c)
		}
		if data := s.fortuneData("ko", p, at, c); strings.Contains(data, "계산된 사실") {
			t.Fatalf("%q: computed facts:\n%s", date, data)
		}
		// the card is the model's, as before the rule engine
		base := action.Card{Title: "t", Text: tagReading, Tone: "fortune"}
		if card := s.fortuneCardFor("ko", "today", base, "head", "foot", c.day); !strings.Contains(card.Text, "**82**/100") {
			t.Fatalf("%q: %s", date, card.Text)
		}
	}
}

func TestTermDateWithoutBirthTime(t *testing.T) {
	// 2026-10-08 is 한로 in Seoul: without a birth time the month pillar is not certain; on 입춘
	// (2026-02-04) the year is not either; a known birth time settles both
	s := privacyService(t)
	withZone(t, s, "Asia/Seoul")
	at := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		date, time string
		want       int
		note       string
	}{
		{"2026-10-08", "", natalMonthUncertain, "월주"},
		{"2026-02-04", "", natalYearUncertain, "연주와 월주"},
		{"2026-02-04", "07:00", natalCertain, ""},
		{"2026-10-09", "", natalCertain, ""},
	} {
		p := fortuneProfile{BirthDate: c.date, BirthTime: c.time}
		ch := s.fortuneChart(p, at, false)
		if ch.uncertain != c.want {
			t.Fatalf("%s %s: %d", c.date, c.time, ch.uncertain)
		}
		data := s.fortuneData("ko", p, at, ch)
		if c.note != "" && !strings.Contains(data, c.note) {
			t.Fatalf("%s: data misses the note:\n%s", c.date, data)
		}
		if c.note == "" && strings.Contains(data, "절기") {
			t.Fatalf("%s: an unneeded note:\n%s", c.date, data)
		}
	}
}

func TestBriefingGreetingsArePolite(t *testing.T) {
	// the Korean greeting examples stay 해요체, like the default persona (internal/lang voice_test.go)
	s := privacyService(t)
	for _, slot := range []string{"morning", "day", "evening", "night"} {
		g := s.I18n.T("ko", "fortune.briefing.greeting."+slot, "name", "철수")
		if !strings.Contains(g, "철수님") || !(strings.Contains(g, "요!") || strings.Contains(g, "요?")) {
			t.Errorf("%s: %q", slot, g)
		}
	}
}
