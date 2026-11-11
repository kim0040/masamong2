package chat

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"masamong/internal/errclass"
)

// The "still looking" stage is told for the searches the turn's limits let in. A deep search the cap
// refuses starts nothing and says nothing; beside an ordinary search that was let in, only the ordinary
// stage is told. Both the tool loop and the planner's steps.
func TestProgressOnlyForAdmittedSearches(t *testing.T) {
	spentGuard := func() *Guard {
		g := &Guard{MaxUSD: 0.03, MaxCalls: 10}
		g.Spend(0.03) // the turn has spent its share
		return g
	}
	loopDo := func(g *Guard, ts Toolset, run ToolRunner, calls ...LoopCall) ([]Progress, []ToolResult, *LoopTrace) {
		t.Helper()
		log := &progressLog{}
		l := ToolLoop{LLM: &loopLLM{replies: []LoopReply{{Calls: calls}, {Text: "-"}}}, Run: run, Role: "planner", Voice: koVoice(), Guard: g, OnProgress: log.add}
		results, tr, err := l.Do(context.Background(), ts, nil, "q", "")
		if err != nil {
			t.Fatal(err)
		}
		return log.got, results, tr
	}
	got, _, tr := loopDo(spentGuard(), depthTools(), (&recorder{}).run, searchCall("a", `{"query":"x","depth":"deep"}`))
	if len(got) != 0 || tr.Steps[0].Reason != "limit" {
		t.Fatalf("refused deep search: progress %+v, step %+v", got, tr.Steps[0])
	}

	// an ordinary search holds more than the cap leaves; the deep one after it waits for its fee and is
	// then refused: only the ordinary stage was told
	synctest.Test(t, func(t *testing.T) {
		ts := depthTools()
		for i := range ts.Enabled {
			if ts.Enabled[i].ID == webSearchTool {
				ts.Enabled[i].FeeBound = func(map[string]any) float64 { return 0.04 }
			}
		}
		tt := newTimedTools(0.035)
		got, _, tr := loopDo(&Guard{MaxUSD: 0.03, MaxCalls: 10}, ts, tt.run, searchCall("a", `{"query":"1s"}`), searchCall("b", `{"query":"x","depth":"deep"}`))
		if want := []Progress{{ProgressSearch, 1, 2}}; !slices.Equal(got, want) || tr.Steps[0].State != "ran" || tr.Steps[1].Reason != "limit" {
			t.Fatalf("progress %+v, want %+v; steps %+v", got, want, tr.Steps)
		}
	})

	// the planner's steps
	log := &progressLog{}
	req := loopRequest("q", ToolModePlanner)
	req.Tools, req.OnProgress, req.Guard = depthTools(), log.add, spentGuard()
	planned := `{"memory_requirement":"none","tool_plan":[{"tool_name":"search.web","parameters":{"query":"x","depth":"deep"}}]}`
	res, err := (&Runtime{LLM: &loopLLM{plannerOut: planned}, Tools: (&recorder{}).run}).Answer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(log.got) != 0 || len(res.Tools) != 1 || res.Tools[0].Status != "unavailable" {
		t.Fatalf("planner: progress %+v, tools %+v", log.got, res.Tools)
	}
}

// fixg1RetryLLM plans with loopLLM and answers through ChatDetailed, failing with errs in order.
type fixg1RetryLLM struct {
	*loopLLM
	mu   sync.Mutex
	errs []error
	msgs [][]Msg
}

func (r *fixg1RetryLLM) ChatDetailed(_ context.Context, _ string, msgs []Msg, _ int, _ float64) (Completion, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, append([]Msg(nil), msgs...))
	var err error
	if len(r.errs) > 0 {
		err, r.errs = r.errs[0], r.errs[1:]
	}
	if err != nil {
		return Completion{}, err
	}
	return Completion{Text: "답", FinishReason: "stop"}, nil
}

// When the answer's prompt was too long, the retry also takes the tool results back to the room of a turn
// without page text (toolBlockMax), cutting the passages first: every result keeps its title.
func TestContextLengthRetryCutsTheToolBlock(t *testing.T) {
	llm := &fixg1RetryLLM{loopLLM: &loopLLM{plannerOut: `{"memory_requirement":"none","tool_plan":[{"tool_name":"search.web","parameters":{"query":"x","depth":"deep"}}]}`},
		errs: []error{classed{errclass.ContextLength}}}
	run := func(context.Context, string, map[string]any) ToolResult { return searchWithPassages(5, 3000) }
	req := loopRequest("q", ToolModePlanner)
	req.Tools = depthTools()
	out, err := (&Runtime{LLM: llm, Tools: run}).Answer(context.Background(), req)
	if err != nil || out.AnswerRetry != string(errclass.ContextLength) || len(llm.msgs) != 2 {
		t.Fatalf("answer %q retry %q calls %d err %v", out.Answer, out.AnswerRetry, len(llm.msgs), err)
	}
	pageText := func(msgs []Msg) int { return strings.Count(msgs[len(msgs)-1].Content, "가") }
	first, second := pageText(llm.msgs[0]), pageText(llm.msgs[1])
	if first <= toolBlockMax || second == 0 || second > toolBlockMax {
		t.Fatalf("page text in the prompt: first %d, retry %d (room %d)", first, second, toolBlockMax)
	}
	last := llm.msgs[1][len(llm.msgs[1])-1].Content
	for i := range 5 {
		if !strings.Contains(last, fmt.Sprintf("title %d", i)) {
			t.Fatalf("result %d lost its title on the retry", i)
		}
	}
}
