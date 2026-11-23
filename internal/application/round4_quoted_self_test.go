package application_test

import (
	"strings"
	"testing"
	"time"

	"masamong/internal/application"
)

// What others said about the caller goes into the answer prompt only for a
// whole request about the caller (the last sentence, in any pack), never for a
// clause inside other words or a request someone else is reported to have made.
// (The caller's own statements come on every turn; the marker is hearsay.)
func TestRound4QuotedSelfPrompt(t *testing.T) {
	t.Parallel()
	s, m, e := memorySvc(t)
	f := reviewPerson{s, m, e, time.Now().UTC().Add(-3 * time.Hour)}
	const marker = "round3-caller-private-fact"
	f.source(t, "g", "c", "round3-caller-source", "303", "지수", f.at)
	ev := f.source(t, "g", "c", "round3-friend-source", "202", "영희", f.at)
	f.fact(t, "g", "c", marker, "303", "202", ev)
	for _, tc := range []struct {
		name   string
		prompt string
		want   bool
	}{
		{"ko/quoted", "친구가 보낸 문장 인용이야. 뜻을 알려 줘.\n> 나 알아?", false},
		{"en/quoted", "What does this line from a song mean?\n> Remember me?", false},
		{"es/quoted", "¿Qué significa el mensaje que recibí?\n> ¿Me conoces?", false},
		{"ja/quoted", "このセリフの意味を教えて。\n> 僕はだれ？", false},
		{"zh-Hans/quoted", "这条引用是什么意思？\n> 你认识我吗？", false},
		{"en/direct", "Remember me?", true},
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
