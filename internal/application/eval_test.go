package application_test

import (
	"testing"

	"masamong/internal/config"
)

func TestEvalDryRunResolvesAttachedChannelScopeAfterClosingReviewRows(t *testing.T) {
	svc := setupSvc(t)
	doc := svc.Doc()
	doc.Guilds = map[string]config.GuildSettings{
		"guild": {
			Channels: map[string]config.ChannelPolicy{
				"room": {RAGView: config.MemoryView{Blocks: []string{"missing-block"}}},
			},
		},
	}
	svc.SetDocForTest(doc)
	out(t, svc, "lab.review", map[string]any{
		"question":    "who brings the tent?",
		"answer":      "Doyun",
		"verdict":     "good",
		"channel_key": "guild:room",
		"meta":        map[string]any{"doc_ids": []any{"leaf-1"}},
	})

	plan := out(t, svc, "eval.run", map[string]any{
		"dry_run": true,
		"modes":   []any{"hybrid"},
	})
	if plan["cases"].(float64) != 1 {
		t.Fatalf("eval plan: %v", plan)
	}
}
