package lexical

import (
	"strings"
	"testing"
)

func TestNgramExpansionAvoidsPerTokenAllocations(t *testing.T) {
	// Long leaves made cold startup allocate one short string per token. The
	// bound allows builder growth but fails the former thousands of allocations.
	tok := DefaultTokenizer()
	text := strings.Repeat("캠핑 가평 abc 🎉 ", 300)
	want := strings.TrimSpace(strings.Repeat("캠핑 가평 a b c 🎉 ", 300))
	if tok.ToNgrams(text) != want {
		t.Fatal("expanded tokens changed")
	}
	allocs := testing.AllocsPerRun(5, func() { _ = tok.ToNgrams(text) })
	if allocs > 20 {
		t.Fatalf("per-leaf allocations %.0f, want <=20", allocs)
	}
}
