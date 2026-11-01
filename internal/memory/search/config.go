// Package search is the product memory retriever: scope → independent candidate sources (leaf and
// summary lexical/dense, speaker, alias, multi-query) → weighted RRF with provenance → auto v2
// (assess → R0 / R2 rerank / one recovery) → relevance floor → adjacent-leaf merge → parent
// expansion into budgeted raw evidence.
//
// It is a port of the test sandbox recipe validated on 2026-09-23 (experimental/rag-leaf-parent
// engine.retrieve, leaf_parent mode) and is checked against sandbox output on real indexes
// (parity_test.go). Deliberately not ported: R1 heuristic ranking (ablation only; auto never picks
// it), soft events (off), the regex intent fallback (default is a neutral intent), baseline windows.
package search

import (
	"encoding/json"
	"math"

	"masamong/internal/lang"
)

// MinScores is `rerank_min_score`: a number, or {model: floor, "_default": floor}.
type MinScores map[string]float64

func (m *MinScores) UnmarshalJSON(raw []byte) error {
	var n float64
	if err := json.Unmarshal(raw, &n); err == nil {
		*m = MinScores{"_default": n}
		return nil
	}
	var obj map[string]float64
	if err := json.Unmarshal(raw, &obj); err != nil {
		return err
	}
	*m = obj
	return nil
}

// PersonConfig is `retrieval.person`.
type PersonConfig struct {
	Enabled              *bool   `json:"enabled,omitempty"`
	Limit                int     `json:"limit,omitempty"`
	RRFWeight            float64 `json:"rrf_weight,omitempty"`
	MaxSpeakersPerTarget int     `json:"max_speakers_per_target,omitempty"`
	Identity             string  `json:"identity,omitempty"` // observed | config | both
	ResolveInScope       *bool   `json:"resolve_in_scope,omitempty"`
}

// AliasConfig is `retrieval.alias`.
type AliasConfig struct {
	Enabled   *bool   `json:"enabled,omitempty"`
	Limit     int     `json:"limit,omitempty"`
	RRFWeight float64 `json:"rrf_weight,omitempty"`
}

// MultiQueryConfig is `retrieval.multi_query`.
type MultiQueryConfig struct {
	Enabled           *bool   `json:"enabled,omitempty"`
	MaxQueries        int     `json:"max_queries,omitempty"`
	SemanticRRFWeight float64 `json:"semantic_rrf_weight,omitempty"`
	KeyTermsRRFWeight float64 `json:"key_terms_rrf_weight,omitempty"`
}

// PoolConfig is `retrieval.candidate_pool`.
type PoolConfig struct {
	Default        int     `json:"default,omitempty"`
	Min            int     `json:"min,omitempty"`
	Max            int     `json:"max,omitempty"`
	PerSourceMin   *int    `json:"per_source_min,omitempty"`
	ReserveMaxFrac float64 `json:"reserve_max_frac,omitempty"`
}

// AutoConfig is `retrieval.auto` (thresholds shared with internal/memory/retrieval).
type AutoConfig struct {
	Enabled           *bool   `json:"enabled,omitempty"`
	AssessTopN        int     `json:"assess_top_n,omitempty"`
	HighAgreement     float64 `json:"high_agreement,omitempty"`
	HighMargin        float64 `json:"high_margin,omitempty"`
	LowAgreement      float64 `json:"low_agreement,omitempty"`
	MinCandidates     int     `json:"min_candidates,omitempty"`
	MinTargetCoverage float64 `json:"min_target_coverage,omitempty"`
}

// RecoveryConfig is `retrieval.recovery`.
type RecoveryConfig struct {
	Enabled         *bool   `json:"enabled,omitempty"`
	MaxAttempts     int     `json:"max_attempts,omitempty"`
	PoolScale       float64 `json:"pool_scale,omitempty"`
	LimitScale      float64 `json:"limit_scale,omitempty"`
	TimeWindowScale float64 `json:"time_window_scale,omitempty"`
}

// RerankConfig is `retrieval.rerank`.
type RerankConfig struct {
	Enabled   *bool  `json:"enabled,omitempty"`
	DocFormat string `json:"doc_format,omitempty"`
}

// MessageDenseConfig is `retrieval.message_dense`: a candidate source that scores single messages
// (memory.message_vectors) and maps them to their leaves. It is off unless Enabled is set, and it only
// runs when the loaded index holds message vectors (Index.SetMessageVectors).
type MessageDenseConfig struct {
	Enabled   *bool   `json:"enabled,omitempty"`
	Limit     int     `json:"limit,omitempty"`      // leaves in the run (default 200)
	RRFWeight float64 `json:"rrf_weight,omitempty"` // weight of the run in the final fusion (default 0.1)
}

// RetrievalConfig is the `retrieval` block.
type RetrievalConfig struct {
	Person    PersonConfig `json:"person"`
	Alias     AliasConfig  `json:"alias"`
	Summaries struct {
		Enabled *bool `json:"enabled,omitempty"`
	} `json:"summaries"`
	MultiQuery MultiQueryConfig `json:"multi_query"`
	Pool       PoolConfig       `json:"candidate_pool"`
	Auto       AutoConfig       `json:"auto"`
	Recovery   RecoveryConfig   `json:"recovery"`
	Rerank     RerankConfig     `json:"rerank"`
	// MessageDense is absent from the JSON while unset, so a config without it reads and writes as before.
	MessageDense MessageDenseConfig `json:"message_dense,omitzero"`
}

// Config holds every knob the retriever reads. JSON names follow the sandbox config.json so a sandbox
// recipe can be loaded verbatim; zero values fall back to the sandbox defaults (see the accessors).
type Config struct {
	// Language is the (merged) language rules for this corpus: name particles, pronouns, stop words,
	// tokenizer scripts, evidence markers. nil = the default language pack (lang.Default).
	Language *lang.Search `json:"-"`

	TopK                   int                 `json:"top_k"`
	RetrievePool           int                 `json:"retrieve_pool"`
	LexicalLimit           int                 `json:"lexical_limit"`
	DenseLimit             int                 `json:"dense_limit"`
	RRFWeights             []float64           `json:"rrf_weights"`
	SummaryEnabled         bool                `json:"summary_enabled"`
	SummaryRRFWeight       *float64            `json:"summary_rrf_weight"`
	ParentExpandBefore     *int                `json:"parent_expand_before"`
	ParentExpandAfter      *int                `json:"parent_expand_after"`
	ContextPrefix          *bool               `json:"context_prefix"`
	DocumentCharLimit      int                 `json:"document_char_limit"`
	EvidenceTotalCharLimit int                 `json:"evidence_total_char_limit"`
	RerankMinScore         MinScores           `json:"rerank_min_score"`
	DedupMode              string              `json:"dedup_mode"`
	LeafSize               int                 `json:"leaf_size"`
	RecentWindowDays       float64             `json:"recent_window_days"`
	RecentWindowLeaves     int                 `json:"recent_window_leaves"`
	RecencyAnchor          string              `json:"recency_anchor"`
	AliasExpandLexical     *bool               `json:"alias_expand_lexical"`
	HybridMinCos           float64             `json:"hybrid_min_cos"`
	PersonAliases          map[string][]string `json:"person_aliases"`
	Retrieval              RetrievalConfig     `json:"retrieval"`
}

// Default is the recipe the sandbox evaluated (config.example.json + retrieval_config.DEFAULTS).
func Default() Config {
	return Config{
		TopK: 5, RetrievePool: 100, LexicalLimit: 200, DenseLimit: 150, RRFWeights: []float64{1, 4},
		SummaryEnabled: true, DocumentCharLimit: 4000, DedupMode: "merge", LeafSize: 8,
		RecentWindowDays: 30, RecentWindowLeaves: 200, RecencyAnchor: "corpus_max",
		RerankMinScore: MinScores{"_default": 0.5},
	}
}

func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

func intOr(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func floatOr(v, def float64) float64 {
	if v == 0 {
		return def
	}
	return v
}

func (c Config) topK() int { return intOr(c.TopK, 5) }

func (c Config) weights() [2]float64 {
	if len(c.RRFWeights) >= 2 {
		return [2]float64{c.RRFWeights[0], c.RRFWeights[1]}
	}
	return [2]float64{1, 4}
}

func (c Config) summaryWeight() float64 {
	if c.SummaryRRFWeight == nil {
		return 1.0
	}
	return *c.SummaryRRFWeight
}

func (c Config) charLimit() int { return intOr(c.DocumentCharLimit, 4000) }

func (c Config) parentBefore() int {
	if c.ParentExpandBefore == nil {
		return 12
	}
	return *c.ParentExpandBefore
}

func (c Config) parentAfter() int {
	if c.ParentExpandAfter == nil {
		return 12
	}
	return *c.ParentExpandAfter
}

// searchLimits is retrieval_post.search_limits.
func (c Config) searchLimits() (int, int) {
	return max(1, intOr(c.LexicalLimit, 200)), max(1, intOr(c.DenseLimit, 150))
}

// poolSize is retrieval_config.pool_size: legacy retrieve_pool wins, recovery scales it.
func (c Config) poolSize(topK int, recovery bool) int {
	p := c.Retrieval.Pool
	base := c.RetrievePool
	if base == 0 {
		base = intOr(p.Default, max(40, topK*8))
	}
	lo, hi := intOr(p.Min, 40), intOr(p.Max, 160)
	if recovery {
		base = int(math.RoundToEven(float64(base) * floatOr(c.Retrieval.Recovery.PoolScale, 1.5)))
	}
	return max(lo, min(hi, base))
}

func (c Config) perSourceMin() int {
	if c.Retrieval.Pool.PerSourceMin == nil {
		return 5
	}
	return *c.Retrieval.Pool.PerSourceMin
}

func (c Config) reserveMaxFrac() float64 { return floatOr(c.Retrieval.Pool.ReserveMaxFrac, 0.2) }

// rerankMinScore is retrieval_post.resolve_rerank_min_score.
func (c Config) rerankMinScore(model string) float64 {
	if v, ok := c.RerankMinScore[model]; ok && model != "" {
		return v
	}
	if v, ok := c.RerankMinScore["_default"]; ok {
		return v
	}
	return 0.5
}

// DefaultMessageDenseWeight is the weight of the message run in the final fusion (the base ranking has
// weight 1). Measured on real data, 1.0 pushed flow (span) questions down by 3-5 questions per set
// without a reranker; 0.1 kept them within ±1 and kept the gains after reranking.
const DefaultMessageDenseWeight = 0.1

func (c Config) messageDenseOn() bool { return boolOr(c.Retrieval.MessageDense.Enabled, false) }

func (c Config) messageDenseLimit() int { return max(1, intOr(c.Retrieval.MessageDense.Limit, 200)) }

func (c Config) messageDenseWeight() float64 {
	return floatOr(c.Retrieval.MessageDense.RRFWeight, DefaultMessageDenseWeight)
}

func (c Config) personIdentity() string {
	switch c.Retrieval.Person.Identity {
	case "config", "both":
		return c.Retrieval.Person.Identity
	}
	return "observed"
}

func (c Config) dedupMode() string {
	if c.DedupMode == "" {
		return "merge"
	}
	return c.DedupMode
}

func (c Config) leafSize() int { return intOr(c.LeafSize, 8) }

func (c Config) docFormat() string {
	if c.Retrieval.Rerank.DocFormat == "" {
		return "summary_body"
	}
	return c.Retrieval.Rerank.DocFormat
}
