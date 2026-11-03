package fortune

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestParseReadingTakesTheKeyLines(t *testing.T) {
	r := ParseReading("SCORE: 82점\nSUMMARY: 차분히 준비한 일이\n빛을 보는 날이에요.\n- **WORK**: 집중이 잘 돼요.\nMONEY: 충동구매만 조심\nDO: 오전에 중요한 일부터 | 일찍 쉬기 |\nAVOID: 바로 답장하기\nLUCKY_COLOR: 파랑\nNOTE: 무시되는 키\nUNKNOWN line goes to nobody")
	if !r.Structured() || r.Ranked() {
		t.Fatalf("structured %v ranked %v", r.Structured(), r.Ranked())
	}
	if r.Score != 82 {
		t.Fatalf("score %d", r.Score)
	}
	if got := r.Values["SUMMARY"]; got != "차분히 준비한 일이 빛을 보는 날이에요." {
		t.Fatalf("a wrapped value continues on the next line: %q", got)
	}
	if r.Values["WORK"] != "집중이 잘 돼요." {
		t.Fatalf("bold and list marks are dropped: %q", r.Values["WORK"])
	}
	if items := Items(r.Values["DO"]); len(items) != 2 || items[1] != "일찍 쉬기" {
		t.Fatalf("items %q", items)
	}
	if _, has := r.Values["NOTE"]; has {
		t.Fatal("unknown keys are not kept")
	}
	if r.Values["LUCKY_COLOR"] != "파랑" {
		t.Fatalf("the unknown NOTE line and its continuation ran into LUCKY_COLOR: %q", r.Values["LUCKY_COLOR"])
	}
	if _, has := r.Values["SCORE"]; has {
		t.Fatal("the score is a number, not a value")
	}
}

func TestParseReadingFallsBackOnFreeText(t *testing.T) {
	for _, text := range []string{
		"**오늘의 흐름**: 85점\n오늘은 좋은 날이에요.",
		"SUMMARY: 한 줄뿐",
		"SCORE: 70\nWORK: 요약이 없음",
		"",
	} {
		if r := ParseReading(text); r.Structured() || r.Ranked() {
			t.Errorf("%q must be shown as written: %+v", text, r)
		}
	}
}

func TestParseReadingScoreOutOfRange(t *testing.T) {
	for in, want := range map[string]int{"150": -1, "-5": -1, "abc": -1, "100": 100, "0": 0, "약 73 정도": 73} {
		if got := ParseReading("SCORE: " + in + "\nSUMMARY: s").Score; got != want {
			t.Errorf("SCORE %q = %d, want %d", in, got, want)
		}
	}
}

func TestParseReadingRepeatedKeyKeepsTheFirst(t *testing.T) {
	r := ParseReading("SUMMARY: first\nSUMMARY: second\nmore of second\nWORK: w")
	if r.Values["SUMMARY"] != "first" {
		t.Fatalf("summary %q", r.Values["SUMMARY"])
	}
}

func TestParseReadingClipsLongValues(t *testing.T) {
	r := ParseReading("SUMMARY: " + strings.Repeat("가", 1000) + "\nWORK: " + strings.Repeat("나", 1000))
	if n := utf8.RuneCountInString(r.Values["SUMMARY"]); n != 400 {
		t.Fatalf("summary %d runes", n)
	}
	if n := utf8.RuneCountInString(r.Values["WORK"]); n != 300 || !strings.HasSuffix(r.Values["WORK"], "…") {
		t.Fatalf("work %d runes", n)
	}
}

func TestParseReadingRanks(t *testing.T) {
	var b strings.Builder
	for _, l := range []string{
		"RANK: 3 | 처녀자리 | 셋째 | 초록 | 3",
		"RANK: 1위 | 염소자리 | 첫째 | 파랑 | 7",
		"RANK: 1 | 사자자리 | 같은 순위 둘째 줄 | 빨강 | 1",
		"RANK: 13 | 뱀주인자리 | 범위 밖",
		"RANK: 2 | 물병자리",
		"RANK: 2 | 물병자리 | 둘째",
		"RANK: 4 | 양자리 | 넷째",
		"RANK: 5 | 황소자리 | 다섯째",
		"RANK: 6 | 쌍둥이자리 | 여섯째",
	} {
		b.WriteString(l + "\n")
	}
	r := ParseReading(b.String())
	if !r.Ranked() || r.Structured() {
		t.Fatalf("ranked %v structured %v", r.Ranked(), r.Structured())
	}
	if len(r.Ranks) != 6 || r.Ranks[0].Sign != "염소자리" || r.Ranks[0].Color != "파랑" || r.Ranks[0].Number != "7" {
		t.Fatalf("ranks %+v", r.Ranks)
	}
	if r.Ranks[1].Comment != "둘째" || r.Ranks[2].Pos != 3 {
		t.Fatalf("a line without a comment is skipped, the order is by rank: %+v", r.Ranks)
	}
	if ParseReading("RANK: 1 | a | b\nRANK: 2 | c | d").Ranked() {
		t.Fatal("two lines are not a ranking")
	}
}

func TestScoreBar(t *testing.T) {
	for score, want := range map[int]string{82: "▰▰▰▰▰▰▰▰▱▱", 0: "▱▱▱▱▱▱▱▱▱▱", 100: "▰▰▰▰▰▰▰▰▰▰", 45: "▰▰▰▰▰▱▱▱▱▱", -1: ""} {
		if got := ScoreBar(score); got != want {
			t.Errorf("ScoreBar(%d) = %q, want %q", score, got, want)
		}
	}
}

func TestParseReadingDropsUnknownKeyLines(t *testing.T) {
	r := ParseReading("SUMMARY: 좋은 날\nLUCKY_COLOR: 파랑\nNOTE: 무시되는 키\n이어지는 줄\nLUCKY_DIRECTION: 북쪽")
	if got := r.Values["LUCKY_COLOR"]; got != "파랑" {
		t.Fatalf("an unknown key line ran into the value above: %q", got)
	}
	if got := r.Values["LUCKY_DIRECTION"]; got != "북쪽" {
		t.Fatalf("LUCKY_DIRECTION: %q", got)
	}
	if _, has := r.Values["NOTE"]; has {
		t.Fatal("unknown keys are not kept")
	}
	if strings.Contains(r.Values["LUCKY_COLOR"]+r.Values["SUMMARY"], "이어지는") {
		t.Fatal("an unknown key's continuation goes nowhere")
	}
}
