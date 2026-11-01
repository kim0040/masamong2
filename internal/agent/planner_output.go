package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"masamong/internal/errcode"
	"masamong/internal/memory/retrieval"
)

// PlannerOutput is the structured verdict a planner model emits for one user message.
// Shape is shared with the test sandbox (contracts/sandbox/tool-parity.json planner_output);
// the sandbox's bot_plan.parse_plan and this parser must accept the same JSON so a plan that
// works in the sandbox behaves identically on the real bot.
type PlannerOutput struct {
	MemoryRequirement retrieval.MemoryRequirement `json:"memory_requirement"`
	TimeScope         string                      `json:"time_scope"`
	QuestionKind      string                      `json:"question_kind"`
	PersonTargets     []string                    `json:"person_targets"`
	Sensitive         bool                        `json:"sensitive"`
	ToolPlan          []PlanStep                  `json:"tool_plan"`

	// v2 (optional; v1 planners omit them). The planner structures *meaning* only — it never
	// chooses R0/R2, pool sizes or weights. Retrieval decides after observing candidates.
	SemanticQuery string                    `json:"semantic_query,omitempty"`
	KeyTerms      []string                  `json:"key_terms,omitempty"`
	Time          *retrieval.TimeConstraint `json:"time,omitempty"`
}

type PlanStep struct {
	ToolName   string         `json:"tool_name"`
	Parameters map[string]any `json:"parameters"`
}

var (
	TimeScopes    = []string{"recent", "any"}
	QuestionKinds = []string{"person_intro", "person_fact", "event", "casual", "other"}
)

const (
	MaxSemanticQueryChars = 300
	MaxKeyTerms           = 12
)

// ParsePlannerOutput tolerates fenced JSON and leading prose (models do that), validates
// enums, and canonicalizes tool names through the resolver (aliases → catalog IDs).
// Unknown tool IDs are rejected here so no cost is reserved for them (08: reject before spend).
func ParsePlannerOutput(raw string, resolve func(string) (string, bool)) (PlannerOutput, error) {
	text := strings.TrimSpace(raw)
	if i := strings.Index(text, "```"); i >= 0 {
		parts := strings.Split(text, "```")
		if len(parts) >= 2 {
			text = strings.TrimPrefix(strings.TrimSpace(parts[1]), "json")
		}
	}
	start, end := strings.Index(text, "{"), strings.LastIndex(text, "}")
	if start < 0 || end < start {
		return PlannerOutput{}, fmt.Errorf("%s: planner output has no JSON object", errcode.PlanRejected)
	}
	var out PlannerOutput
	if err := json.Unmarshal([]byte(text[start:end+1]), &out); err != nil {
		return PlannerOutput{}, fmt.Errorf("%s: %v", errcode.PlanRejected, err)
	}
	switch out.MemoryRequirement {
	case retrieval.MemoryNone, retrieval.MemoryOptional, retrieval.MemoryRequired:
	default:
		return PlannerOutput{}, fmt.Errorf("%s: bad memory_requirement %q", errcode.PlanRejected, out.MemoryRequirement)
	}
	if out.TimeScope == "" {
		out.TimeScope = "any"
	}
	if !contains(TimeScopes, out.TimeScope) {
		return PlannerOutput{}, fmt.Errorf("%s: bad time_scope %q", errcode.PlanRejected, out.TimeScope)
	}
	if out.QuestionKind == "" {
		out.QuestionKind = "other"
	}
	if !contains(QuestionKinds, out.QuestionKind) {
		return PlannerOutput{}, fmt.Errorf("%s: bad question_kind %q", errcode.PlanRejected, out.QuestionKind)
	}
	// v2 time constraint: validate kind, then keep time_scope and time consistent both ways.
	if out.Time != nil {
		if out.Time.Kind == "" {
			out.Time.Kind = retrieval.TimeAny
		}
		if !retrieval.ValidTimeKind(out.Time.Kind) {
			return PlannerOutput{}, fmt.Errorf("%s: bad time.kind %q", errcode.PlanRejected, out.Time.Kind)
		}
		if out.Time.Kind == retrieval.TimeRecent {
			out.TimeScope = "recent"
		}
	} else if out.TimeScope == "recent" {
		out.Time = &retrieval.TimeConstraint{Kind: retrieval.TimeRecent}
	}
	out.SemanticQuery = strings.TrimSpace(out.SemanticQuery)
	if len([]rune(out.SemanticQuery)) > MaxSemanticQueryChars {
		out.SemanticQuery = string([]rune(out.SemanticQuery)[:MaxSemanticQueryChars])
	}
	terms := out.KeyTerms[:0:0]
	for _, t := range out.KeyTerms {
		t = strings.TrimSpace(t)
		if t != "" && !contains(terms, t) && len(terms) < MaxKeyTerms {
			terms = append(terms, t)
		}
	}
	out.KeyTerms = terms
	if len(out.ToolPlan) > MaxGraphNodes {
		return PlannerOutput{}, fmt.Errorf("%s: too many tool steps", errcode.PlanRejected)
	}
	for i := range out.ToolPlan {
		name := strings.TrimSpace(out.ToolPlan[i].ToolName)
		if resolve != nil {
			id, ok := resolve(name)
			if !ok {
				return PlannerOutput{}, fmt.Errorf("%s: unknown tool %q", errcode.PlanRejected, name)
			}
			name = id
		}
		out.ToolPlan[i].ToolName = name
		if out.ToolPlan[i].Parameters == nil {
			out.ToolPlan[i].Parameters = map[string]any{}
		}
	}
	if out.MemoryRequirement == retrieval.MemoryRequired && !out.HasTool("memory.search") {
		out.ToolPlan = append(out.ToolPlan, PlanStep{ToolName: "memory.search", Parameters: map[string]any{}})
	}
	return out, nil
}

func (p PlannerOutput) HasTool(id string) bool {
	for _, s := range p.ToolPlan {
		if s.ToolName == id {
			return true
		}
	}
	return false
}

// Intent converts the planner verdict into the retrieval intent the ranking policy consumes.
func (p PlannerOutput) Intent() retrieval.Intent {
	it := retrieval.Intent{
		Recent:        p.TimeScope == "recent",
		Intro:         p.QuestionKind == "person_intro",
		PersonTargets: append([]string(nil), p.PersonTargets...),
		SemanticQuery: p.SemanticQuery,
		KeyTerms:      append([]string(nil), p.KeyTerms...),
	}
	if p.Time != nil {
		it.Time = *p.Time
	} else if p.TimeScope == "recent" {
		it.Time = retrieval.TimeConstraint{Kind: retrieval.TimeRecent}
	} else {
		it.Time = retrieval.TimeConstraint{Kind: retrieval.TimeAny}
	}
	return it
}

// NeedsRetrieval mirrors the sandbox gate.
func (p PlannerOutput) NeedsRetrieval() bool {
	return retrieval.NeedsRetrieval(p.MemoryRequirement, p.HasTool("memory.search"))
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
