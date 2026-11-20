package openai

import "testing"

// A Claude model bills Anthropic's five-minute cache write (1.25x input) on every route, a router
// included; a Gemini model on OpenRouter writes at the plain input price.
func TestCacheWriteFactorFollowsTheModelFamily(t *testing.T) {
	router := &Adapter{BaseURL: "https://openrouter.ai/api/v1", API: APIOpenRouter}
	for model, want := range map[string]float64{
		"anthropic/claude-haiku-5-5": 1.25,
		"google/gemini-4-hydrogen":   1,
	} {
		if got := router.cacheWriteFactor(model); got != want {
			t.Fatalf("%s on OpenRouter: write factor %v, want %v", model, got, want)
		}
	}
	native := &Adapter{BaseURL: "https://api.anthropic.com/v1", API: APIAnthropic}
	if got := native.cacheWriteFactor("claude-haiku-5-5"); got != 1.25 {
		t.Fatalf("native Anthropic: write factor %v, want 1.25", got)
	}
}
