// Package retrieval holds the retrieval contracts that the product shares with the
// test sandbox (contracts/sandbox/tool-parity.json → ranking): ranking modes, the `auto`
// v2 cascade (assess → finish / rerank / recovery), retrieval states, and the planner's memory requirement.
//
// It deliberately contains no lexical/vector/rerank implementation. Those live in memory/search.
// This package owns the decision policy so the sandbox and daemon cannot drift on which ranking
// runs for a query.
package retrieval

// RankingMode is one retrieval ranking variant. Later IDs do not include earlier ones:
// R2 is hybrid pool + semantic rerank only, not R0+R1.
type RankingMode string

const (
	Hybrid    RankingMode = "hybrid"    // R0 · 어휘+벡터 RRF 후 절단
	Heuristic RankingMode = "heuristic" // R1 · R0 풀 + 최근성·인물 규칙 가산. ablation 전용 — auto는 절대 고르지 않는다
	Rerank    RankingMode = "rerank"    // R2 · R0 풀 + 의미 재순위만 (규칙 없음). 모델은 설정으로 교체
	Auto      RankingMode = "auto"      // 질의마다 R0 또는 R2 (R1 제외). 합치지 않는다
)

// ConcreteModes are the rankings that can actually run (R1 only when asked for explicitly).
var ConcreteModes = []RankingMode{Hybrid, Heuristic, Rerank}

// LegacyAliases maps retired names (vendor names) to current modes.
var LegacyAliases = map[string]RankingMode{"cohere": Rerank, "semantic": Rerank, "r0": Hybrid, "r1": Heuristic, "r2": Rerank}

// Normalize accepts current names and legacy aliases; ok=false for unknown input.
func Normalize(raw string) (RankingMode, bool) {
	switch RankingMode(raw) {
	case Hybrid, Heuristic, Rerank, Auto:
		return RankingMode(raw), true
	}
	if m, ok := LegacyAliases[raw]; ok {
		return m, true
	}
	return "", false
}

// MemoryRequirement is the planner's verdict on whether channel memory is needed at all.
type MemoryRequirement string

const (
	MemoryNone     MemoryRequirement = "none"     // 잡담·도구만 → 과거 검색 0회
	MemoryOptional MemoryRequirement = "optional" // 플래너가 memory.search를 넣었을 때만
	MemoryRequired MemoryRequirement = "required" // 기억 없이는 답할 수 없음
)

// State is the retrieval outcome reported to the synthesizer.
type State string

const (
	StateNotAttempted       State = "not_attempted"
	StateCandidates         State = "candidates"
	StateEmptyInScope       State = "empty_in_allowed_scope"
	StateInsufficientScored State = "insufficient_authorized_candidates" // rerank floor cut everything
)

// Intent is the planner-derived retrieval intent. Keyword regexes are not the source of
// truth here; the planner LLM emits time_scope / question_kind / person_targets.
// Recent and PersonTargets are *scope/candidate-source* inputs (recency window, speaker
// retriever), never ranking bonuses — that was R1, retired from auto on 2026-09-17.
type Intent struct {
	Recent        bool           // time_scope == "recent" (kept for v1 planners)
	Intro         bool           // question_kind == "person_intro"
	PersonTargets []string       // names the question is about (cleaned; no pronouns)
	Time          TimeConstraint // v2: structured window; Kind == "any" when unconstrained
	SemanticQuery string         // v2: second dense query (optional)
	KeyTerms      []string       // v2: second lexical query (optional)
}

// TimeConstraint is the planner's structured time scope (contract planner_output.time).
// It restricts the candidate *universe*; it is never a recency bonus.
type TimeConstraint struct {
	Kind string `json:"kind"`           // one of TimeKinds
	From string `json:"from,omitempty"` // ISO-8601, before/after/between only
	To   string `json:"to,omitempty"`
}

const (
	TimeAny       = "any"
	TimeRecent    = "recent"
	TimeToday     = "today"
	TimeYesterday = "yesterday"
	TimeLastWeek  = "last_week"
	TimeThisMonth = "this_month"
	TimeBefore    = "before"
	TimeAfter     = "after"
	TimeBetween   = "between"
)

// TimeKinds mirrors contracts/sandbox/tool-parity.json planner_output.time_kind (order matters).
var TimeKinds = []string{TimeAny, TimeRecent, TimeToday, TimeYesterday, TimeLastWeek, TimeThisMonth, TimeBefore, TimeAfter, TimeBetween}

func ValidTimeKind(k string) bool {
	for _, x := range TimeKinds {
		if x == k {
			return true
		}
	}
	return false
}

// Windowed reports whether the constraint narrows the universe at all.
func (t TimeConstraint) Windowed() bool { return t.Kind != "" && t.Kind != TimeAny }

// Confidence is the rule-based assessment of the R0 v2 candidate pool (auto v2, cascade).
type Confidence string

const (
	ConfidenceHigh   Confidence = "high"
	ConfidenceMedium Confidence = "medium"
	ConfidenceLow    Confidence = "low"
)

// Signals are what the sandbox computes over the fused top-N candidates
// (candidates_assess.signals_for). The daemon will compute the same once memory.search lands.
type Signals struct {
	CandidateCount   int
	Agreement        float64  // share of top-N surfaced by ≥2 retriever families
	TopMargin        float64  // (score1-score2)/score1 of fused RRF scores
	TargetCoverage   *float64 // nil when nobody was named or nobody resolved
	TargetsRequested bool
	TargetsResolved  bool
}

// Thresholds mirror contracts/sandbox/tool-parity.json ranking.auto_policy.thresholds.
type Thresholds struct {
	AssessTopN        int
	HighAgreement     float64
	HighMargin        float64
	LowAgreement      float64
	MinCandidates     int
	MinTargetCoverage float64
}

var DefaultThresholds = Thresholds{AssessTopN: 10, HighAgreement: 0.6, HighMargin: 0.15, LowAgreement: 0.2, MinCandidates: 3, MinTargetCoverage: 0.3}

// Assess is the pure level rule; identical to candidates_assess.level_from_signals and checked
// against the contract's `cases` by both parity tests.
func Assess(sig Signals, th Thresholds) (Confidence, string) {
	if sig.CandidateCount < th.MinCandidates {
		return ConfidenceLow, "few_candidates"
	}
	if sig.TargetsRequested && sig.TargetsResolved && sig.TargetCoverage != nil && *sig.TargetCoverage < th.MinTargetCoverage {
		return ConfidenceLow, "target_missing"
	}
	if sig.Agreement < th.LowAgreement {
		return ConfidenceLow, "low_agreement"
	}
	if sig.TargetsRequested && !sig.TargetsResolved {
		return ConfidenceMedium, "target_unresolved"
	}
	if sig.Agreement >= th.HighAgreement && sig.TopMargin >= th.HighMargin {
		return ConfidenceHigh, "agreement_and_margin"
	}
	return ConfidenceMedium, "uncertain"
}

// AutoPolicy is the `auto` v2 cascade. Values mirror contracts/sandbox/tool-parity.json
// ranking.auto_policy; the parity test fails if they drift.
//
//	R0 v2 candidates → Assess → high: OnHigh (R0) · medium: OnMedium (R2) ·
//	low: recovery once (wider pool/limits/window) → OnLow (R2). No reranker → NoRerank (R0).
type AutoPolicy struct {
	OnHigh                RankingMode
	OnMedium              RankingMode
	OnLow                 RankingMode
	NoRerank              RankingMode // reranker unavailable (no key / below budget floor)
	RerankFailed          RankingMode // reranker selected but the call failed
	MinRemainingBudgetUSD float64
	RecoveryMaxAttempts   int
}

// DefaultAutoPolicy (2026-09-17): R2 stays the quality path (gold v3 R2 R@1 84.7% vs R0 52.9%),
// but only when the observed pool is not already confident. Without a reranker (no key / no
// budget / call failed) auto lands on R0 — never R1 (−25pp R@1 vs R0 on gold v3, 07 log); the
// person/recent signals R1 used are applied as scope and candidate sources inside R0 instead.
var DefaultAutoPolicy = AutoPolicy{
	OnHigh:                Hybrid,
	OnMedium:              Rerank,
	OnLow:                 Rerank,
	NoRerank:              Hybrid,
	RerankFailed:          Hybrid,
	MinRemainingBudgetUSD: 0.02,
	RecoveryMaxAttempts:   1,
}

// Reason strings are shared with the sandbox trace field `ranking_auto_reason`.
const (
	ReasonR0Confident         = "r0_confident"
	ReasonRerankUncertain     = "rerank_uncertain"
	ReasonRerankLowConfidence = "rerank_low_confidence"
	ReasonRecoveryRerank      = "recovery_rerank"
	ReasonRecoveryNoRerank    = "recovery_no_rerank"
	ReasonNoRerank            = "no_rerank"
	ReasonRerankFailed        = "rerank_failed"
)

// Decision is one step of the cascade. Recover=true means: run the recovery retrieval, then
// call Decide again with recoveryDone=true.
type Decision struct {
	Mode    RankingMode
	Reason  string
	Recover bool
}

// Decide resolves what to do with an assessed candidate pool.
func (p AutoPolicy) Decide(level Confidence, rerankAvailable bool, remainingUSD float64, recoveryDone bool) Decision {
	canRerank := rerankAvailable && remainingUSD >= p.MinRemainingBudgetUSD
	switch level {
	case ConfidenceHigh:
		return Decision{Mode: p.OnHigh, Reason: ReasonR0Confident}
	case ConfidenceMedium:
		if !canRerank {
			return Decision{Mode: p.NoRerank, Reason: ReasonNoRerank}
		}
		return Decision{Mode: p.OnMedium, Reason: ReasonRerankUncertain}
	}
	// low
	if !recoveryDone && p.RecoveryMaxAttempts > 0 {
		return Decision{Recover: true, Reason: "recovery_low_confidence"}
	}
	if !canRerank {
		if recoveryDone {
			return Decision{Mode: p.NoRerank, Reason: ReasonRecoveryNoRerank}
		}
		return Decision{Mode: p.NoRerank, Reason: ReasonNoRerank}
	}
	if recoveryDone {
		return Decision{Mode: p.OnLow, Reason: ReasonRecoveryRerank}
	}
	return Decision{Mode: p.OnLow, Reason: ReasonRerankLowConfidence}
}

// Fallback is where `auto` lands when the R2 call itself fails after being selected.
func (p AutoPolicy) Fallback() (RankingMode, string) {
	return p.RerankFailed, ReasonRerankFailed
}

// NeedsRetrieval mirrors the sandbox gate: none → never; required → always;
// optional → only if the plan explicitly asked for memory.search.
func NeedsRetrieval(req MemoryRequirement, planHasMemorySearch bool) bool {
	switch req {
	case MemoryNone:
		return false
	case MemoryRequired:
		return true
	case MemoryOptional:
		return planHasMemorySearch
	}
	return planHasMemorySearch
}

// StateFor derives the reported state from what retrieval produced.
func StateFor(attempted bool, hits int, abstained bool) State {
	switch {
	case !attempted:
		return StateNotAttempted
	case hits > 0:
		return StateCandidates
	case abstained:
		return StateInsufficientScored
	}
	return StateEmptyInScope
}
