package conversation

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// A transcript over budget is cut at a snap boundary the line count alone decides (SnapCut), so the
// prompt built on it keeps a stable prefix. While no boundary can hold the minimum lines, the
// minimum is kept and the first line follows each new row.
func TestSelectCutsAtSnapBoundaries(t *testing.T) {
	base := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	var lines []Line
	for i := range 30 {
		lines = append(lines, Line{ID: string(rune('a' + i)), Author: "C", At: base.Add(time.Duration(i) * time.Minute), Text: strings.Repeat("x", 60)})
	}
	kept, omitted := Select(lines, 700, 8)
	if len(kept) == len(lines) || omitted == 0 {
		t.Fatalf("over budget: %d kept %d omitted", len(kept), omitted)
	}
	if len(kept) != 8 || kept[0].ID != "w" {
		// ten 65-rune rows fit 700: the earliest fit snaps to boundary 32, which cannot hold the
		// minimum eight, so the floor keeps exactly eight and the first line moves with the new row
		t.Fatalf("the minimum lines are kept while a boundary cannot hold them: %d kept, first %q", len(kept), kept[0].ID)
	}
	grown := append(append([]Line(nil), lines...),
		Line{ID: "5", Author: "C", At: base.Add(30 * time.Minute), Text: "국수 먹자"},
		Line{ID: "6", Author: "bot", Bot: true, At: base.Add(31 * time.Minute), Text: "좋아"})
	again, _ := Select(grown, 700, 8)
	if len(again) != 8 || again[0].ID != "y" {
		t.Fatalf("still floor-bound a turn later: %d kept, first %q", len(again), again[0].ID)
	}
	// a budget that keeps a whole boundary of rows pins the first line to the boundary, and it stays
	// when the transcript grows: sixteen 65-rune rows fit 1040 runes, so the earliest fit 14 snaps
	// forward to 16 and the same 'q' heads both turns
	kept, _ = Select(lines, 1040, 8)
	if kept[0].ID != "q" || len(kept) != 14 {
		t.Fatalf("a boundary's worth fits: %d kept, first %q", len(kept), kept[0].ID)
	}
	again, _ = Select(grown, 1040, 8)
	if again[0].ID != kept[0].ID {
		t.Fatalf("a turn later the boundary must not move: kept %q, again %q", kept[0].ID, again[0].ID)
	}
}

// A dense stretch of talk — no silence anywhere — is where the suffix recomputed per turn used to
// move the first line every turn. Now the earliest fit snaps forward to a boundary, so consecutive
// turns keep the same first line until the budget forces the next one (the reviewer's 250-rune row
// per minute).
func TestSelectDenseHistorySnapsToBoundaries(t *testing.T) {
	base := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	lines := make([]Line, 60)
	for i := range lines {
		lines[i] = Line{ID: fmt.Sprintf("line-%02d", i), Author: "A", At: base.Add(time.Duration(i) * time.Minute),
			Text: fmt.Sprintf("line-%02d %s", i, strings.Repeat("a", 237))} // 250 runes a row
	}
	first := func(n int) (string, int) {
		kept, dropped := Select(lines[:n], 5000, 8)
		return kept[0].ID, dropped
	}
	// twenty 250-rune rows just fill the budget
	if got, dropped := first(20); got != "line-00" || dropped != 0 {
		t.Fatalf("the budget just fits: first %s dropped %d", got, dropped)
	}
	// over it the earliest fit (one more row out per new row) wants boundary 16, which cannot yet
	// hold the minimum eight: the floor keeps eight and the first line still follows each new row
	for n, want := range map[int]string{21: "line-13", 22: "line-14", 23: "line-15"} {
		if got, dropped := first(n); got != want || dropped != n-8 {
			t.Fatalf("%d rows: first %s dropped %d, want %s", n, got, dropped, want)
		}
	}
	// from 24 rows boundary 16 holds the minimum: the first line stays line-16 until the tail grows
	// past it — never a row in between — and after boundary 32 the same again
	for _, n := range []int{24, 25, 26, 36} {
		if got, _ := first(n); got != "line-16" {
			t.Fatalf("%d rows: first %s, want line-16", n, got)
		}
	}
	for n, want := range map[int]string{37: "line-29", 38: "line-30", 39: "line-31"} {
		if got, _ := first(n); got != want {
			t.Fatalf("%d rows (floor-bound before boundary 32 holds): first %s, want %s", n, got, want)
		}
	}
	for _, n := range []int{40, 41, 42, 52} {
		if got, _ := first(n); got != "line-32" {
			t.Fatalf("%d rows: first %s, want line-32", n, got)
		}
	}
}
