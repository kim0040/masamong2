package search

import (
	"strings"
	"unicode/utf8"

	"masamong/internal/lang"
)

// NameMatcher checks a whole display-name span. Unlike lexical retrieval,
// selecting an account must not cross even a change of script or a digit.
func NameMatcher(language lang.Search) func(text, label string) bool {
	rules := newTextRules(&language)
	particles := strings.Fields("은 는 이 가 을 를 의 도 만 한테 에게 께 랑 이랑 하고 와 과 아 야 이는 이가")
	for _, r := range language.Particles {
		particles = append(particles, string(r))
	}
	suffix := map[string]bool{"": true}
	for _, honor := range strings.Fields("형 누나 오빠 언니 님 씨 さん ちゃん 君 様 先生 女士") {
		suffix[honor] = true
		for _, p := range particles {
			suffix[honor+p] = true
		}
	}
	for _, p := range particles {
		suffix[p] = true
	}
	return func(text, label string) bool {
		tail, ok := strings.CutPrefix(text, label)
		return ok && utf8.RuneCountInString(label) > 1 && !rules.pronoun(label) && suffix[tail]
	}
}
