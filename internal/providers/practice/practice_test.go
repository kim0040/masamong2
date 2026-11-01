package practice

import (
	"strings"
	"testing"
)

func TestPickEvidenceQuotesTheLineThatAnswers(t *testing.T) {
	prompt := strings.Join([]string{
		"<기억 요약 참고용>", "[1] 캠핑 계획", "</기억 요약>",
		"<기억>",
		"[1] 날짜=2026-05-02 사람=지우, 도윤",
		"[kakao:practice · 2026-05-02 · 지우, 도윤]",
		"지우: 다들 5월 연휴에 뭐 해?",
		"도윤: 나도! 텐트는 내가 챙길게",
		"서아: 그럼 고기는 내가 사 갈게",
		"",
		"[2] 날짜=2026-06-14 사람=서아",
		"서아: 백운대 찍고 내려왔다",
		"</기억>",
		"",
		"시험자: 캠핑 갈 때 텐트 누가 챙기기로 했지?",
	}, "\n")
	got := pickEvidence(prompt, 1)
	if got != "도윤: 나도! 텐트는 내가 챙길게" {
		t.Fatalf("picked %q", got)
	}
	if pickEvidence("no evidence here\n\n시험자: 안녕", 3) != "" {
		t.Fatal("a prompt without evidence must not invent any")
	}
}
