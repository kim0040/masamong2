package application_test

import (
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
)

// The caller's own card goes into the answer prompt only for a whole request
// about the caller (the last sentence, in any pack), never for a clause inside
// other words or a request someone else is reported to have made.
func TestRound3SelfPredicatePrompts(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	const marker = "round3-caller-private-fact"
	ev := f.source(t, "g", "c", "round3-caller-source", "303", "지수", f.at)
	f.fact(t, "g", "c", marker, "303", "303", ev)
	for _, tc := range []struct {
		name   string
		prompt string
		want   bool
	}{
		{"ko/explicit", "날 기억해?", true},
		{"ko/about-me", "나에 대해 뭐 알아?", true},
		{"ko/bot-name", "마사몽아, 내가 누구야?", true},
		{"ko/after-greeting", "안녕! 날 기억해요? ㅋㅋ", true},
		{"ko/round2-aside", "나는 기억력이 좋아. 오늘 서울 날씨 알려줘.", false},
		{"ko/unrelated-word", "내 정보통신 과제 마감 언제야?", false},
		{"ko/reported", "날 기억해? 라고 걔가 물었어", false},
		{"ko/statement", "나 기억해.", false},
		{"en/explicit", "What do you know about me?", true},
		{"en/short-explicit-recall", "Remember me?", true},
		{"en/greeting", "Hey, do you remember me? :)", true},
		{"en/weather", "What is the weather today?", false},
		{"en/quoted", "\"Do you remember me?\" he asked.", false},
		{"en/imperative", "Remember me to your mom.", false},
		{"es/explicit", "¿Quién soy?", true},
		{"es/recall", "Oye, ¿te acuerdas de mí?", true},
		{"es/remind-not-recall", "Tú me recuerdas a mi hermana. ¿Qué tiempo hace hoy?", false},
		{"es/remind-alone", "Me recuerdas a mi hermana.", false},
		{"ja/explicit", "私はどんな人？", true},
		{"ja/recall", "ねえ、私のこと覚えてる？", true},
		{"ja/embedded-statement", "私のことを覚えてる人に聞いた。今日の天気を教えて。", false},
		{"ja/reported", "私のこと覚えてる？って聞かれた", false},
		{"zh-Hans/explicit", "我是谁？", true},
		{"zh-Hans/recall", "你好！你还记得我吗？", true},
		{"zh-Hans/embedded-statement", "他问你还记得我吗，我没回答。今天天气怎么样？", false},
		{"zh-Hans/reported", "你还记得我吗？他问。", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tag, _, _ := strings.Cut(tc.name, "/")
			turn(t, f.Service, "303", "지수", tc.prompt, func(in *application.ChatInput) { in.Locale = tag })
			prompt := f.model.last("answer")
			got := block(prompt, cardOpen, cardClose) +
				block(prompt, "<person_cards for reference>", "</person_cards>") +
				block(prompt, "<fichas_de_personas de referencia>", "</fichas_de_personas>") +
				block(prompt, "<人物カード 参考用>", "</人物カード>") +
				block(prompt, "<人物卡片 参考>", "</人物卡片>")
			if strings.Contains(got, marker) != tc.want {
				t.Errorf("prompt %q: caller marker want=%t, actual card=%q", tc.prompt, tc.want, got)
			}
		})
	}
}
