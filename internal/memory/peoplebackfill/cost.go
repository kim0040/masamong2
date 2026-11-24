package peoplebackfill

import "fmt"

// Model is a model the backfill may send windows to (the contract names these two only).
type Model string

const (
	ModelHaiku  Model = "claude-haiku-5-5"
	ModelSonnet Model = "claude-sonnet-5-5"
)

// Mode is how a window is sent: one live request, or one item of a Message Batches job at half
// price.
type Mode string

const (
	ModeStandard Mode = "standard"
	ModeBatch    Mode = "batch"
)

// USD per 1M tokens, Anthropic list prices of 2026-11. Prompt-cache reads are billed at a tenth
// of the input price; a batch item at half of both.
var prices = map[Model]struct{ in, out float64 }{
	ModelHaiku:  {0.10, 0.50},
	ModelSonnet: {2.00, 10.00},
}

// Haiku 5.5 bills a whole request whose prompt is over 100k tokens at 5x ($0.50/$2.50). A window is far
// below that on live data, but its merged lines are not bounded in total, so the reservation and the
// settlement price such a prompt at the higher tier.
const (
	haikuLongPrompt = 100_000
	haikuLongFactor = 5.0
)

const (
	cacheReadFactor = 0.1 // input read from the prompt cache, relative to the input price
	batchFactor     = 0.5 // Message Batches
)

// The per-window token estimates the contract's budget arithmetic uses: the fixed person_extract
// prompt and the people list go on every window, every merged line costs about a vector line, and
// the answer is bounded by its expected length.
const (
	promptTokens = 1100 // the fixed prompt
	peopleTokens = 100  // the people list
	lineTokens   = 28   // per merged line
	outTokens    = 550  // the answer per window
	highMargin   = 1.2  // the high end adds a 20 percent request margin
)

func priceOf(model Model, mode Mode) (in, out float64, err error) {
	p, ok := prices[model]
	if !ok {
		return 0, 0, fmt.Errorf("peoplebackfill: unknown model %q", model)
	}
	switch mode {
	case ModeStandard:
		return p.in, p.out, nil
	case ModeBatch:
		return p.in * batchFactor, p.out * batchFactor, nil
	}
	return 0, 0, fmt.Errorf("peoplebackfill: unknown mode %q", mode)
}

// EstimateTokens is the token estimate of sending windows (skipped ones are never sent): the
// fixed prompt plus the people list per window, and lineTokens per merged line.
func EstimateTokens(windows []Window) (in, out int) {
	for _, w := range windows {
		if w.Skipped {
			continue
		}
		in += promptTokens + peopleTokens + lineTokens*len(w.Lines)
		out += outTokens
	}
	return in, out
}

// EstimateCost is the low/high USD range of sending windows on model in mode: low prices the
// token estimate exactly, high adds the 20 percent request margin.
func EstimateCost(windows []Window, model Model, mode Mode) (low, high float64, err error) {
	in, out, err := priceOf(model, mode)
	if err != nil {
		return 0, 0, err
	}
	tin, tout := EstimateTokens(windows)
	low = (float64(tin)*in + float64(tout)*out) / 1e6
	return low, low * highMargin, nil
}

// Usage is the real billed usage of one finished window, for settling against the estimate.
type Usage struct {
	InputTokens     int
	OutputTokens    int
	CacheReadTokens int // input read from the prompt cache: a tenth of the input price
}

// Settle is the real USD cost of u on model in mode.
func Settle(u Usage, model Model, mode Mode) (float64, error) {
	in, out, err := priceOf(model, mode)
	if err != nil {
		return 0, err
	}
	if model == ModelHaiku && u.InputTokens+u.CacheReadTokens > haikuLongPrompt {
		in, out = in*haikuLongFactor, out*haikuLongFactor // a long prompt is billed at the higher tier
	}
	return (float64(u.InputTokens)*in + float64(u.CacheReadTokens)*in*cacheReadFactor + float64(u.OutputTokens)*out) / 1e6, nil
}

// CanSubmit reports whether the next batch may go out under the run's USD cap: it may while
// spent + the in-flight maximum + the next batch's maximum is not over the cap.
func CanSubmit(spent, inflightMax, nextMax, cap float64) bool {
	return spent+inflightMax+nextMax <= cap
}
