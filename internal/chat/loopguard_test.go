package chat

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestCutLoopKeepsTheFirstRound(t *testing.T) {
	round := "그래서 결론은 이렇습니다. 오늘은 비가 오니까 우산을 챙기고, 내일은 맑으니까 빨래를 하면 됩니다. "
	text := "날씨를 정리하면요. " + strings.Repeat(round, 8)
	got, looped := cutLoop(text)
	if !looped || got != strings.TrimSpace("날씨를 정리하면요. "+round) {
		t.Fatalf("loop: %v %q", looped, got)
	}
	if got, _ := cutLoop(strings.Repeat(round, 8)); got != strings.TrimSpace(round) {
		t.Fatalf("a loop from the first rune keeps one round: %q", got)
	}
	link := "참고: https://example.com/articles/2026/10/31/weather-report-for-the-bot\n"
	if got, _ := cutLoop(strings.Repeat(link, 8)); got != strings.TrimSpace(link) {
		t.Fatalf("a round ending in a line break is cut there, not inside the next link: %q", got)
	}
	var rows, links strings.Builder
	for i := range 12 {
		fmt.Fprintf(&rows, "\t{name: \"case%d\", input: defaultInputForTesting, want: defaultExpectedResultForTesting},\n", i)
		fmt.Fprintf(&links, "%d번 항목 설명이 여기에 조금 들어갑니다 %d: https://example.com/articles/2026/10/31/weather-report-for-the-bot\n", i, i*3)
	}
	for _, ok := range []string{rows.String(), links.String(),
		strings.Repeat("ㅋ", 600), // a short-period repeat is laughter, not a loop
		strings.Repeat("가나다라마바사아자차카타파하 ", 40)[:200],       // too short to check
		"처음 부분. " + strings.Repeat(round, 2) + varied(30), // repeats, but not five times
	} {
		if _, looped := cutLoop(ok); looped {
			t.Fatalf("not a loop: %q", ok[:min(40, len(ok))])
		}
	}
}

func TestAnswerCarriesTheTimeAndLineAges(t *testing.T) {
	v := koVoice()
	now := time.Date(2026, 10, 31, 15, 4, 0, 0, time.FixedZone("KST", 9*3600))
	in := AnswerInput{System: "s", Message: "어제 뭐 했지?", Speaker: "영희", Now: now, History: []Turn{
		{Role: "user", Speaker: "철수", Content: "어제 영화 봤어", At: now.Add(-26 * time.Hour)},
		{Role: "user", Speaker: "민수", Content: "나는 아까 밥 먹음", At: now.Add(-25 * time.Minute)},
		{Role: "assistant", Content: "맛있었겠다", At: now.Add(-24 * time.Minute)},
		{Role: "user", Speaker: "영희", Content: "방금 왔어", At: now.Add(-time.Minute)},
	}}
	msgs := BuildAnswerMessages(v, in)
	if got := msgs[1].Content; got != "철수 (1일 전): 어제 영화 봤어" {
		t.Fatalf("a day-old line: %q", got)
	}
	if got := msgs[2].Content; got != "민수 (25분 전): 나는 아까 밥 먹음" {
		t.Fatalf("a 25-minute-old line: %q", got)
	}
	if msgs[3].Content != "맛있었겠다" || msgs[4].Content != "영희: 방금 왔어" {
		t.Fatalf("the bot's own lines and recent lines carry no age: %q %q", msgs[3].Content, msgs[4].Content)
	}
	last := msgs[len(msgs)-1].Content
	if !strings.HasPrefix(last, "지금 시각: 2026-10-31 15:04 ") || !strings.Contains(strings.SplitN(last, "\n", 2)[0], "KST") {
		t.Fatalf("time line: %q", strings.SplitN(last, "\n", 2)[0])
	}
	in.Now = time.Time{}
	plain := BuildAnswerMessages(v, in)
	if strings.Contains(plain[len(plain)-1].Content, "지금 시각") || strings.Contains(plain[1].Content, "전)") {
		t.Fatal("no time leaves the prompt as evaluated")
	}
}

// varied is n sentences that differ from each other.
func varied(n int) string {
	var b strings.Builder
	for i := range n {
		fmt.Fprintf(&b, "이야기 %d번째는 조금씩 다른 내용입니다. ", i*7+3)
	}
	return b.String()
}
