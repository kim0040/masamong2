package application

import (
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/features/notices"
)

const tagReading = `SCORE: 82
SUMMARY: 차분히 준비한 일이 빛을 보는 날이에요.
WORK: 집중이 잘 돼요.
MONEY: 충동구매만 조심하면 괜찮아요.
LOVE: 먼저 안부를 건네 보세요.
HEALTH: 저녁엔 일찍 쉬어요.
DO: 오전에 중요한 일부터 | 산책하기
AVOID: 감정이 올라올 때 바로 답장하기
LUCKY_COLOR: 파랑
LUCKY_ITEM: 손목시계
LUCKY_TIME: 오후 3시`

func TestFortuneCardLaysOutAReading(t *testing.T) {
	s := privacyService(t)
	base := action.Card{Title: "🔮 오늘의 운세", Text: tagReading, Footer: "old", Ephemeral: true, Tone: "fortune"}
	c := s.fortuneCard("ko", "today", base, "철수님 · 10월 15일 (수)", "일진 을사 · 염소자리 · 오늘 남은 조회 횟수: 2회")
	for _, want := range []string{"철수님 · 10월 15일 (수)", "▰▰▰▰▰▰▰▰▱▱  **82**/100", "차분히 준비한 일이", "💼 **일·학업** 집중이 잘 돼요.", "🌿 **컨디션**"} {
		if !strings.Contains(c.Text, want) {
			t.Errorf("text misses %q:\n%s", want, c.Text)
		}
	}
	if strings.Contains(c.Text, "SCORE") || strings.Contains(c.Text, "LUCKY") {
		t.Fatalf("keys must not show: %s", c.Text)
	}
	var inline int
	var do string
	for _, f := range c.Fields {
		if f.Inline {
			inline++
		}
		if strings.Contains(f.Name, "이렇게 해 보세요") {
			do = f.Value
		}
	}
	if inline != 3 || do != "• 오전에 중요한 일부터\n• 산책하기" {
		t.Fatalf("lucky in three columns, advice as bullets: %+v", c.Fields)
	}
	if c.Footer != "일진 을사 · 염소자리 · 오늘 남은 조회 횟수: 2회" || !c.Ephemeral || c.Tone != "fortune" {
		t.Fatalf("card %+v", c)
	}

	// the longer readings give each area its own field
	long := s.fortuneCard("ko", "detail", base, "head", "f")
	if strings.Contains(long.Text, "일·학업") {
		t.Fatalf("detail areas are fields: %s", long.Text)
	}
	found := false
	for _, f := range long.Fields {
		found = found || f.Name == "💼 일·학업" && f.Value == "집중이 잘 돼요."
	}
	if !found {
		t.Fatalf("area field missing: %+v", long.Fields)
	}
}

func TestFortuneCardKeepsFreeTextAsWritten(t *testing.T) {
	s := privacyService(t)
	base := action.Card{Title: "t", Text: "**오늘의 흐름**: 85점\n좋은 날이에요.", Footer: "남은 2회", Tone: "fortune"}
	if got := s.fortuneCard("ko", "today", base, "head", "new footer"); got.Text != base.Text || got.Footer != "남은 2회" || len(got.Fields) != 0 {
		t.Fatalf("free text must be shown as before: %+v", got)
	}
}

func TestFortuneCardLuckyLineWhenLong(t *testing.T) {
	s := privacyService(t)
	text := "SUMMARY: s\nLUCKY_COLOR: 파랑\nLUCKY_ITEM: 오래된 가죽 수첩과 만년필 한 자루\nLUCKY_NUMBER: 7"
	c := s.fortuneCard("ko", "today", action.Card{Text: text}, "", "")
	if len(c.Fields) != 1 || c.Fields[0].Inline || !strings.Contains(c.Fields[0].Value, "🎨 파랑 · 🎁 오래된") {
		t.Fatalf("a long value puts the lucky points on one line: %+v", c.Fields)
	}
}

func TestRankingCardUsesCatalogSignNames(t *testing.T) {
	s := privacyService(t)
	var b strings.Builder
	for i, name := range []string{"염소자리", "물병", "Leo", "양자리", "황소자리", "쌍둥이자리", "게자리", "처녀자리", "천칭자리", "전갈자리", "사수자리", "물고기자리"} {
		b.WriteString("RANK: " + string(rune('0'+(i+1)/10)) + string(rune('0'+(i+1)%10)) + " | " + name + " | 코멘트 | 파랑 | 7\n")
	}
	c := s.rankingCard("ko", action.Card{Title: "🏆", Text: b.String(), Footer: "sky"})
	for _, want := range []string{"**🌟 상위권**", "🥇 **염소자리** — 코멘트 · 🎨 파랑 · 🔢 7", "🥈 **물병자리**", "🥉 **사자자리**", "**☁️ 하위권**", "12. **물고기자리**"} {
		if !strings.Contains(c.Text, want) {
			t.Errorf("ranking misses %q:\n%s", want, c.Text)
		}
	}
	if c.Footer != "sky" {
		t.Fatalf("footer %q", c.Footer)
	}
	if got := s.rankingCard("ko", action.Card{Text: "1위 염소자리"}); got.Text != "1위 염소자리" {
		t.Fatal("a text that is not a ranking is shown as written")
	}
}

func TestNoticeCardGroupsByBandWithDaysLeft(t *testing.T) {
	s := privacyService(t)
	c := &notices.Catalog{}
	today := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	list := []rankedNotice{
		{Title: "참고 공지", URL: "https://u/3", Published: "2026-10-12", University: "가대", Score: notices.Scored{Band: "reference"}},
		{Title: "장학금 [2차] 신청", URL: "https://u/1", Published: "2026-10-14", University: "가대", Score: notices.Scored{Band: "action", Deadline: "2026-10-20"}},
		{Title: "해커톤", URL: "https://u/2", University: "가대", Score: notices.Scored{Band: "opportunity", Deadline: "2026-10-15"}},
	}
	card := s.noticeCard("ko", "📚", notices.KindSchool, list, c, today)
	want := "**⏰ 할 일 · 1건**\n**[장학금 (2차) 신청](https://u/1)**\n마감 **10/20** (D-5)\n\n" +
		"**✨ 기회 · 1건**\n[해커톤](https://u/2)\n마감 **10/15** (D-day)\n\n" +
		"**📄 참고 · 1건**\n[참고 공지](https://u/3)\n10/12"
	if card.Text != want {
		t.Fatalf("school card:\n%s\nwant:\n%s", card.Text, want)
	}
	if card.Tone != "school" || strings.Contains(card.Text, "\n  ") {
		t.Fatalf("tone %q, no indent", card.Tone)
	}
}

func TestNoticeCardGroupsTransferByUniversity(t *testing.T) {
	s := privacyService(t)
	today := time.Date(2026, 10, 15, 8, 0, 0, 0, time.UTC)
	list := []rankedNotice{
		{Title: "모집요강", URL: "https://a/1", Published: "2026-10-14", University: "가대"},
		{Title: "합격자 발표", URL: "https://b/1", Published: "2025-12-01", University: "나대"},
		{Title: "면접 안내", URL: "https://a/2", University: "가대"},
	}
	card := s.noticeCard("ko", "🎓", notices.KindTransfer, list, &notices.Catalog{}, today)
	want := "**🎓 가대 · 2건**\n[모집요강](https://a/1)\n10/14\n[면접 안내](https://a/2)\n\n**🎓 나대 · 1건**\n[합격자 발표](https://b/1)\n2025-12-01"
	if card.Text != want || card.Tone != "transfer" {
		t.Fatalf("transfer card:\n%s", card.Text)
	}
}

func TestDayLabelsFollowTheLanguage(t *testing.T) {
	s := privacyService(t)
	d := time.Date(2026, 10, 15, 0, 0, 0, 0, time.UTC)
	for loc, want := range map[string]string{"ko": "10월 15일 (목)", "en": "Thu, 10/15", "es": "jue 15/10"} {
		if got := s.dayLabel(loc, d); got != want {
			t.Errorf("%s: %q, want %q", loc, got, want)
		}
	}
	if got := s.shortDate("es", d); got != "15/10" {
		t.Errorf("es short %q", got)
	}
}
