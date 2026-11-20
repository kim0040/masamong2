package search

import (
	"strings"

	"masamong/internal/lang"
)

// NameMatcher shares search's token boundaries and pronoun exclusions with
// callers matching observed display names, without constructing a search index.
func NameMatcher(language lang.Search) func(text, label string) bool {
	language.LatinWords = true // Discord display names can mix scripts in any server
	rules := newTextRules(&language)
	return func(text, label string) bool {
		return !rules.pronoun(label) && rules.tokenInText(strings.ToLower(text), strings.ToLower(label), nil)
	}
}
