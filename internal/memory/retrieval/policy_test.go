package retrieval

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func f(v float64) *float64 { return &v }

func TestDecideCascade(t *testing.T) {
	p := DefaultAutoPolicy
	cases := []struct {
		name     string
		level    Confidence
		rerank   bool
		budget   float64
		recovery bool
		want     Decision
	}{
		{"high → R0, no rerank call", ConfidenceHigh, true, 1, false, Decision{Mode: Hybrid, Reason: ReasonR0Confident}},
		{"high without reranker → still R0", ConfidenceHigh, false, 0, false, Decision{Mode: Hybrid, Reason: ReasonR0Confident}},
		{"medium → R2", ConfidenceMedium, true, 1, false, Decision{Mode: Rerank, Reason: ReasonRerankUncertain}},
		{"medium, no key → R0 (never R1)", ConfidenceMedium, false, 1, false, Decision{Mode: Hybrid, Reason: ReasonNoRerank}},
		{"medium, budget floor → R0", ConfidenceMedium, true, 0.01, false, Decision{Mode: Hybrid, Reason: ReasonNoRerank}},
		{"low → recover first", ConfidenceLow, true, 1, false, Decision{Recover: true, Reason: "recovery_low_confidence"}},
		{"low after recovery → R2", ConfidenceLow, true, 1, true, Decision{Mode: Rerank, Reason: ReasonRecoveryRerank}},
		{"low after recovery, no key → R0", ConfidenceLow, false, 1, true, Decision{Mode: Hybrid, Reason: ReasonRecoveryNoRerank}},
	}
	for _, c := range cases {
		got := p.Decide(c.level, c.rerank, c.budget, c.recovery)
		if got != c.want {
			t.Fatalf("%s: got %+v want %+v", c.name, got, c.want)
		}
	}
	noRecovery := p
	noRecovery.RecoveryMaxAttempts = 0
	if got := noRecovery.Decide(ConfidenceLow, true, 1, false); got.Recover || got.Mode != Rerank || got.Reason != ReasonRerankLowConfidence {
		t.Fatalf("recovery disabled: %+v", got)
	}
	if m, why := p.Fallback(); m != Hybrid || why != ReasonRerankFailed {
		t.Fatalf("rerank failure must land on R0: %s/%s", m, why)
	}
	for _, m := range []RankingMode{p.OnHigh, p.OnMedium, p.OnLow, p.NoRerank, p.RerankFailed} {
		if m == Heuristic {
			t.Fatal("auto must never route to R1")
		}
	}
}

func TestAssessRule(t *testing.T) {
	th := DefaultThresholds
	if l, why := Assess(Signals{CandidateCount: 2, Agreement: 1, TopMargin: 1}, th); l != ConfidenceLow || why != "few_candidates" {
		t.Fatalf("few: %s/%s", l, why)
	}
	if l, _ := Assess(Signals{CandidateCount: 40, Agreement: 0.8, TopMargin: 0.3, TargetsRequested: true, TargetsResolved: true, TargetCoverage: f(0.1)}, th); l != ConfidenceLow {
		t.Fatalf("target missing must be low: %s", l)
	}
	if l, why := Assess(Signals{CandidateCount: 40, Agreement: 0.8, TopMargin: 0.3, TargetsRequested: true}, th); l != ConfidenceMedium || why != "target_unresolved" {
		t.Fatalf("unresolved person caps at medium: %s/%s", l, why)
	}
	if l, _ := Assess(Signals{CandidateCount: 40, Agreement: 0.7, TopMargin: 0.2}, th); l != ConfidenceHigh {
		t.Fatalf("agreement+margin: %s", l)
	}
}

func TestNormalizeLegacyAliases(t *testing.T) {
	for raw, want := range map[string]RankingMode{"cohere": Rerank, "r2": Rerank, "rerank": Rerank, "auto": Auto, "R0": ""} {
		got, ok := Normalize(raw)
		if want == "" {
			if ok {
				t.Fatalf("%s must be unknown (case-sensitive contract)", raw)
			}
			continue
		}
		if !ok || got != want {
			t.Fatalf("%s → %s ok=%v want %s", raw, got, ok, want)
		}
	}
}

func TestNeedsRetrievalAndState(t *testing.T) {
	if NeedsRetrieval(MemoryNone, true) {
		t.Fatal("none must never retrieve, even with a stray memory.search step")
	}
	if !NeedsRetrieval(MemoryRequired, false) {
		t.Fatal("required must retrieve")
	}
	if NeedsRetrieval(MemoryOptional, false) || !NeedsRetrieval(MemoryOptional, true) {
		t.Fatal("optional follows the plan")
	}
	if StateFor(false, 0, false) != StateNotAttempted || StateFor(true, 3, false) != StateCandidates ||
		StateFor(true, 0, true) != StateInsufficientScored || StateFor(true, 0, false) != StateEmptyInScope {
		t.Fatal("state mapping")
	}
}

type contractCase struct {
	Name    string `json:"name"`
	Signals struct {
		CandidateCount   int      `json:"candidate_count"`
		Agreement        float64  `json:"agreement"`
		TopMargin        float64  `json:"top_margin"`
		TargetCoverage   *float64 `json:"target_coverage"`
		TargetsRequested bool     `json:"targets_requested"`
		TargetsResolved  bool     `json:"targets_resolved"`
	} `json:"signals"`
	Level string `json:"level"`
}

// The contract file is the single source; Go constants must not drift from it.
func TestParityWithSandboxContract(t *testing.T) {
	root := findRepoRoot(t)
	raw, err := os.ReadFile(filepath.Join(root, "contracts", "sandbox", "tool-parity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Ranking struct {
			R0         string `json:"R0"`
			R1         string `json:"R1"`
			R2         string `json:"R2"`
			Auto       string `json:"auto"`
			AutoPolicy struct {
				Kind         string   `json:"kind"`
				OnHigh       string   `json:"on_high"`
				OnMedium     string   `json:"on_medium"`
				OnLow        string   `json:"on_low"`
				NoRerank     string   `json:"no_rerank"`
				RerankFailed string   `json:"rerank_failed"`
				MinBudget    float64  `json:"min_remaining_budget_usd"`
				Recovery     int      `json:"recovery_max_attempts"`
				Reasons      []string `json:"reasons"`
				Thresholds   struct {
					AssessTopN        int     `json:"assess_top_n"`
					HighAgreement     float64 `json:"high_agreement"`
					HighMargin        float64 `json:"high_margin"`
					LowAgreement      float64 `json:"low_agreement"`
					MinCandidates     int     `json:"min_candidates"`
					MinTargetCoverage float64 `json:"min_target_coverage"`
				} `json:"thresholds"`
				Cases []contractCase `json:"cases"`
			} `json:"auto_policy"`
			LegacyAliases     map[string]string `json:"legacy_aliases"`
			RetrievalState    []string          `json:"retrieval_state"`
			MemoryRequirement []string          `json:"memory_requirement"`
		} `json:"ranking"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	r := spec.Ranking
	if r.R0 != string(Hybrid) || r.R1 != string(Heuristic) || r.R2 != string(Rerank) || r.Auto != string(Auto) {
		t.Fatalf("ranking names drift: %+v", r)
	}
	if r.AutoPolicy.Kind != "cascade_v2" {
		t.Fatalf("auto_policy.kind = %q, want cascade_v2", r.AutoPolicy.Kind)
	}
	p := DefaultAutoPolicy
	if r.AutoPolicy.OnHigh != string(p.OnHigh) || r.AutoPolicy.OnMedium != string(p.OnMedium) || r.AutoPolicy.OnLow != string(p.OnLow) ||
		r.AutoPolicy.NoRerank != string(p.NoRerank) || r.AutoPolicy.RerankFailed != string(p.RerankFailed) ||
		r.AutoPolicy.MinBudget != p.MinRemainingBudgetUSD || r.AutoPolicy.Recovery != p.RecoveryMaxAttempts {
		t.Fatalf("auto_policy drift: contract=%+v go=%+v", r.AutoPolicy, p)
	}
	for _, v := range []string{r.AutoPolicy.OnHigh, r.AutoPolicy.OnMedium, r.AutoPolicy.OnLow, r.AutoPolicy.NoRerank, r.AutoPolicy.RerankFailed} {
		if v == string(Heuristic) {
			t.Fatalf("contract auto_policy must not route to R1: %+v", r.AutoPolicy)
		}
	}
	th := r.AutoPolicy.Thresholds
	if th.AssessTopN != DefaultThresholds.AssessTopN || th.HighAgreement != DefaultThresholds.HighAgreement || th.HighMargin != DefaultThresholds.HighMargin ||
		th.LowAgreement != DefaultThresholds.LowAgreement || th.MinCandidates != DefaultThresholds.MinCandidates || th.MinTargetCoverage != DefaultThresholds.MinTargetCoverage {
		t.Fatalf("thresholds drift: contract=%+v go=%+v", th, DefaultThresholds)
	}
	wantReasons := map[string]bool{ReasonR0Confident: true, ReasonRerankUncertain: true, ReasonRerankLowConfidence: true,
		ReasonRecoveryRerank: true, ReasonRecoveryNoRerank: true, ReasonNoRerank: true, ReasonRerankFailed: true}
	if len(r.AutoPolicy.Reasons) != len(wantReasons) {
		t.Fatalf("reasons count drift: %v", r.AutoPolicy.Reasons)
	}
	for _, s := range r.AutoPolicy.Reasons {
		if !wantReasons[s] {
			t.Fatalf("unknown reason in contract: %s", s)
		}
	}
	if len(r.AutoPolicy.Cases) == 0 {
		t.Fatal("contract must carry assessment cases")
	}
	for _, c := range r.AutoPolicy.Cases {
		sig := Signals{CandidateCount: c.Signals.CandidateCount, Agreement: c.Signals.Agreement, TopMargin: c.Signals.TopMargin,
			TargetCoverage: c.Signals.TargetCoverage, TargetsRequested: c.Signals.TargetsRequested, TargetsResolved: c.Signals.TargetsResolved}
		if got, _ := Assess(sig, DefaultThresholds); string(got) != c.Level {
			t.Fatalf("case %q: go=%s contract=%s", c.Name, got, c.Level)
		}
	}
	for raw, target := range r.LegacyAliases {
		if got, ok := LegacyAliases[raw]; !ok || string(got) != target {
			t.Fatalf("legacy alias %s→%s missing/drifted in Go (%s)", raw, target, got)
		}
	}
	wantStates := []State{StateNotAttempted, StateCandidates, StateEmptyInScope, StateInsufficientScored}
	if len(r.RetrievalState) != len(wantStates) {
		t.Fatalf("retrieval_state drift: %v", r.RetrievalState)
	}
	for i, s := range wantStates {
		if r.RetrievalState[i] != string(s) {
			t.Fatalf("retrieval_state[%d]=%s want %s", i, r.RetrievalState[i], s)
		}
	}
	wantReq := []MemoryRequirement{MemoryNone, MemoryOptional, MemoryRequired}
	for i, s := range wantReq {
		if r.MemoryRequirement[i] != string(s) {
			t.Fatalf("memory_requirement[%d]=%s want %s", i, r.MemoryRequirement[i], s)
		}
	}
}

func findRepoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	t.Fatal("go.mod not found")
	return ""
}
