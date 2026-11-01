package discord

import (
	"fmt"
	"strings"
	"testing"
)

// Discord folds a tall message: a text that has to be split anyway is cut after softLines lines, but
// never inside a code block, and a text that fits is left whole.
func TestSplitMessageSoftLineCap(t *testing.T) {
	const limit = 2000
	lines := func(n, width int) string {
		var b []string
		for i := 0; i < n; i++ {
			b = append(b, fmt.Sprintf("%02d %s", i, strings.Repeat("a", width)))
		}
		return strings.Join(b, "\n")
	}
	lineCount := func(s string) int { return strings.Count(s, "\n") + 1 }

	// 60 lines of 43 characters: 2.6k, over the limit; the first part ends at 17 lines, not near 2000
	parts := splitMessage(lines(60, 40), limit)
	checkParts(t, "tall", parts, limit)
	if len(parts) < 2 {
		t.Fatalf("parts = %d", len(parts))
	}
	for i, p := range parts[:len(parts)-1] {
		if n := lineCount(p); n > softLines {
			t.Errorf("part %d has %d lines", i, n)
		}
	}
	if lineCount(parts[0]) != softLines {
		t.Errorf("first part has %d lines, want %d", lineCount(parts[0]), softLines)
	}
	if got, want := strings.Fields(strings.Join(parts, "\n")), strings.Fields(lines(60, 40)); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Error("the split changed the text")
	}

	// a tall text that fits is one message
	if p := splitMessage(lines(40, 10), limit); len(p) != 1 {
		t.Errorf("a text under the limit was split into %d", len(p))
	}

	// the 17th line falls inside a code block: no soft cut there, so the line cap never opens a block of
	// its own; the 2000 limit still cuts it (closing and reopening the fence) as before
	code := lines(5, 70) + "\n```go\n" + lines(25, 70) + "\n```\n" + lines(10, 70)
	cparts := splitMessage(code, limit)
	checkParts(t, "code", cparts, limit)
	if len(cparts) < 2 {
		t.Fatalf("parts = %d", len(cparts))
	}
	if first := cparts[0]; !strings.Contains(first, "```go") || lineCount(first) <= softLines {
		t.Errorf("the line cap cut inside the code block (%d lines):\n%s", lineCount(first), first)
	}

	// a cut that would leave a sliver is not taken
	short := strings.Repeat("x\n", 30) + strings.Repeat("word ", 500)
	sp := splitMessage(short, limit)
	checkParts(t, "short lines", sp, limit)
	if len(sp[0]) < limit/4 {
		t.Errorf("sliver first part: %q", sp[0])
	}
}
