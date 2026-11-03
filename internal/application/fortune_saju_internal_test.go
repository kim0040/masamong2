package application

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/features/fortune"
)

// exampleDay is fortune's worked example (saju_test.go TestWorkedExample): natal 경오년 무인월 갑자일 without
// an hour, today 정축월 기축일. Its computed values: scores 76/70/70/65/62, lucky colour 12 (남색), item 13
// (남색 노트), north, number 1, best hours 묘 and 신; 정재 day, 육합 with the day branch, 천을귀인.
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
	for _, want := range []string{"🎨 남색", "🎁 남색 노트", "🔢 1", "🧭 북쪽", "⏰ 묘시(05~07시)"} {
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
	base := action.Card{Title: "t", Text: "오늘은 85점! 행운의 색은 파랑.", Tone: "fortune"}
	c := s.fortuneCardFor("ko", "morning", base, "head", "foot", exampleDay())
	if c.Text != base.Text || len(c.Fields) != 2 || !strings.Contains(c.Fields[0].Value, "**76**/100") ||
		!strings.Contains(c.Fields[0].Value, "🧭 정재일") || !strings.Contains(c.Fields[1].Value, "🎨 남색") {
		t.Fatalf("free text keeps its words and gets the computed points: %+v", c)
	}
}

func TestFortuneDataCarriesTheComputedFacts(t *testing.T) {
	s := privacyService(t)
	seoul, _ := time.LoadLocation("Asia/Seoul")
	at := time.Date(2026, 11, 3, 8, 0, 0, 0, seoul)
	p := fortuneProfile{BirthDate: "1990-01-01"}
	day := fortuneDay(p, at)
	data := s.fortuneData("ko", p, at, day)
	for _, want := range []string{"[계산된 사실", "점수: 총운 ", "행운: 색 ", "태어난 시각을 몰라", "타고난 오행: "} {
		if !strings.Contains(data, want) {
			t.Errorf("data misses %q:\n%s", want, data)
		}
	}
	if plain := s.fortuneData("ko", p, at, nil); strings.Contains(plain, "계산된 사실") {
		t.Fatalf("month and year readings get no day facts:\n%s", plain)
	}
	// a known birth time adds the hour pillar and drops the note
	p.BirthTime = "14:30"
	if with := s.fortuneData("ko", p, at, fortuneDay(p, at)); strings.Contains(with, "태어난 시각을 몰라") || !strings.Contains(with, "시") {
		t.Fatalf("with a birth time:\n%s", with)
	}
	// the same profile and date give the same facts
	if again := fortuneDay(fortuneProfile{BirthDate: "1990-01-01"}, at); !reflect.DeepEqual(again, day) {
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
