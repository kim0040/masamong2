package config

import (
	"slices"
	"testing"

	"masamong/internal/providers/openai"
)

// Sonnet 20: the levels a config may set are the levels the adapter writes, in the same order (least
// thinking first): the two lists are kept apart only so config validation does not reach into a provider.
func TestModelEffortsMatchTheAdapter(t *testing.T) {
	if !slices.Equal(ModelEfforts, openai.EffortLevels) {
		t.Fatalf("config.ModelEfforts %v, openai.EffortLevels %v", ModelEfforts, openai.EffortLevels)
	}
}
