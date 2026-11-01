package episodes

import (
	"strings"

	"masamong/internal/memory/lexical"
)

// Query is the caller's conversation as words, for finding the stored episodes and topics that share
// words with it (the read path, docs/plan-memory-conversation.md §5.4.6). It uses the language's lexical
// tokenizer and costs no model call. A word counts as found in a text when most of its n-grams are
// there, also after its trailing particle is cut ("등산은" finds "등산").
type Query struct {
	tok       *lexical.Tokenizer
	particles string
	words     []queryWord
}

type queryWord struct {
	forms   [][]string // n-grams of the word and of the word without its particle
	message bool       // from the message itself (else from the conversation around it)
}

// NewQuery reads the message and the lines around it (newest last).
func NewQuery(t *lexical.Tokenizer, particles, message string, context []string) Query {
	q := Query{tok: t, particles: particles}
	seen := map[string]bool{}
	add := func(text string, fromMessage bool) {
		for _, w := range t.QueryWords(strings.ToLower(text)) {
			if seen[w] {
				continue
			}
			seen[w] = true
			qw := queryWord{message: fromMessage}
			qw.forms = append(qw.forms, strings.Fields(t.ToNgrams(w)))
			if r := []rune(w); len(r) >= 3 && strings.ContainsRune(particles, r[len(r)-1]) {
				qw.forms = append(qw.forms, strings.Fields(t.ToNgrams(string(r[:len(r)-1]))))
			}
			q.words = append(q.words, qw)
		}
	}
	add(message, true)
	for i := len(context) - 1; i >= 0; i-- {
		add(context[i], false)
	}
	return q
}

// Empty reports whether the conversation has no discriminating word.
func (q Query) Empty() bool { return len(q.words) == 0 }

// Words is how many words the query has.
func (q Query) Words() int { return len(q.words) }

// FromMessage reports whether word i came from the message itself.
func (q Query) FromMessage(i int) bool { return q.words[i].message }

// Matches lists the words of the query found in text (their indexes).
func (q Query) Matches(text string) []int {
	if len(q.words) == 0 || strings.TrimSpace(text) == "" {
		return nil
	}
	grams := map[string]bool{}
	for _, g := range tokens(q.tok, strings.ToLower(text)) {
		grams[g] = true
	}
	var out []int
	for i, w := range q.words {
		for _, form := range w.forms {
			if len(form) == 0 {
				continue
			}
			hit := 0
			for _, g := range form {
				if grams[g] {
					hit++
				}
			}
			if hit*3 >= len(form)*2 {
				out = append(out, i)
				break
			}
		}
	}
	return out
}
