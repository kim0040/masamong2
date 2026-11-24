package peoplebackfill

import (
	"math"
	"testing"
)

// A Haiku prompt over 100k tokens is billed at the higher tier, so the reservation covers it; Sonnet and a
// prompt at the threshold keep the base price.
func TestSettleHaikuLongPromptAtHigherTier(t *testing.T) {
	for _, c := range []struct {
		model Model
		mode  Mode
		u     Usage
		want  float64
	}{
		{ModelHaiku, ModeStandard, Usage{InputTokens: 100_000, OutputTokens: 1000}, (100_000*0.10 + 1000*0.50) / 1e6},
		{ModelHaiku, ModeStandard, Usage{InputTokens: 101_000, OutputTokens: 5120}, (101_000*0.50 + 5120*2.50) / 1e6},
		{ModelHaiku, ModeStandard, Usage{InputTokens: 60_000, CacheReadTokens: 50_000}, (60_000*0.50 + 50_000*0.50*0.1) / 1e6},
		{ModelHaiku, ModeBatch, Usage{InputTokens: 101_000, OutputTokens: 5120}, (101_000*0.25 + 5120*1.25) / 1e6},
		// the reviewers' case: a 90k prompt reserved with the cache-write surcharge (112,500 weighted) stays
		// at the base tier; a raw prompt over 100k does not
		{ModelHaiku, ModeStandard, Usage{InputTokens: 112_500, OutputTokens: 5120, PromptTokens: 90_000}, (112_500*0.10 + 5120*0.50) / 1e6},
		{ModelHaiku, ModeBatch, Usage{InputTokens: 112_500, OutputTokens: 5120, PromptTokens: 90_000}, (112_500*0.05 + 5120*0.25) / 1e6},
		{ModelHaiku, ModeStandard, Usage{InputTokens: 126_250, OutputTokens: 5120, PromptTokens: 101_000}, (126_250*0.50 + 5120*2.50) / 1e6},
		{ModelSonnet, ModeStandard, Usage{InputTokens: 101_000, OutputTokens: 5120}, (101_000*2.0 + 5120*10.0) / 1e6},
	} {
		got, err := Settle(c.u, c.model, c.mode)
		if err != nil || math.Abs(got-c.want) > 1e-9 {
			t.Errorf("%s %s %+v: %v (%v), want %v", c.model, c.mode, c.u, got, err, c.want)
		}
	}
}
