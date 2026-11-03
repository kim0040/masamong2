package chat

import (
	"context"
	"slices"
	"testing"
)

// stableLLM is loopLLM reporting whether its role binds the reasoning it sends back to the tool list
// (StableToolsLLM).
type stableLLM struct {
	*loopLLM
	stable bool
}

func (s stableLLM) StableTools(string) bool { return s.stable }

// Effort review round 2, finding 4: the loop freezes its tool list only for a role that may bind it
// (StableToolsLLM) and only once an answer that binds it (LoopReply.BindsTools: an Anthropic answer, a
// fallback's included) is in the loop. Until then a closed tool leaves the list, as for any model.
func TestToolLoopFreezesToolsOnlyAfterABindingAnswer(t *testing.T) {
	// round 1 reads the web (writing tools close) and the quote fails once; round 2 the quote fails
	// again (it is removed)
	script := func(binds1, binds2 bool) []LoopReply {
		return []LoopReply{
			{Calls: []LoopCall{call("a", "search__web", `{"query":"apple"}`), call("b", "finance__quote", `{"symbol":"AAPL"}`)}, BindsTools: binds1},
			{Calls: []LoopCall{call("c", "finance__quote", `{"symbol":"APPLE"}`)}, BindsTools: binds2},
			{Calls: []LoopCall{call("d", "weather__query", `{"region":"Busan"}`)}},
		}
	}
	run := func(llm LLM, inner *loopLLM) [][]string {
		t.Helper()
		rec := &recorder{fail: map[string]bool{"finance.quote": true}}
		if _, err := (&Runtime{LLM: llm, Tools: rec.run}).Answer(context.Background(), loopRequest("apple price", ToolModeLoop)); err != nil {
			t.Fatal(err)
		}
		if len(inner.offered) != 3 {
			t.Fatalf("three rounds: %v", inner.offered)
		}
		return inner.offered
	}
	closed := func(offer []string) bool { return !slices.Contains(offer, "polls__create") }

	// a model that binds nothing, or a role that says it does not: closed tools leave the list
	for name, mk := range map[string]func(*loopLLM) LLM{
		"no StableToolsLLM":   func(l *loopLLM) LLM { return l },
		"StableTools = false": func(l *loopLLM) LLM { return stableLLM{l, false} },
	} {
		inner := &loopLLM{replies: script(true, true)}
		offered := run(mk(inner), inner)
		if !closed(offered[1]) || slices.Contains(offered[2], "finance__quote") {
			t.Fatalf("%s: closed tools leave the list: %v", name, offered)
		}
	}

	// a stable role whose first answer binds: round 1's list on every round
	inner := &loopLLM{replies: script(true, false)}
	offered := run(stableLLM{inner, true}, inner)
	if !slices.Equal(offered[1], offered[0]) || !slices.Equal(offered[2], offered[0]) {
		t.Fatalf("frozen at round 1: %v", offered)
	}

	// a non-Anthropic primary answers round 1, its Anthropic fallback round 2: round 2 gets the filtered
	// list, and round 3 the list round 2's binding answer was given (the quote, removed since, included)
	inner = &loopLLM{replies: script(false, true)}
	offered = run(stableLLM{inner, true}, inner)
	if !closed(offered[1]) || !slices.Contains(offered[1], "finance__quote") {
		t.Fatalf("round 2 not frozen yet: %v", offered)
	}
	if !slices.Equal(offered[2], offered[1]) {
		t.Fatalf("round 3 keeps the list the binding answer saw: %v", offered)
	}
}
