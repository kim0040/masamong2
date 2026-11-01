package chat

import "testing"

func TestDetectMessageLanguage(t *testing.T) {
	for _, tc := range []struct{ message, want string }{
		{"what should I have for lunch?", "en"},
		{"おすすめのランチは？", "ja"},
		{"好きな食べ物は？", "ja"},
		{"北京天气怎么样", "zh-Hans"},
		{"점심 뭐 먹을까?", "ko"},
		{"¿Qué debería comer hoy?", "es"},
		{"¿QUÉ debería comer hoy?", "es"},
		{"🙂", ""},
	} {
		if got := DetectLanguage(tc.message); got != tc.want {
			t.Errorf("DetectLanguage(%q) = %q, want %q", tc.message, got, tc.want)
		}
	}
}

func TestAnswerTokenBudgetScalesForLanguageAndLongForm(t *testing.T) {
	if got := answerTokenLimit("안녕", "ko"); got <= answerTokenLimit("hello", "en") {
		t.Fatalf("CJK budget %d must exceed English budget %d", got, answerTokenLimit("hello", "en"))
	}
	if got := answerTokenLimit("조선 역사를 자세히 설명해줘", "ko"); got <= answerTokenLimit("안녕", "ko") {
		t.Fatalf("long-form budget %d must exceed the short-answer budget", got)
	}
}
