package discord

import "testing"

func TestFortuneAnimalsVerb(t *testing.T) {
	verbs := wordsFor("fortune", "verb", "today", "detail", "month", "year", "animals")
	for _, w := range []string{"animals", "띠", "띠별", "生肖"} {
		if verbs[w] != "animals" {
			t.Fatalf("%q: %q", w, verbs[w])
		}
	}
	verb, rest := splitVerb("띠", "", verbs)
	if action, in := fortuneRequest(verb, rest); action != "fortune.query" || in["what"] != "animals" {
		t.Fatalf("%s %v", action, in)
	}
}
