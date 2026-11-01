package search

import "testing"

// The floor keeps laughter, jamo noise, punctuation, emoji and filler out of the message index. The
// same table is replayed by the sandbox (testdata/message_dense/fixture.json "eligibility").
func TestMeaningfulRunesAndMessageEligible(t *testing.T) {
	for _, c := range []struct {
		name, text string
		runes      int
		eligible   bool
	}{
		{"empty", "", 0, false},
		{"laughter", "ㅋㅋㅋㅋㅋㅋㅋㅋ", 0, false},
		{"mixed jamo", "ㅎㅎㅠㅠㅜㅜㅇㅇ", 0, false},
		{"jamo range end", "ㅏㅑㆎㆎ", 0, false},
		{"punctuation", "...!!!???~~~", 0, false},
		{"emoji", "👍👍🎉🎉🔥🔥🔥", 0, false},
		{"emoji with joiner and variation selector", "👨‍👩‍👧 ❤️ 1️⃣", 1, false},
		{"spaces and controls", " \t\n ​ ", 0, false},
		{"five syllables", "가나다라마", 5, false},
		{"six syllables", "가나다라마바", 6, true},
		{"six with laughter around", "ㅋㅋ가나다라마바ㅋㅋ", 6, true},
		{"five plus decoration", "가나다라마!!! 👍 ㅋㅋㅋ", 5, false},
		{"latin five", "hello", 5, false},
		{"latin six", "hellos", 6, true},
		{"latin with punctuation", "he-l-l-o!", 5, false},
		{"digits count", "123456", 6, true},
		{"kana", "こんにちは!", 5, false},
		{"han", "你好吗你好吗", 6, true},
		{"sentence", "내일 몇 시에 만나?", 7, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := MeaningfulRunes(c.text); got != c.runes {
				t.Fatalf("MeaningfulRunes(%q) = %d, want %d", c.text, got, c.runes)
			}
			if got := MessageEligible(c.text); got != c.eligible {
				t.Fatalf("MessageEligible(%q) = %v, want %v", c.text, got, c.eligible)
			}
		})
	}
}
