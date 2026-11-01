package agent

import "testing"

func TestRejectCycleBeforeCost(t *testing.T) {
	_, err := Execute(Plan{Nodes: []Node{
		{ID: "a", DependsOn: []string{"b"}},
		{ID: "b", DependsOn: []string{"a"}},
	}}, "ok")
	if err == nil {
		t.Fatal("cycle must be rejected")
	}
}

func TestRejectEmptyPlan(t *testing.T) {
	if err := Validate(Plan{}); err == nil {
		t.Fatal("expected reject")
	}
}

func TestPartialQuoteWithoutFX(t *testing.T) {
	ex, err := Execute(Plan{Nodes: []Node{
		{ID: "q", Need: "quote"},
		{ID: "f", Need: "fx", DependsOn: []string{"q"}},
	}}, "fx_fail")
	if err != nil {
		t.Fatal(err)
	}
	if ex.Result != "partial" {
		t.Fatalf("result %s", ex.Result)
	}
	for _, n := range ex.Nodes {
		if n.Need == "fx" && n.Output != nil {
			t.Fatal("must not synthesize FX")
		}
	}
}

func TestHardBudgetConstants(t *testing.T) {
	if MaxPlannerRounds != 2 || MaxDeepRAGRounds != 2 {
		t.Fatalf("planner/deep RAG rounds must stay bounded: %d %d", MaxPlannerRounds, MaxDeepRAGRounds)
	}
	if MaxProactiveSends < 1 {
		t.Fatal("MaxProactiveSends")
	}
	if MaxGraphNodes < 1 || MaxExternalTries < 1 {
		t.Fatal("graph/external limits")
	}
}
