package search

import "unicode"

// MinMessageRunes is how many meaningful characters a message needs before it gets its own vector.
// Shorter lines ("ㅋㅋㅋ", "ok", "👍", a lone "네") say too little to be found by meaning, and they are the
// bulk of a chat history, so leaving them out saves most of the storage and embedding cost.
const MinMessageRunes = 6

// MeaningfulRunes counts the characters of a message that carry content: Unicode letters (categories L*)
// and numbers (N*), once per code point, except the Hangul compatibility jamo U+3131–U+318E (ㅋ, ㅎ, ㅠ,
// ㅜ, ㅇ …: laughter and keyboard noise). Everything else never counts: white space and control
// characters, punctuation, symbols and emoji, combining marks, format characters (variation selectors,
// the zero-width joiner, the keycap mark …), private-use and unassigned code points.
//
// The rule has no word lists, so it holds for any language pack. experimental/rag-leaf-parent mirrors it
// as meaningful_runes; the fixture in testdata/message_dense pins both. The two runtimes ship different
// Unicode versions (Go's unicode tables and Python's unicodedata), so a code point assigned as a letter
// or number in one version and still unassigned in the other can count in only one of them; that is
// rare and only changes whether such a message gets a vector of its own.
func MeaningfulRunes(text string) int {
	n := 0
	for _, r := range text {
		if meaningfulRune(r) {
			n++
		}
	}
	return n
}

func meaningfulRune(r rune) bool {
	if r >= 0x3131 && r <= 0x318E {
		return false
	}
	return unicode.IsLetter(r) || unicode.IsNumber(r)
}

// MessageEligible reports whether a message's own text (without the speaker label) is long enough to
// be embedded as a message vector: at least MinMessageRunes meaningful characters.
func MessageEligible(content string) bool { return MeaningfulRunes(content) >= MinMessageRunes }
