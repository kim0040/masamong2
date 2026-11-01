package engine

import "testing"

// The indexing preview's token estimate: Hangul is priced near the billed Korean embedding usage
// (2026-10-11), other scripts keep their earlier rates.
func TestApproxTokensByScript(t *testing.T) {
	for _, c := range []struct {
		text string
		want int
	}{
		{"", 0},
		{"가나다라마바사아자차", 14}, // 10 syllables × 1.4
		{"abcdefg", 2},     // 7 / 3.5
		{"漢字かな", 3},        // 4 / 1.3
		{"철수: hi", 4},      // 2×1.4 + 5/3.5 = 4.23
	} {
		if got := approxTokens(c.text); got != c.want {
			t.Errorf("%q: %d, want %d", c.text, got, c.want)
		}
	}
}
