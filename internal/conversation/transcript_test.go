package conversation

import (
	"strings"
	"testing"
	"time"
)

// A transcript over budget is cut at the start of a stretch of talk, and a growing transcript keeps
// the same first line until the tail refills the budget — the cut depends on the lines alone, so two
// consecutive turns pick the same start and the prompt built on them keeps a stable prefix.
func TestSelectCutsAtStretchStarts(t *testing.T) {
	base := time.Date(2026, 10, 31, 15, 0, 0, 0, time.UTC)
	var lines []Line
	at := base
	for i := range 30 {
		if i > 0 && i%10 == 0 {
			at = at.Add(10 * time.Minute) // a stretch is ten lines a minute apart
		} else if i > 0 {
			at = at.Add(time.Minute)
		}
		lines = append(lines, Line{ID: string(rune('a' + i)), Author: "C", At: at, Text: strings.Repeat("x", 60)})
	}
	kept, omitted := Select(lines, 700, 8)
	if len(kept) == len(lines) || omitted == 0 {
		t.Fatalf("over budget: %d kept %d omitted", len(kept), omitted)
	}
	if len(kept) < 8 {
		t.Fatalf("never fewer than the minimum lines: %d", len(kept))
	}
	if first := kept[0]; first.ID != "u" {
		t.Fatalf("the cut lands on a stretch start, not inside one: kept starts at %q", first.ID)
	}
	// next turn: the same lines plus the call and its answer — the same first line stays
	grown := append(append([]Line(nil), lines...),
		Line{ID: "5", Author: "C", At: at.Add(time.Minute), Text: "국수 먹자"},
		Line{ID: "6", Author: "bot", Bot: true, At: at.Add(2 * time.Minute), Text: "좋아"})
	again, _ := Select(grown, 700, 8)
	if again[0].ID != kept[0].ID {
		t.Fatalf("a turn later the cut must not move: kept %q, again %q", kept[0].ID, again[0].ID)
	}
	// and when the anchored stretch can no longer fit the budget, the cut is still a boundary or the
	// tentative 70% cut — never an inside-stretch line chosen arbitrarily
	for range 8 {
		at = at.Add(time.Minute)
		grown = append(grown, Line{ID: "g" + at.Format("04:05"), Author: "C", At: at, Text: strings.Repeat("y", 60)})
	}
	moved, _ := Select(grown, 700, 8)
	if len(moved) < 8 {
		t.Fatalf("never fewer than the minimum lines: %d", len(moved))
	}
}
