package application

import (
	"testing"

	"masamong/internal/memory/episodes"
)

func TestExplicitFactText(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"  Hello, World! ", "helloworld"},
		{"{@user1} likes tea", "likestea"},
		{"likestea {@user1}", "likestea"},
		{"커피를 좋아해요", "커피를좋아해요"},
		{"", ""},
		{"{@onlyref}", ""},
		{"dots…and spaces\t\nhere", "dotsandspaceshere"},
	} {
		if got := explicitFactText(tc.in); got != tc.want {
			t.Errorf("explicitFactText(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestRuneBigrams(t *testing.T) {
	if got := runeBigrams("ab"); len(got) != 1 {
		t.Fatalf("one pair from two runes: %v", got)
	}
	got := runeBigrams("abc")
	for _, want := range []string{"ab", "bc"} {
		if _, ok := got[want]; !ok {
			t.Fatalf("missing bigram %q in %v", want, got)
		}
	}
	if len(runeBigrams("a")) != 0 || len(runeBigrams("")) != 0 {
		t.Fatal("fewer than two runes make no bigrams")
	}
	if got := runeBigrams("가나다"); len(got) != 2 {
		t.Fatalf("bigrams are runes, not bytes: %v", got)
	}
}

func TestExplicitFactDice(t *testing.T) {
	if got := explicitFactDice("same words", "same words"); got != 1 {
		t.Fatalf("identical facts score %f", got)
	}
	if got := explicitFactDice("aaaa", "zzzz"); got != 0 {
		t.Fatalf("disjoint facts score %f", got)
	}
	if got := explicitFactDice("", "anything"); got != 0 {
		t.Fatalf("an empty side scores %f", got)
	}
	// Punctuation and refs are stripped before scoring: the same fact written differently still matches.
	if got := explicitFactDice("{@u1} likes green tea.", "Likes green tea!"); got < explicitFactDiceThreshold {
		t.Fatalf("the same fact scores below the dedupe threshold: %f", got)
	}
	if got := explicitFactDice("{@u1} likes green tea.", "{@u1} hates loud noise."); got >= explicitFactDiceThreshold {
		t.Fatalf("different facts reach the dedupe threshold: %f", got)
	}
}

func TestExplicitContains(t *testing.T) {
	if explicitContains(nil, "ref") {
		t.Fatal("found in an empty list")
	}
	if !explicitContains([]string{"a", "ref"}, "ref") {
		t.Fatal("member not found")
	}
	if explicitContains([]string{"ref2"}, "ref") {
		t.Fatal("a prefix match is not a member")
	}
}

func TestForgetPendingKey(t *testing.T) {
	if got := forgetPendingKey("g1", "p2"); got != "memory:forget:pending:g1:p2" {
		t.Fatalf("%q", got)
	}
}

func TestSameExplicitFact(t *testing.T) {
	base := func() episodes.Item {
		return episodes.Item{Kind: "fact", Text: "{@p1} likes green tea", Evidence: []string{"msg1"}, By: []string{"p1"}}
	}
	if !sameExplicitFact(base(), base(), "p1") {
		t.Fatal("the same fact is not detected as a repeat")
	}
	for _, tc := range []struct {
		name string
		mut  func(*episodes.Item)
	}{
		{"not a fact", func(it *episodes.Item) { it.Kind = "todo" }},
		{"no evidence", func(it *episodes.Item) { it.Evidence = nil }},
		{"different evidence", func(it *episodes.Item) { it.Evidence = []string{"msg2"} }},
		{"speaker not in By", func(it *episodes.Item) { it.By = []string{"p2"} }},
		{"speaker not referenced", func(it *episodes.Item) { it.Text = "someone likes green tea" }},
		{"far text", func(it *episodes.Item) { it.Text = "{@p1} hates loud noise a lot" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			it := base()
			tc.mut(&it)
			if sameExplicitFact(it, base(), "p1") {
				t.Fatal("a different item passes as the same fact")
			}
		})
	}
	if sameExplicitFact(base(), base(), "") {
		t.Fatal("an empty reference matches")
	}
}
