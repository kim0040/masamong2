package chat

import (
	"strings"
	"testing"
	"time"

	"masamong/internal/lang"
)

// A marked line carries its own date and time in the pack's short form, so a timestamp written
// before midnight reads the same after it — nothing in the stable prefix rewrites when the day
// rolls over. The marks: the first shown line, a line that opens a new stretch of talk (lineAgeAfter
// or more after the one before it), and the first user line after a bot line so marked. The current
// time still heads the final user message.
func TestAnswerLineTimeMarksSurviveMidnight(t *testing.T) {
	kst := time.FixedZone("KST", 9*3600)
	midnight := time.Date(2026, 11, 21, 0, 0, 0, 0, kst)
	history := []Turn{
		{Role: "user", Speaker: "A", Content: "old one", At: midnight.Add(-20 * time.Minute)},    // 11-20 23:40, first line
		{Role: "user", Speaker: "A", Content: "old two", At: midnight.Add(-19 * time.Minute)},    // a minute on: unmarked
		{Role: "assistant", Content: "back after a silence", At: midnight.Add(-9 * time.Minute)}, // 23:51, opens a stretch
		{Role: "user", Speaker: "A", Content: "old three", At: midnight.Add(-8 * time.Minute)},   // 23:52, the answer to it
	}
	want := []string{
		"A (11-20 23:40): old one",
		"A: old two",
		"(11-20 23:51) back after a silence",
		"A (11-20 23:52): old three",
	}
	for _, tag := range []string{"ko", "en", "es", "ja", "zh-Hans"} {
		t.Run(tag, func(t *testing.T) {
			pack, ok := lang.Get(tag)
			if !ok {
				t.Fatal(tag)
			}
			v := Voice{Pack: pack}
			before := BuildAnswerMessages(v, AnswerInput{System: "S", History: history, Message: "next", Now: midnight.Add(-time.Minute)})
			after := BuildAnswerMessages(v, AnswerInput{System: "S", History: history, Message: "next", Now: midnight.Add(time.Minute)})
			for i, w := range want {
				if before[i+1].Content != w {
					t.Fatalf("line %d before midnight: %q, want %q", i, before[i+1].Content, w)
				}
				if after[i+1].Content != before[i+1].Content {
					t.Fatalf("line %d rewrote at midnight: %q -> %q", i, before[i+1].Content, after[i+1].Content)
				}
				if strings.Contains(before[i+1].Content, "2026-") {
					t.Fatalf("the short form, not the full date: %q", before[i+1].Content)
				}
			}
			if !strings.Contains(after[len(after)-1].Content, "2026-11-21 00:01") {
				t.Fatalf("the current time still heads the user message: %q", after[len(after)-1].Content)
			}
		})
	}
}

// A line the cut puts first is marked because it is first, with its own date and time.
func TestAnswerLineTimeFirstAfterCutIsMarked(t *testing.T) {
	kst := time.FixedZone("KST", 9*3600)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, kst)
	rows := make([]Turn, 20)
	for i := range rows {
		rows[i] = Turn{Role: "user", Speaker: "A", Content: "row", At: now.Add(time.Duration(i) * time.Minute)}
	}
	// the cap of ten shows rows 10-19: the first shown line is marked with its own date and time
	msgs := BuildAnswerMessages(koVoice(), AnswerInput{System: "S", History: rows, Message: "next", MaxHistory: 10, Now: now})
	if got := msgs[1].Content; !strings.Contains(got, "(11-20 ") {
		t.Fatalf("the first shown line carries its own date and time: %q", got)
	}
}
