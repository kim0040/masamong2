package peoplebackfill

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
	"time"
)

var t0 = time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)

func msg(id, ref, text string, min int) Line {
	return Line{ID: id, Ref: ref, At: t0.Add(time.Duration(min) * time.Minute), Text: text}
}

// talk writes n alternating-speaker human lines, every one over the meaningful-rune floor.
func talk(room string, n int, startMin int) []Line {
	var out []Line
	for i := 0; i < n; i++ {
		ref := "111111111111111111"
		if i%2 == 1 {
			ref = "222222222222222222"
		}
		out = append(out, Line{ID: room + "-m" + itoa(startMin+i), Ref: ref,
			At: t0.Add(time.Duration(startMin+i) * time.Minute), Text: "a line of real talk"})
	}
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [8]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func TestBuildWindowsMergesRunsAndJoinsSegments(t *testing.T) {
	// one segment: a's burst, a bot line, b's two lines; a hard gap, then another segment
	lines := []Line{
		msg("m1", "111111111111111111", "first part here", 0),
		msg("m2", "111111111111111111", "second part here", 0),
		{ID: "m3", Bot: true, At: t0.Add(time.Minute), Text: "the bot answered at length"},
		msg("m4", "222222222222222222", "an answer worth keeping", 2),
		msg("m5", "222222222222222222", "and one more answer", 2),
		msg("m6", "111111111111111111", "after the silence", 60),
	}
	ws := BuildWindows("room-1", lines, Options{PromptHash: "v1"})
	if len(ws) != 1 {
		t.Fatalf("two segments under the limit join one window: %d", len(ws))
	}
	w := ws[0]
	if len(w.Lines) != 4 {
		t.Fatalf("a burst is one line, bot lines their own run: %v", w.Lines)
	}
	if w.Lines[0].Text != "first part here\nsecond part here" || w.Lines[1].Text != "the bot answered at length" ||
		w.Lines[2].Text != "an answer worth keeping\nand one more answer" || w.Lines[3].Text != "after the silence" {
		t.Fatalf("merged texts: %v", w.Lines)
	}
	if w.Lines[0].Ref != "111111111111111111" || !w.Lines[1].Bot || w.Lines[1].Ref != "" {
		t.Fatalf("speakers: %+v", w.Lines[0:2])
	}
	if w.FirstID != "m1" || w.LastID != "m6" || !w.First.Equal(t0) || !w.Last.Equal(t0.Add(60*time.Minute)) {
		t.Fatalf("window bounds: %+v", w)
	}
	if w.Skipped {
		t.Fatal("a talkative window is not skipped")
	}
	sum := sha256.Sum256([]byte("room-1\x00m1\x00m6\x00v1"))
	if w.ID != hex.EncodeToString(sum[:]) {
		t.Fatalf("window id %q", w.ID)
	}
}

func TestBuildWindowsSplitsAtTheLineLimit(t *testing.T) {
	// three segments of 50 merged lines each (a hard gap separates them; the segmenter's own
	// line cap is higher, so each stays one segment)
	var lines []Line
	for s := 0; s < 3; s++ {
		lines = append(lines, talk("r", 50, s*24*60)...)
	}
	ws := BuildWindows("r", lines, Options{MaxLines: 120})
	if len(ws) != 2 {
		t.Fatalf("three 50-line segments pack as 100 then 50: %d windows", len(ws))
	}
	for _, w := range ws {
		if len(w.Lines) > 120 {
			t.Fatalf("window over the limit: %d", len(w.Lines))
		}
	}
	if got := len(ws[0].Lines) + len(ws[1].Lines); got != 150 {
		t.Fatalf("every merged line lands in one window: %d", got)
	}
	if len(ws[0].Lines) != 100 || len(ws[1].Lines) != 50 || !ws[0].Last.Before(ws[1].First) {
		t.Fatalf("windows keep time order: %+v", ws)
	}
	// a hard cap just under two segments gives one segment per window
	ws = BuildWindows("r", lines, Options{MaxLines: 60})
	if len(ws) != 3 || len(ws[0].Lines) != 50 || len(ws[1].Lines) != 50 || len(ws[2].Lines) != 50 {
		t.Fatalf("whole segments only: %d windows of %d", len(ws), len(ws[0].Lines))
	}
}

func TestBuildWindowsSkipsThinWindows(t *testing.T) {
	// nothing but laughter and short acknowledgements: the merged text stays under the floor
	ws := BuildWindows("r", []Line{
		msg("m1", "111111111111111111", "ㅋㅋㅋ", 0),
		msg("m2", "222222222222222222", "ok", 1),
	}, Options{})
	if len(ws) != 1 || !ws[0].Skipped {
		t.Fatalf("no line of six meaningful runes is skipped: %+v", ws)
	}
	// bot lines are context: a long bot answer does not rescue a human-less window
	ws = BuildWindows("r", []Line{
		msg("m1", "111111111111111111", "네", 0),
		{ID: "m2", Bot: true, At: t0.Add(time.Minute), Text: "a long helpful bot answer about things"},
	}, Options{})
	if len(ws) != 1 || !ws[0].Skipped {
		t.Fatalf("the bot's own lines never count for the floor: %+v", ws)
	}
	// a merged run of short human lines can reach the floor together and is not skipped
	ws = BuildWindows("r", []Line{
		msg("m1", "111111111111111111", "so", 0),
		msg("m2", "111111111111111111", "fine", 0),
		msg("m3", "222222222222222222", "ㄱㄱ", 1),
	}, Options{})
	if len(ws) != 1 || ws[0].Skipped {
		t.Fatalf("one merged line over the floor keeps the window: %+v", ws)
	}
}

func TestWindowIDsAreDeterministicAndDistinct(t *testing.T) {
	lines := talk("r", 40, 0)
	a := BuildWindows("r", lines, Options{PromptHash: "v1"})
	b := BuildWindows("r", lines, Options{PromptHash: "v1"})
	if len(a) != len(b) || a[0].ID != b[0].ID {
		t.Fatal("same input, same ids")
	}
	c := BuildWindows("r", lines, Options{PromptHash: "v2"})
	if c[0].ID == a[0].ID {
		t.Fatal("a prompt version change re-keys the windows")
	}
	d := BuildWindows("r2", lines, Options{PromptHash: "v1"})
	if d[0].ID == a[0].ID {
		t.Fatal("the room is part of the id")
	}
	if len(a[0].ID) != 64 {
		t.Fatalf("hex sha256: %q", a[0].ID)
	}
	// the same lines read as another room's history never share a window id
	ids := map[string]bool{}
	for _, w := range append(a, d...) {
		if ids[w.ID] {
			t.Fatalf("window id shared across rooms: %s", w.ID)
		}
		ids[w.ID] = true
	}
}

// TestWindowUsesChronologicalEndpoints: segments can interleave (a topic resumed after another one
// started); the window's lines, first and last observation (its id) and last time follow time order,
// not the order of the segments.
func TestWindowUsesChronologicalEndpoints(t *testing.T) {
	lines := []Line{
		// m1, m2, m4 are one topic, m3 another (the segmenter reads words, so the lines are Korean)
		msg("m1", "111111111111111111", "등산 산길 등산화 정상", 0),
		msg("m2", "222222222222222222", "등산 산길 등산화 정상 좋아", 1),
		msg("m3", "333333333333333333", "피자 치즈 오븐 반죽", 2),
		msg("m4", "111111111111111111", "등산 산길 등산화 다시", 3),
	}
	ws := BuildWindows("r", lines, Options{PromptHash: "v1"})
	if len(ws) != 1 {
		t.Fatalf("one window: %d", len(ws))
	}
	w := ws[0]
	if w.FirstID != "m1" || w.LastID != "m4" || !w.Last.Equal(t0.Add(3*time.Minute)) {
		t.Fatalf("endpoints: first %s last %s at %v", w.FirstID, w.LastID, w.Last)
	}
	var order []string
	for _, l := range w.Lines {
		order = append(order, l.IDs...)
	}
	if len(w.Lines) != 4 || strings.Join(order, ",") != "m1,m2,m3,m4" {
		t.Fatalf("lines in time order: %v", w.Lines)
	}
	sum := sha256.Sum256([]byte("r\x00m1\x00m4\x00v1"))
	if w.ID != hex.EncodeToString(sum[:]) {
		t.Fatalf("window id %q", w.ID)
	}
}
