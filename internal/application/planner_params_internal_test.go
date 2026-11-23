package application

import (
	"testing"

	"masamong/internal/chat"
	"masamong/internal/lang"
)

// A planner step for a standard web search keeps the page count it names: search reads `pages` (an
// explicit 0 keeps the search to snippets), so the tool declares it, and the planner's step validation,
// which keeps only declared parameters, does not strip it.
func TestPlannerStepKeepsTheSearchPageCount(t *testing.T) {
	t.Parallel()
	infos := toolsOf(t, fixg1Svc(t, opsOrder, nil, nil))
	ts := chat.Toolset{Known: map[string]bool{"search.web": true}, Enabled: []chat.ToolInfo{infos["search.web"]}}
	p, _ := lang.Get("en")
	plan := chat.ParsePlan(`{"memory_requirement":"none","tool_plan":[{"tool_name":"search.web","parameters":{"query":"news","pages":0,"provider":"exa"}}]}`)
	a := chat.Assess(chat.Voice{Pack: p}, plan, ts, false, true)
	if len(a.Steps) != 1 {
		t.Fatalf("steps %+v", a.Steps)
	}
	got := a.Steps[0].Parameters
	if pages, ok := got["pages"]; !ok || pages != float64(0) {
		t.Fatalf("pages dropped from the step: %+v (stripped %v)", got, a.Stripped)
	}
	if _, has := got["provider"]; has {
		t.Fatalf("the operator's provider choice reached the step: %+v", got)
	}
}
