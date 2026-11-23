package peoplebackfill

import (
	"math"
	"testing"
)

func TestEstimateCost(t *testing.T) {
	ws := roomWindows("r", 3, 40) // 3 windows of 40 merged lines
	if len(ws) != 3 {
		t.Fatal("fixture")
	}
	in, out := EstimateTokens(ws)
	if in != 3*(1100+100+28*40) || out != 3*550 {
		t.Fatalf("tokens: %d %d", in, out)
	}
	low, high, err := EstimateCost(ws, ModelHaiku, ModeStandard)
	if err != nil {
		t.Fatal(err)
	}
	want := (float64(in)*0.10 + float64(out)*0.50) / 1e6
	if math.Abs(low-want) > 1e-12 || math.Abs(high-want*1.2) > 1e-12 {
		t.Fatalf("haiku standard: %v %v want %v", low, high, want)
	}
	low2, _, err := EstimateCost(ws, ModelSonnet, ModeStandard)
	if err != nil {
		t.Fatal(err)
	}
	want2 := (float64(in)*2 + float64(out)*10) / 1e6
	if math.Abs(low2-want2) > 1e-12 {
		t.Fatalf("sonnet: %v want %v", low2, want2)
	}
	low3, _, err := EstimateCost(ws, ModelHaiku, ModeBatch)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(low3-want/2) > 1e-12 {
		t.Fatalf("batch is half: %v want %v", low3, want/2)
	}
	// skipped windows are never sent and cost nothing
	ws[0].Skipped = true
	in2, _ := EstimateTokens(ws)
	if in2 != in-(1100+100+28*40) {
		t.Fatalf("skipped windows are not counted: %d", in2)
	}
	if _, _, err := EstimateCost(ws, "claude-nope", ModeStandard); err == nil {
		t.Fatal("an unknown model is an error")
	}
	if _, _, err := EstimateCost(ws, ModelHaiku, "stream"); err == nil {
		t.Fatal("an unknown mode is an error")
	}
}

func TestSettle(t *testing.T) {
	u := Usage{InputTokens: 5000, OutputTokens: 550, CacheReadTokens: 2000}
	got, err := Settle(u, ModelHaiku, ModeStandard)
	if err != nil {
		t.Fatal(err)
	}
	want := (5000*0.10 + 2000*0.10*0.1 + 550*0.50) / 1e6
	if math.Abs(got-want) > 1e-12 {
		t.Fatalf("settle: %v want %v", got, want)
	}
	b, err := Settle(u, ModelHaiku, ModeBatch)
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(b-want/2) > 1e-12 {
		t.Fatalf("batch settles at half: %v", b)
	}
	if _, err := Settle(u, "nope", ModeStandard); err == nil {
		t.Fatal("unknown model")
	}
}

func TestCanSubmit(t *testing.T) {
	for _, c := range []struct {
		spent, inflight, next, cap float64
		want                       bool
	}{
		{0, 0, 3.0, 3.0, true},   // exactly at the cap is not over it
		{0, 0, 3.01, 3.0, false}, // over
		{2.9, 0, 0.05, 3.0, true},
		{2.9, 0.2, 0, 3.0, false}, // in-flight maximum counts
		{2.9, 0, 0.2, 3.0, false}, // the next batch's maximum counts
		{4.0, 0, 0, 3.0, false},   // already over
		{0, 0, 0, 0, true},        // nothing pending fits a zero cap
		{0, 0, 0.001, 0, false},   // but no paid request does
	} {
		if got := CanSubmit(c.spent, c.inflight, c.next, c.cap); got != c.want {
			t.Errorf("CanSubmit(%v, %v, %v, %v) = %v want %v", c.spent, c.inflight, c.next, c.cap, got, c.want)
		}
	}
}
