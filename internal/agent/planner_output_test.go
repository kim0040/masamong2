package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"masamong/internal/catalog"
	"masamong/internal/memory/retrieval"
)

func resolveCatalog(name string) (string, bool) {
	aliases := map[string]string{"local_rag": "memory.search", "get_weather_forecast": "weather.query"}
	if id, ok := aliases[name]; ok {
		name = id
	}
	_, ok := catalog.Lookup(name)
	return name, ok
}

func TestParsePlannerOutputSandboxShape(t *testing.T) {
	raw := "```json\n{\"memory_requirement\":\"required\",\"time_scope\":\"recent\",\"question_kind\":\"person_fact\",\"person_targets\":[\"철수\"],\"sensitive\":false,\"tool_plan\":[{\"tool_name\":\"weather.query\",\"parameters\":{\"region\":\"서울\"}}]}\n```"
	out, err := ParsePlannerOutput(raw, resolveCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if !out.NeedsRetrieval() || !out.HasTool("memory.search") {
		t.Fatal("required must add memory.search and retrieve")
	}
	it := out.Intent()
	if !it.Recent || it.Intro || len(it.PersonTargets) != 1 {
		t.Fatalf("intent %+v", it)
	}
	// auto v2 decides after observing candidates, not from the intent alone: the same intent
	// finishes on R0 when the pool is confident and goes to R2 when it is not.
	if d := retrieval.DefaultAutoPolicy.Decide(retrieval.ConfidenceHigh, true, 1, false); d.Mode != retrieval.Hybrid {
		t.Fatalf("confident pool must not rerank: %+v", d)
	}
	if d := retrieval.DefaultAutoPolicy.Decide(retrieval.ConfidenceMedium, true, 1, false); d.Mode != retrieval.Rerank {
		t.Fatalf("uncertain pool must rerank: %+v", d)
	}
}

func TestParsePlannerOutputV2Fields(t *testing.T) {
	raw := `{"memory_requirement":"required","time_scope":"any","question_kind":"person_fact","person_targets":["현민"],
	  "semantic_query":"현민이 최근 서버 구성에 대해 말한 내용","key_terms":["서버","구성","서버"],
	  "time":{"kind":"last_week"},"tool_plan":[]}`
	out, err := ParsePlannerOutput(raw, resolveCatalog)
	if err != nil {
		t.Fatal(err)
	}
	if out.SemanticQuery == "" || len(out.KeyTerms) != 2 {
		t.Fatalf("v2 fields: %+v", out)
	}
	it := out.Intent()
	if it.Time.Kind != retrieval.TimeLastWeek || !it.Time.Windowed() || it.Recent {
		t.Fatalf("time constraint must flow into intent without flipping v1 Recent: %+v", it)
	}
	// v1 output (no time object) still yields a structured constraint
	v1, err := ParsePlannerOutput(`{"memory_requirement":"required","time_scope":"recent","tool_plan":[]}`, resolveCatalog)
	if err != nil || v1.Time == nil || v1.Time.Kind != retrieval.TimeRecent || !v1.Intent().Recent {
		t.Fatalf("v1 recent must derive time.kind=recent: %+v %v", v1, err)
	}
	// time.kind=recent without time_scope sets time_scope for v1 consumers
	v2, err := ParsePlannerOutput(`{"memory_requirement":"required","time":{"kind":"recent"},"tool_plan":[]}`, resolveCatalog)
	if err != nil || v2.TimeScope != "recent" || !v2.Intent().Recent {
		t.Fatalf("time.kind=recent must set time_scope: %+v %v", v2, err)
	}
	if _, err := ParsePlannerOutput(`{"memory_requirement":"required","time":{"kind":"sometime"},"tool_plan":[]}`, resolveCatalog); err == nil {
		t.Fatal("bad time.kind must be rejected")
	}
	plain, _ := ParsePlannerOutput(`{"memory_requirement":"none","tool_plan":[]}`, resolveCatalog)
	if plain.Intent().Time.Kind != retrieval.TimeAny || plain.Intent().Time.Windowed() {
		t.Fatalf("unconstrained: %+v", plain.Intent().Time)
	}
}

func TestParsePlannerOutputRejects(t *testing.T) {
	if _, err := ParsePlannerOutput("no json", nil); err == nil {
		t.Fatal("prose must be rejected")
	}
	if _, err := ParsePlannerOutput(`{"memory_requirement":"maybe","tool_plan":[]}`, nil); err == nil {
		t.Fatal("bad enum must be rejected")
	}
	if _, err := ParsePlannerOutput(`{"memory_requirement":"none","tool_plan":[{"tool_name":"nonexistent.tool"}]}`, resolveCatalog); err == nil {
		t.Fatal("unknown tool must be rejected before any cost")
	}
	out, err := ParsePlannerOutput(`{"memory_requirement":"none","tool_plan":[]}`, resolveCatalog)
	if err != nil || out.NeedsRetrieval() || out.TimeScope != "any" || out.QuestionKind != "other" {
		t.Fatalf("chit-chat: %+v %v", out, err)
	}
}

func TestPlannerEnumsMatchContract(t *testing.T) {
	dir, _ := os.Getwd()
	var root string
	for i := 0; i < 8; i++ {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			root = dir
			break
		}
		dir = filepath.Dir(dir)
	}
	raw, err := os.ReadFile(filepath.Join(root, "contracts", "sandbox", "tool-parity.json"))
	if err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Planner struct {
			MemoryRequirement []string `json:"memory_requirement"`
			TimeScope         []string `json:"time_scope"`
			QuestionKind      []string `json:"question_kind"`
			TimeKind          []string `json:"time_kind"`
			Fields            []string `json:"fields"`
		} `json:"planner_output"`
	}
	if err := json.Unmarshal(raw, &spec); err != nil {
		t.Fatal(err)
	}
	eq := func(a, b []string) bool {
		if len(a) != len(b) {
			return false
		}
		for i := range a {
			if a[i] != b[i] {
				return false
			}
		}
		return true
	}
	if !eq(spec.Planner.TimeScope, TimeScopes) || !eq(spec.Planner.QuestionKind, QuestionKinds) {
		t.Fatalf("planner enums drift: %+v vs %v %v", spec.Planner, TimeScopes, QuestionKinds)
	}
	if !eq(spec.Planner.TimeKind, retrieval.TimeKinds) {
		t.Fatalf("time_kind drift: %v vs %v", spec.Planner.TimeKind, retrieval.TimeKinds)
	}
	for _, f := range []string{"semantic_query", "key_terms", "time"} {
		if !contains(spec.Planner.Fields, f) {
			t.Fatalf("contract planner_output.fields lacks v2 field %s", f)
		}
	}
	want := []string{string(retrieval.MemoryNone), string(retrieval.MemoryOptional), string(retrieval.MemoryRequired)}
	if !eq(spec.Planner.MemoryRequirement, want) {
		t.Fatalf("memory_requirement drift: %v vs %v", spec.Planner.MemoryRequirement, want)
	}
}
