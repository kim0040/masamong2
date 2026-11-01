package lexical

import (
	"strings"
	"testing"
)

// Forty compatibility vowels (a long ㅠㅠ) are forty non-starters once decomposed: x/text puts a CGJ
// after the thirtieth, Python does not. The query must stay one word and the text one run of tokens.
func TestNFKCDropsStreamSafeJoiner(t *testing.T) {
	jamo := strings.Repeat("ㅠ", 25) + strings.Repeat("ㅜ", 15)
	if got := QueryWords(jamo); len(got) != 1 || strings.ContainsRune(got[0], cgj) {
		t.Fatalf("query split by the stream-safe joiner: %q", got)
	}
	if got := MatchExpression(jamo, 2); strings.Count(got, `"`) != 2 {
		t.Fatalf("one phrase expected: %s", got)
	}
	if got := ToNgrams(jamo, 2); strings.ContainsRune(got, cgj) || len(strings.Fields(got)) != 40 {
		t.Fatalf("index tokens: %q", got)
	}
	// a joiner the text already had is the writer's, and stays
	if in := "a" + string(cgj) + "b"; nfkc(in) != in {
		t.Fatalf("existing joiner removed: %q", nfkc(in))
	}
}
