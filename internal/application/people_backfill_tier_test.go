package application

import (
	"math"
	"strings"
	"testing"

	"masamong/internal/memory/peoplebackfill"
)

// The reservation of a Haiku prompt that cannot reach 100k tokens stays at the base price tier, even with
// the cache-write surcharge added; a prompt that can is reserved at the higher tier.
func TestBackfillReservationPicksTierFromRawPrompt(t *testing.T) {
	for _, c := range []struct {
		raw  int
		mode peoplebackfill.Mode
		want float64
	}{
		{90_000, peoplebackfill.ModeStandard, (112_500*0.10 + float64(pbWireOut)*0.50) / 1e6},
		{90_000, peoplebackfill.ModeBatch, (112_500*0.05 + float64(pbWireOut)*0.25) / 1e6},
		{101_000, peoplebackfill.ModeStandard, (126_250*0.50 + float64(pbWireOut)*2.50) / 1e6},
	} {
		bp := pbPrompt{system: strings.Repeat("a", c.raw-pbFraming)}
		if got := pbMaxCost(bp, peoplebackfill.ModelHaiku, c.mode); math.Abs(got-c.want) > 1e-9 {
			t.Errorf("raw %d %s: reserved %v, want %v", c.raw, c.mode, got, c.want)
		}
	}
}
