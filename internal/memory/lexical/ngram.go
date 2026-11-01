// Package lexical is the memory lexical retriever: script-aware n-gram expansion plus an in-memory
// SQLite FTS5 index (unicode61, bm25 k1=1.2 b=0.75).
//
// The default Tokenizer ports tools/evaluation/lexical.py (the "bigram" variant the test sandbox
// evaluated) so the daemon ranks exactly like the sandbox: a Korean word with a particle attached
// (초코우유를) still matches its stem (초코우유) because both are indexed as overlapping syllable
// bigrams, and the query is an OR of per-word phrases ranked by BM25. Other languages configure the
// Tokenizer from their language pack (internal/lang): which scripts are n-grammed (Han, Kana for
// Chinese/Japanese), whether Latin words are indexed whole, and the stop words.
package lexical

import (
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/text/unicode/norm"

	"masamong/internal/lang"
)

// Tokenizer turns text into space-separated index tokens. The zero value is not useful; use
// DefaultTokenizer or NewTokenizer.
type Tokenizer struct {
	N          int // n-gram size (2 = the sandbox "bigram" variant)
	hangul     bool
	han        bool
	kana       bool
	latinWords bool
	stop       map[string]bool
}

// NewTokenizer builds a tokenizer. scripts names the scripts expanded into n-grams (hangul, han,
// kana); latinWords keeps Latin-script words whole instead of one token per letter.
func NewTokenizer(n int, scripts []string, latinWords bool, stopWords []string) Tokenizer {
	if n <= 0 {
		n = 2
	}
	t := Tokenizer{N: n, latinWords: latinWords, stop: map[string]bool{}}
	for _, s := range scripts {
		switch strings.ToLower(s) {
		case "hangul":
			t.hangul = true
		case "han":
			t.han = true
		case "kana":
			t.kana = true
		}
	}
	for _, w := range stopWords {
		t.stop[w] = true
	}
	return t
}

// FromLanguage builds the tokenizer a (merged) language pack asks for.
func FromLanguage(s lang.Search) Tokenizer {
	return NewTokenizer(2, s.NgramScripts, s.LatinWords, s.StopWords)
}

// DefaultTokenizer is the default language's tokenizer (Korean: the sandbox-parity bigram variant,
// whose stop words are lexical.py STOP_WORDS).
func DefaultTokenizer() Tokenizer {
	p, _ := lang.Get(lang.Default)
	return FromLanguage(p.Search)
}

// isHangulSyllable is the sandbox's `[가-힣]` class (precomposed syllables only; jamo such as ㅋ are not).
func isHangulSyllable(r rune) bool { return r >= '가' && r <= '힣' }

func (t Tokenizer) ngramRune(r rune) bool {
	switch {
	case t.hangul && isHangulSyllable(r):
		return true
	case t.han && unicode.Is(unicode.Han, r):
		return true
	case t.kana && (unicode.Is(unicode.Hiragana, r) || unicode.Is(unicode.Katakana, r)):
		return true
	}
	return false
}

func isLatinWordRune(r rune) bool {
	return unicode.Is(unicode.Latin, r) || (r < 0x80 && (unicode.IsDigit(r) || r == '_'))
}

// isPySpace is Python's str.isspace / `\s` for str patterns (Py_UNICODE_ISSPACE). It differs from
// unicode.IsSpace on U+001C..U+001F, which Python treats as whitespace.
func isPySpace(r rune) bool {
	switch {
	case r >= 0x09 && r <= 0x0D, r >= 0x1C && r <= 0x20:
		return true
	case r == 0x85, r == 0xA0, r == 0x1680, r >= 0x2000 && r <= 0x200A:
		return true
	case r == 0x2028, r == 0x2029, r == 0x202F, r == 0x205F, r == 0x3000:
		return true
	}
	return false
}

// ToNgrams expands runs of n-gram scripts into overlapping n-grams and keeps every other non-space
// character as its own token (`초코우유를` → `초코 코우 우유 유를`); with latinWords, a Latin word is one
// token. Input is NFKC-normalized first. With DefaultTokenizer the output is identical to
// lexical.to_ngrams for the same input.
func (t Tokenizer) ToNgrams(text string) string {
	n := t.N
	runes := []rune(nfkc(text))
	// Cold builds expand every leaf again. Writing tokens straight to the final
	// string avoids a slice plus a separately allocated string for each gram;
	// rune writes keep normalization and token separators exactly unchanged.
	var out strings.Builder
	out.Grow(len(text))
	first := true
	write := func(token []rune) {
		if !first {
			out.WriteByte(' ')
		}
		first = false
		for _, r := range token {
			out.WriteRune(r)
		}
	}
	for i := 0; i < len(runes); {
		r := runes[i]
		if t.ngramRune(r) {
			j := i
			for j < len(runes) && t.ngramRune(runes[j]) {
				j++
			}
			run := runes[i:j]
			if len(run) < n {
				write(run)
			} else {
				for k := 0; k+n <= len(run); k++ {
					write(run[k : k+n])
				}
			}
			i = j
			continue
		}
		if t.latinWords && isLatinWordRune(r) {
			j := i
			for j < len(runes) && isLatinWordRune(runes[j]) {
				j++
			}
			write(runes[i:j])
			i = j
			continue
		}
		if !isPySpace(r) {
			write(runes[i : i+1])
		}
		i++
	}
	return out.String()
}

// cgj is U+034F COMBINING GRAPHEME JOINER.
const cgj = rune(0x034F)

// nfkc is NFKC as Python's unicodedata.normalize computes it. x/text follows the UAX #15 stream-safe
// format and puts a CGJ after 30 non-starters in a row (forty compatibility jamo, say); Python does not,
// and the CGJ would split one word into two phrases in Go only. Joiners the text already had are kept.
func nfkc(s string) string {
	out := norm.NFKC.String(s)
	if !strings.ContainsRune(out, cgj) || strings.ContainsRune(s, cgj) {
		return out
	}
	return strings.ReplaceAll(out, string(cgj), "")
}

// punct is `[^\w가-힣]+` with Python's Unicode \w (letters, numbers, underscore).
var punct = regexp.MustCompile(`[^\p{L}\p{N}_가-힣]+`)

// QueryWords splits a query into discriminating words (≥2 characters, not a stop word).
func (t Tokenizer) QueryWords(q string) []string {
	cleaned := punct.ReplaceAllString(nfkc(q), " ")
	var out []string
	for _, w := range strings.FieldsFunc(cleaned, isPySpace) {
		if len([]rune(w)) >= 2 && !t.stop[w] && !t.stop[strings.ToLower(w)] {
			out = append(out, w)
		}
	}
	return out
}

// MatchExpression is the FTS5 MATCH expression the sandbox sends: one quoted n-gram phrase per
// query word, joined with OR (lexical._or_phrases).
func (t Tokenizer) MatchExpression(q string) string {
	var parts []string
	for _, w := range t.QueryWords(q) {
		grams := strings.Fields(t.ToNgrams(w))
		if len(grams) == 0 {
			continue
		}
		parts = append(parts, `"`+strings.ReplaceAll(strings.Join(grams, " "), `"`, `""`)+`"`)
	}
	return strings.Join(parts, " OR ")
}

// ToNgrams, QueryWords and MatchExpression with the default (sandbox) tokenizer.
var defaultTok = DefaultTokenizer()

func ToNgrams(text string, n int) string {
	t := defaultTok
	t.N = n
	return t.ToNgrams(text)
}

func QueryWords(q string) []string { return defaultTok.QueryWords(q) }

func MatchExpression(q string, n int) string {
	t := defaultTok
	t.N = n
	return t.MatchExpression(q)
}
