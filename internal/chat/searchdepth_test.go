package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
	"unicode/utf8"

	"masamong/internal/lang"
)

// depthTools is loopTools with search.web marked as the application marks it: its calls run at once
// (Parallel) and each pays at most a known fee (MaxFeeUSD), and it takes depth and queries.
func depthTools() Toolset {
	ts := loopTools()
	for i := range ts.Enabled {
		if ts.Enabled[i].ID == webSearchTool {
			ts.Enabled[i].Parallel, ts.Enabled[i].Pays, ts.Enabled[i].MaxFeeUSD = true, true, 0.015
			ts.Enabled[i].Params = append(ts.Enabled[i].Params, ToolParam{Name: "depth", Enum: []string{"quick", "standard", "deep"}},
				ToolParam{Name: "queries", Type: "array", Max: ptr(3)})
		}
	}
	return ts
}

// timedTools runs a web search for the time its query names ("1s", "300ms"; one second otherwise) and
// charges fee for it as search.web does; other tools take a second and pay nothing. It notes when each
// call started and ended.
type timedTools struct {
	mu     sync.Mutex
	fee    float64
	starts map[string]time.Duration
	ends   map[string]time.Duration
	t0     time.Time
}

func newTimedTools(fee float64) *timedTools {
	return &timedTools{fee: fee, starts: map[string]time.Duration{}, ends: map[string]time.Duration{}, t0: time.Now()}
}

func (tt *timedTools) run(ctx context.Context, tool string, p map[string]any) ToolResult {
	name, _ := p["query"].(string)
	if name == "" {
		name, _ = p["region"].(string)
	}
	d, err := time.ParseDuration(name)
	if err != nil {
		d = time.Second
	}
	tt.mu.Lock()
	tt.starts[name] = time.Since(tt.t0)
	tt.mu.Unlock()
	time.Sleep(d)
	if tool == webSearchTool {
		g := GuardFrom(ctx)
		g.Spend(tt.fee)
		g.Paid(tt.fee)
	}
	tt.mu.Lock()
	tt.ends[name] = time.Since(tt.t0)
	tt.mu.Unlock()
	return ToolResult{Tool: tool, OK: true, Data: map[string]any{"status": "ok", "name": name}}
}

func searchCall(id, args string) LoopCall { return call(id, "search__web", args) }

// Three web searches of one round take the time of the slowest, not the sum, and their results keep the
// model's order although the first ends last. Calls of a tool that may not run beside itself still run one
// after another, in the model's order.
func TestToolLoopRunsARoundsSearchesAtOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{searchCall("a", `{"query":"1s"}`), searchCall("b", `{"query":"600ms"}`),
			searchCall("c", `{"query":"300ms"}`)}}, {Text: "-"}}}
		tt := newTimedTools(0.007)
		g := &Guard{MaxUSD: 0.05, MaxCalls: 10}
		l := ToolLoop{LLM: llm, Run: tt.run, Role: "planner", Voice: koVoice(), Guard: g}
		results, _, err := l.Do(context.Background(), depthTools(), nil, "q", "")
		if err != nil {
			t.Fatal(err)
		}
		if took := time.Since(tt.t0); took != time.Second {
			t.Fatalf("the round took %v, want the slowest search's 1s", took)
		}
		var names []string
		for _, r := range results {
			names = append(names, r.Data["name"].(string))
		}
		if !slices.Equal(names, []string{"1s", "600ms", "300ms"}) {
			t.Fatalf("results not in the model's order: %v", names)
		}
		if _, spent, _ := g.Totals(); math.Abs(spent-0.021) > 1e-12 || g.HeldTotal() != 0 {
			t.Fatalf("spent %v, still held %v", spent, g.HeldTotal())
		}
	})
	synctest.Test(t, func(t *testing.T) {
		llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{call("a", "weather__query", `{"region":"300ms"}`),
			call("b", "weather__query", `{"region":"200ms"}`)}}, {Text: "-"}}}
		tt := newTimedTools(0)
		l := ToolLoop{LLM: llm, Run: tt.run, Role: "planner", Voice: koVoice(), Guard: &Guard{MaxCalls: 10}}
		if _, _, err := l.Do(context.Background(), depthTools(), nil, "q", ""); err != nil {
			t.Fatal(err)
		}
		if tt.starts["200ms"] < tt.ends["300ms"] {
			t.Fatalf("the second weather call started at %v, before the first ended at %v", tt.starts["200ms"], tt.ends["300ms"])
		}
	})
}

// Under the default cap (0.03) the holds of two searches (0.015 each) leave no room for a third: it is not
// refused but waits until one has ended, and is then let in on what that one really paid, as it would have
// been one search after another.
func TestHeldSearchesWaitInsteadOfBeingRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{searchCall("a", `{"query":"a 1"}`), searchCall("b", `{"query":"b 1"}`),
			searchCall("c", `{"query":"c 1"}`)}}, {Text: "-"}}}
		tt := newTimedTools(0.007)
		g := &Guard{MaxUSD: 0.03, MaxCalls: 10}
		l := ToolLoop{LLM: llm, Run: tt.run, Role: "planner", Voice: koVoice(), Guard: g}
		results, tr, err := l.Do(context.Background(), depthTools(), nil, "q", "")
		if err != nil {
			t.Fatal(err)
		}
		for i, r := range results {
			if !r.OK || tr.Steps[i].State != "ran" {
				t.Fatalf("call %d: %+v %+v", i, r, tr.Steps[i])
			}
		}
		if tt.starts["a 1"] != 0 || tt.starts["b 1"] != 0 || tt.starts["c 1"] != time.Second {
			t.Fatalf("starts %v: want two at once and the third after one ended", tt.starts)
		}
		if g.HeldTotal() != 0 {
			t.Fatalf("still held %v", g.HeldTotal())
		}
	})
}

// The property the holds keep: with every fee at most its bound, the steps let in are exactly the ones let
// in when the same steps run one after another, whatever the timing, the mix of tools (bounded fee, unknown
// fee, free), the money cap and the call cap.
func TestHoldsAdmitWhatSequentialExecutionAdmits(t *testing.T) {
	rng := rand.New(rand.NewPCG(7, 11))
	const bound = 0.015
	type spec struct {
		info ToolInfo
		fee  float64
		dur  time.Duration
	}
	for trial := range 500 {
		n := 1 + rng.IntN(9)
		specs := make([]spec, n)
		for i := range specs {
			s := spec{info: ToolInfo{ID: fmt.Sprint("tool", rng.IntN(3)), Parallel: rng.IntN(2) == 0}, dur: time.Duration(1+rng.IntN(2000)) * time.Millisecond}
			switch rng.IntN(3) {
			case 0: // pays at most its bound
				s.info.Pays, s.info.MaxFeeUSD, s.fee = true, bound, float64(rng.IntN(16))*0.001
			case 1: // pays a fee nobody knows ahead
				s.info.Pays, s.fee = true, float64(rng.IntN(21))*0.001
			}
			specs[i] = s
		}
		capUSD, maxCalls := 0.0, 0
		if rng.IntN(5) > 0 {
			capUSD = (float64(rng.IntN(45)) + 0.5) * 0.001 // never a sum of fees: no tie for rounding to break
		}
		if rng.IntN(4) == 0 {
			maxCalls = 1 + rng.IntN(n)
		}
		var want []int
		spent, calls := 0.0, 0
		for i, s := range specs {
			if (capUSD > 0 && spent >= capUSD) || (maxCalls > 0 && calls >= maxCalls) {
				continue
			}
			want = append(want, i)
			calls++
			spent += s.fee
		}
		var got []int
		synctest.Test(t, func(t *testing.T) {
			g := &Guard{MaxUSD: capUSD, MaxCalls: maxCalls}
			ctx := context.Background()
			var mu sync.Mutex
			var steps []step
			for i, s := range specs {
				hold := &paidHold{}
				steps = append(steps, step{key: stepKey(s.info, s.info.ID), pays: s.info.Pays, maxFee: s.info.MaxFeeUSD, hold: hold, decide: func() bool {
					ok, end := admitTool(ctx, g, s.info.ID, s.info, nil)
					hold.end = end
					if ok {
						g.Note(0)
						mu.Lock()
						got = append(got, i)
						mu.Unlock()
					}
					return ok
				}, run: func() {
					time.Sleep(s.dur)
					g.Spend(s.fee)
					g.Paid(s.fee)
				}})
			}
			runSteps(ctx, g, steps)
			if g.HeldTotal() != 0 {
				t.Errorf("trial %d: %v still held", trial, g.HeldTotal())
			}
		})
		for _, i := range got {
			if !slices.Contains(want, i) {
				t.Fatalf("trial %d: step %d let in, which one after another refuses (cap %v, calls %d, got %v, want %v)", trial, i, capUSD, maxCalls, got, want)
			}
		}
		if !slices.Equal(got, want) {
			t.Fatalf("trial %d: let in %v, one after another lets in %v (cap %v, calls %d)", trial, got, want, capUSD, maxCalls)
		}
	}
}

// A hold is let go exactly once: a second release changes nothing, a call that panicked or was cancelled
// gives its hold back, and an admission cancelled while it waits for a hold holds nothing and is no refusal.
func TestHoldIsReleasedOnceOnEndCancelAndPanic(t *testing.T) {
	g := &Guard{MaxUSD: 1}
	okA, releaseA := g.AllowHoldingContext(context.Background(), "a", 0.015)
	okB, releaseB := g.AllowHoldingContext(context.Background(), "b", 0.012)
	if !okA || !okB || math.Abs(g.HeldTotal()-0.027) > 1e-12 {
		t.Fatalf("held %v", g.HeldTotal())
	}
	releaseA()
	releaseA()
	if math.Abs(g.HeldTotal()-0.012) > 1e-12 {
		t.Fatalf("after releasing a twice: held %v, want b's 0.012", g.HeldTotal())
	}
	releaseB()
	if g.HeldTotal() != 0 {
		t.Fatalf("held %v", g.HeldTotal())
	}

	// a search that panics
	llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{searchCall("a", `{"query":"x"}`), searchCall("b", `{"query":"y"}`)}}, {Text: "-"}}}
	panicky := func(_ context.Context, tool string, p map[string]any) ToolResult {
		if p["query"] == "x" {
			panic("boom")
		}
		return ToolResult{Tool: tool, OK: true}
	}
	g = &Guard{MaxUSD: 0.03}
	results, _, err := ToolLoop{LLM: llm, Run: panicky, Role: "planner", Voice: koVoice(), Guard: g}.Do(context.Background(), depthTools(), nil, "q", "")
	if err != nil || len(results) != 2 || results[0].OK || !results[1].OK || g.HeldTotal() != 0 {
		t.Fatalf("results %+v err %v held %v", results, err, g.HeldTotal())
	}

	// a round cancelled before it runs
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g = &Guard{MaxUSD: 0.03}
	cl := cancelLLM{&loopLLM{replies: []LoopReply{{Calls: []LoopCall{searchCall("a", `{"query":"x"}`), searchCall("b", `{"query":"y"}`)}}}}, cancel}
	rec := &recorder{}
	results, _, _ = ToolLoop{LLM: cl, Run: rec.run, Role: "planner", Voice: koVoice(), Guard: g}.Do(ctx, depthTools(), nil, "q", "")
	if len(rec.ran) != 0 || len(results) != 2 || results[0].Status != "cancelled" || g.HeldTotal() != 0 {
		t.Fatalf("ran %v results %+v held %v", rec.tools(), results, g.HeldTotal())
	}

	// an admission waiting for a hold, cancelled
	synctest.Test(t, func(t *testing.T) {
		g := &Guard{MaxUSD: 0.03}
		g.AllowHoldingContext(context.Background(), "a", 0.015)
		g.AllowHoldingContext(context.Background(), "b", 0.015)
		ctx, cancel := context.WithCancel(context.Background())
		var ok, done bool
		go func() {
			ok, _ = g.AllowHoldingContext(ctx, "c", 0.015)
			done = true
		}()
		synctest.Wait()
		if done {
			t.Fatal("the third search was decided while the holds alone filled the cap")
		}
		cancel()
		synctest.Wait()
		if !done || ok || math.Abs(g.HeldTotal()-0.03) > 1e-12 {
			t.Fatalf("cancelled admission: done %v ok %v held %v", done, ok, g.HeldTotal())
		}
		if _, _, skipped := g.Totals(); len(skipped) != 0 {
			t.Fatalf("a cancelled admission is no refusal: %v", skipped)
		}
	})
}

// A step that pays a fee nobody knows ahead is still waited for under a money cap: the step after it is
// decided once it has ended. One with a bound is not waited for.
func TestUnknownFeeStepIsStillWaitedFor(t *testing.T) {
	for _, maxFee := range []float64{0, 0.015} {
		synctest.Test(t, func(t *testing.T) {
			g := &Guard{MaxUSD: 0.03}
			ctx := context.Background()
			start := time.Now()
			var decidedAt time.Duration
			info := ToolInfo{ID: "search.web", Pays: true, Parallel: true, MaxFeeUSD: maxFee}
			hold := &paidHold{}
			steps := []step{
				{key: "", pays: true, maxFee: maxFee, hold: hold, decide: func() bool {
					ok, end := admitTool(ctx, g, info.ID, info, nil)
					hold.end = end
					return ok
				}, run: func() { time.Sleep(time.Second); g.Spend(0.007) }},
				{decide: func() bool { decidedAt = time.Since(start); return g.AllowContext(ctx, "weather.query") }, run: func() {}},
			}
			runSteps(ctx, g, steps)
			want := time.Second
			if maxFee > 0 {
				want = 0
			}
			if decidedAt != want {
				t.Fatalf("bound %v: the next step was decided at %v, want %v", maxFee, decidedAt, want)
			}
		})
	}
}

// The memory search the planner plans beside a web search with a bound starts beside it instead of after
// it, and is told the budget less the bound the search holds: never more than what the search leaves.
func TestMemorySearchBesideAHeldSearchIsToldWhatIsLeft(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const planCost, fee = 0.0001, 0.012
		plan := `{"memory_requirement":"required","question_kind":"event","person_targets":[],"semantic_query":"camping","tool_plan":[{"tool_name":"search.web","parameters":{"query":"x"}}]}`
		for _, capUSD := range []float64{0, 0.03} {
			m := &beganMemory{latencyMemory: &latencyMemory{started: time.Now()}}
			llm := &latencyLLM{plans: []string{plan}, planCost: planCost}
			req := requestWithPaidSearch()
			req.ToolMode = ToolModePlanner
			req.Guard = &Guard{MaxUSD: capUSD, MaxCalls: 6}
			for i := range req.Tools.Enabled {
				if req.Tools.Enabled[i].ID == webSearchTool {
					req.Tools.Enabled[i].Parallel, req.Tools.Enabled[i].MaxFeeUSD = true, 0.015
				}
			}
			if _, err := (&Runtime{LLM: llm, Memory: m, Tools: paidSearch(fee)}).Answer(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if len(m.searches) != 1 || m.began[0] >= fakePlanner+fakeSearch {
				t.Fatalf("cap %v: the memory search began at %v, after the web search ended", capUSD, m.began)
			}
			told, left := m.searches[0].RemainingUSD, 0.05-planCost-fee
			if told > left+1e-12 || math.Abs(told-(0.05-planCost-0.015)) > 1e-12 {
				t.Fatalf("cap %v: told %v; the search leaves %v and holds 0.015", capUSD, told, left)
			}
		}
	})
}

// The call cap counts calls as before: parallel searches take one call each, and the one past the cap is
// held back with "unavailable".
func TestParallelSearchesKeepTheCallCount(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{searchCall("a", `{"query":"x"}`), searchCall("b", `{"query":"y"}`),
		searchCall("c", `{"query":"z"}`)}}, {Text: "-"}}}
	rec := &recorder{}
	g := &Guard{MaxCalls: 2}
	results, tr, err := ToolLoop{LLM: llm, Run: rec.run, Role: "planner", Voice: koVoice(), Guard: g}.Do(context.Background(), depthTools(), nil, "q", "")
	if err != nil {
		t.Fatal(err)
	}
	if calls, _, _ := g.Totals(); calls != 2 || len(rec.ran) != 2 || results[2].Status != "unavailable" || tr.Steps[2].Reason != "limit" {
		t.Fatalf("calls %d ran %v results %+v", calls, rec.tools(), results)
	}
}

// The loop model reads titles, snippets, urls, dates and the read flag of search results, never the text
// read from their pages; the result itself keeps it for the answer.
func TestLoopViewLeavesOutPageText(t *testing.T) {
	r := ToolResult{Tool: webSearchTool, OK: true, Data: map[string]any{"status": "ok", "pages_read": 1, "results": []any{
		map[string]any{"title": "T1", "snippet": "S1", "url": "https://a.example/1", "datetime": "2026-11-01", "passage": strings.Repeat("page one ", 200), "read": "ok"},
		map[string]any{"title": "T2", "snippet": "S2", "url": "https://a.example/2", "text": "service text", "read": "timeout"},
	}}}
	msg := loopResultMessage(r, "n0nce")
	for _, gone := range []string{"page one", "service text", `"passage"`, `"text"`} {
		if strings.Contains(msg, gone) {
			t.Fatalf("the loop view carries %q: %s", gone, msg)
		}
	}
	for _, kept := range []string{"T1", "S1", "https://a.example/1", "2026-11-01", `"read":"ok"`, "T2", `"read":"timeout"`, `"pages_read":1`} {
		if !strings.Contains(msg, kept) {
			t.Fatalf("the loop view lost %q: %s", kept, msg)
		}
	}
	if first := r.Data["results"].([]any)[0].(map[string]any); first["passage"] == nil {
		t.Fatal("the result itself lost its passage")
	}
	plain := ToolResult{Tool: webSearchTool, OK: true, Data: map[string]any{"results": []any{map[string]any{"title": "T", "url": "u"}}}}
	if got, want := loopResultMessage(plain, "n"), fenceToolResult("n", `{"data":{"results":[{"title":"T","url":"u"}]},"ok":true,"tool":"search.web","untrusted_data":true}`); got != want {
		t.Fatalf("a result without page text changed:\n%s\n%s", got, want)
	}
}

type progressLog struct {
	mu  sync.Mutex
	got []Progress
}

func (p *progressLog) add(x Progress) {
	p.mu.Lock()
	p.got = append(p.got, x)
	p.mu.Unlock()
}

// Progress: a round that starts searches is told once per stage — search for the first, deep when one of a
// round's searches is deep, more from the second round on — with the round and its number of searches. The
// planner's steps tell it too (round 1), the shadow loop never, and a listener that panics breaks nothing.
func TestProgressIsToldOncePerStage(t *testing.T) {
	answer := func(req Request, llm *loopLLM) []Progress {
		t.Helper()
		log := &progressLog{}
		req.OnProgress = log.add
		req.Tools = depthTools()
		if _, err := (&Runtime{LLM: llm, Tools: (&recorder{}).run}).Answer(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		return log.got
	}
	loop := func(rounds ...[]LoopCall) *loopLLM {
		llm := &loopLLM{}
		for _, r := range rounds {
			llm.replies = append(llm.replies, LoopReply{Calls: r})
		}
		return llm
	}
	req := loopRequest("tell me about the match and the transfer news", ToolModeLoop)
	got := answer(req, loop(
		[]LoopCall{searchCall("a", `{"query":"match"}`), searchCall("b", `{"query":"transfer"}`), call("w", "weather__query", `{"region":"Busan"}`)},
		[]LoopCall{searchCall("c", `{"query":"more"}`)},
		[]LoopCall{searchCall("d", `{"query":"again"}`)}))
	if want := []Progress{{ProgressSearch, 1, 2}, {ProgressMore, 2, 1}}; !slices.Equal(got, want) {
		t.Fatalf("loop: %+v, want %+v", got, want)
	}
	got = answer(req, loop([]LoopCall{call("w", "weather__query", `{"region":"Busan"}`)}, []LoopCall{searchCall("a", `{"query":"x","depth":"deep"}`)}))
	if want := []Progress{{ProgressDeep, 2, 1}}; !slices.Equal(got, want) {
		t.Fatalf("deep: %+v, want %+v", got, want)
	}
	if got = answer(req, loop([]LoopCall{call("w", "weather__query", `{"region":"Busan"}`)})); len(got) != 0 {
		t.Fatalf("no search, yet %+v", got)
	}

	planned := `{"memory_requirement":"none","tool_plan":[{"tool_name":"search.web","parameters":{"query":"x","depth":"deep"}},{"tool_name":"search.web","parameters":{"query":"y"}}]}`
	got = answer(loopRequest("q", ToolModePlanner), &loopLLM{plannerOut: planned})
	if want := []Progress{{ProgressDeep, 1, 2}}; !slices.Equal(got, want) {
		t.Fatalf("planner: %+v, want %+v", got, want)
	}
	// the loop takes no tools: the planner's steps tell it
	got = answer(loopRequest("q", ToolModeLoop), &loopLLM{plannerOut: planned, errs: []error{ErrToolsUnsupported}})
	if want := []Progress{{ProgressDeep, 1, 2}}; !slices.Equal(got, want) {
		t.Fatalf("fallback: %+v, want %+v", got, want)
	}
	if got = answer(loopRequest("q", ToolModeShadow), loop([]LoopCall{searchCall("a", `{"query":"x","depth":"deep"}`)})); len(got) != 0 {
		t.Fatalf("shadow: %+v", got)
	}

	req = loopRequest("q", ToolModeLoop)
	req.OnProgress = func(Progress) { panic("listener down") }
	req.Tools = depthTools()
	res, err := (&Runtime{LLM: loop([]LoopCall{searchCall("a", `{"query":"x"}`)}), Tools: (&recorder{}).run}).Answer(context.Background(), req)
	if err != nil || len(res.Tools) != 1 || !res.Tools[0].OK {
		t.Fatalf("a panicking listener broke the turn: %+v %v", res.Tools, err)
	}
}

// The loop offers queries as a list of texts and passes the tool at most three non-empty ones; a lone
// text is a list of one.
func TestQueriesParameterIsAListOfTexts(t *testing.T) {
	var spec ToolSpec
	for _, s := range ToolSchemas(depthTools()) {
		if s.ID == webSearchTool {
			spec = s
		}
	}
	if !strings.Contains(string(spec.Parameters), `"queries":{"type":"array","items":{"type":"string"},"maxItems":3}`) {
		t.Fatalf("schema %s", spec.Parameters)
	}
	llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{
		searchCall("a", `{"query":"x","depth":"deep","queries":["a"," b ","",7,"c","d"]}`),
		searchCall("b", `{"query":"y","depth":"deep","queries":"one"}`),
		searchCall("c", `{"query":"z","queries":[]}`)}}, {Text: "-"}}}
	rec := &recorder{}
	if _, _, err := (ToolLoop{LLM: llm, Run: rec.run, Role: "planner", Voice: koVoice()}).Do(context.Background(), depthTools(), nil, "q", ""); err != nil {
		t.Fatal(err)
	}
	if len(rec.ran) != 3 {
		t.Fatalf("ran %v", rec.tools())
	}
	got := func(query string) string { // the calls run at once: found by their query, not their order
		for _, r := range rec.ran {
			if r.params["query"] == query {
				raw, _ := json.Marshal(r.params["queries"])
				return string(raw)
			}
		}
		return "missing"
	}
	if got("x") != `["a","b","c"]` || got("y") != `["one"]` || got("z") != "null" {
		t.Fatalf("queries %s %s %s", got("x"), got("y"), got("z"))
	}
}

// searchWithPassages is a search result of n items whose passages take passage runes each.
func searchWithPassages(n, passage int) ToolResult {
	var items []any
	for i := range n {
		items = append(items, map[string]any{"title": fmt.Sprintf("title %d", i), "url": fmt.Sprintf("https://site%d.example/page", i),
			"snippet": strings.Repeat("snippet ", 40), "datetime": "2026-11-10", "passage": strings.Repeat("가", passage), "read": "ok"})
	}
	return ToolResult{Tool: webSearchTool, OK: true, Data: map[string]any{"status": "ok", "query": "q", "pages_read": n, "results": items}}
}

// Without passages the tool block keeps toolBlockMax (it renders as before); passages raise it, and a deep
// search raises it most.
func TestToolBlockBudgetFollowsThePagesRead(t *testing.T) {
	plain := ToolResult{Tool: webSearchTool, OK: true, Data: fakeSearchResults(5)}
	weather := ToolResult{Tool: "weather.query", OK: true, Data: map[string]any{"temp_c": 18.0}}
	if got := toolBlockBudget([]ToolResult{plain, weather}, true); got != toolBlockMax {
		t.Fatalf("without passages %d", got)
	}
	if got := toolBlockBudget([]ToolResult{plain, searchWithPassages(1, 10)}, false); got != toolBlockPages {
		t.Fatalf("with passages %d", got)
	}
	if got := toolBlockBudget([]ToolResult{searchWithPassages(1, 10)}, true); got != toolBlockDeep {
		t.Fatalf("deep %d", got)
	}
}

// Over its budget the block cuts the passages first, the longest the most, and every result keeps its
// title, url and snippet; the results it was given are not changed. Only when the passages are all gone does
// the cut reach the results.
func TestToolBlockCutsPassagesBeforeAnyResult(t *testing.T) {
	short := searchWithPassages(1, 80)
	long := []ToolResult{searchWithPassages(5, 3000), short, searchWithPassages(5, 2000)}
	out := toolJSON(long, nil, toolBlockPages)
	if n := utf8.RuneCountInString(out); n > toolBlockPages {
		t.Fatalf("block of %d runes", n)
	}
	var block struct {
		Usable []ToolResult `json:"usable"`
	}
	if err := json.Unmarshal([]byte(out), &block); err != nil || len(block.Usable) != 3 {
		t.Fatalf("block %v: %s", err, out)
	}
	cut := 0
	for i, r := range block.Usable {
		items := resultItems(r.Data)
		if len(items) != len(resultItems(long[i].Data)) {
			t.Fatalf("result %d lost items before its passages were gone", i)
		}
		for _, it := range items {
			if it["title"] == "" || it["url"] == "" || it["snippet"] == "" {
				t.Fatalf("an item lost its title, url or snippet: %v", it)
			}
			p, _ := it["passage"].(string)
			if strings.HasSuffix(p, "…") {
				cut = max(cut, utf8.RuneCountInString(p))
			}
		}
	}
	if p := resultItems(block.Usable[1].Data)[0]["passage"]; p != strings.Repeat("가", 80) {
		t.Fatalf("the short passage was cut: %v", p)
	}
	if cut == 0 || cut > 2000 {
		t.Fatalf("the long passages were cut to %d runes", cut)
	}
	if p := resultItems(long[0].Data)[0]["passage"].(string); utf8.RuneCountInString(p) != 3000 {
		t.Fatal("the results given were changed")
	}

	// so many results that their snippets alone overflow: the passages go, then the list is cut as before
	huge := []ToolResult{searchWithPassages(60, 500)}
	out = toolJSON(huge, nil, toolBlockPages)
	if n := utf8.RuneCountInString(out); n > toolBlockPages || strings.Contains(out, `"passage"`) || !json.Valid([]byte(out)) {
		t.Fatalf("overflow: %d runes, passage left %v", n, strings.Contains(out, `"passage"`))
	}
}

// After a deep search the answer is asked for a synthesis (tool_conclusion_deep) instead of the short
// conclusion, in every pack; otherwise nothing changes.
func TestDeepSearchAsksForASynthesis(t *testing.T) {
	for _, tag := range lang.Available() {
		p, _ := lang.Get(tag)
		if strings.TrimSpace(p.Chat.ToolConclusionDeep) == "" {
			t.Fatalf("%s has no tool_conclusion_deep", tag)
		}
		v := Voice{Pack: p}
		r := ToolResult{Tool: webSearchTool, OK: true, Data: map[string]any{"results": []any{}}}
		deep := BuildAnswerMessages(v, AnswerInput{System: "S", Message: "q", Tools: []ToolResult{r}, DeepSearch: true})
		plain := BuildAnswerMessages(v, AnswerInput{System: "S", Message: "q", Tools: []ToolResult{r}})
		d, s := deep[len(deep)-1].Content, plain[len(plain)-1].Content
		if !strings.Contains(d, p.Chat.ToolConclusionDeep) || strings.Contains(s, p.Chat.ToolConclusionDeep) {
			t.Fatalf("%s: the deep line is not chosen by depth", tag)
		}
		if c := p.Chat.ToolConclusion; c != "" && (strings.Contains(d, c) || !strings.Contains(s, c)) {
			t.Fatalf("%s: the short conclusion is not replaced by the deep one", tag)
		}
	}
}

// answerLLM notes what the answer was asked with.
type answerLLM struct {
	*loopLLM
	maxTokens int
	user      string
}

func (a *answerLLM) Chat(ctx context.Context, model string, msgs []Msg, n int, temp float64) (string, float64, error) {
	if model == "response" {
		a.maxTokens, a.user = n, msgs[len(msgs)-1].Content
	}
	return a.loopLLM.Chat(ctx, model, msgs, n, temp)
}

// A turn whose loop searched deep asks the answer for the synthesis and lets it be half as long again; a
// standard search changes neither.
func TestDeepSearchTurnGetsTheSynthesisAndRoom(t *testing.T) {
	en, _ := lang.Get("en")
	for _, depth := range []string{"deep", "standard"} {
		llm := &answerLLM{loopLLM: &loopLLM{replies: []LoopReply{{Calls: []LoopCall{searchCall("a", `{"query":"x","depth":"`+depth+`"}`)}}}}}
		req := loopRequest("q", ToolModeLoop)
		req.Tools = depthTools()
		if _, err := (&Runtime{LLM: llm, Tools: (&recorder{}).run}).Answer(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		base := answerTokenLimit("q", "en")
		deep := depth == "deep"
		if wantMax := map[bool]int{true: base * 3 / 2, false: base}[deep]; llm.maxTokens != wantMax {
			t.Fatalf("%s: answer max tokens %d, want %d", depth, llm.maxTokens, wantMax)
		}
		if strings.Contains(llm.user, en.Chat.ToolConclusionDeep) != deep || strings.Contains(llm.user, en.Chat.ToolConclusion) == deep {
			t.Fatalf("%s: conclusion line in the prompt is the wrong one", depth)
		}
	}
}

// depthBound is a FeeBound by depth, as the application gives search.web.
func depthBound(params map[string]any) float64 {
	switch params["depth"] {
	case "quick":
		return 0.005
	case "deep":
		return 0.06
	}
	return 0.01
}

// A call holds the bound of its own parameters (FeeBound), decided when it is let in. A deep search whose
// bound is above the whole cap is let in all the same when nothing is spent or held yet: a step's own bound
// does not count against it, as a step's own fee never did. What it holds then makes the next step wait.
func TestFeeBoundFollowsTheCallsDepth(t *testing.T) {
	info := ToolInfo{ID: webSearchTool, Pays: true, Parallel: true, MaxFeeUSD: 0.06, FeeBound: depthBound}
	for depth, want := range map[string]float64{"quick": 0.005, "": 0.01, "deep": 0.06} {
		g := &Guard{MaxUSD: 0.03}
		ok, release := admitTool(context.Background(), g, webSearchTool, info, map[string]any{"depth": depth})
		if !ok || g.HeldTotal() != want {
			t.Fatalf("depth %q: allowed %v, held %v, want %v", depth, ok, g.HeldTotal(), want)
		}
		release()
	}
	synctest.Test(t, func(t *testing.T) {
		llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{searchCall("a", `{"query":"2s","depth":"deep"}`),
			searchCall("b", `{"query":"1s","depth":"quick"}`)}}, {Text: "-"}}}
		tt := newTimedTools(0.012)
		ts := depthTools()
		for i := range ts.Enabled {
			if ts.Enabled[i].ID == webSearchTool {
				ts.Enabled[i].MaxFeeUSD, ts.Enabled[i].FeeBound = 0.06, depthBound
			}
		}
		results, _, err := ToolLoop{LLM: llm, Run: tt.run, Role: "planner", Voice: koVoice(), Guard: &Guard{MaxUSD: 0.03}}.Do(context.Background(), ts, nil, "q", "")
		if err != nil || !results[0].OK || !results[1].OK {
			t.Fatalf("results %+v err %v", results, err)
		}
		// the deep search's 0.06 fills the cap: the quick one waits for its real fee (0.012), then runs
		if tt.starts["2s"] != 0 || tt.starts["1s"] != 2*time.Second {
			t.Fatalf("starts %v", tt.starts)
		}
	})
}

// When what a deep search holds would leave the memory search less than its reranker needs, the memory
// search waits for the search to end and is told what it really left, as before fees had bounds. A search
// whose bound leaves enough does not hold it up.
func TestMemorySearchWaitsForADeepHoldThatStarvesIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const planCost, fee = 0.0001, 0.012
		for _, depth := range []string{"deep", "standard"} {
			plan := `{"memory_requirement":"required","question_kind":"event","person_targets":[],"semantic_query":"camping","tool_plan":[{"tool_name":"search.web","parameters":{"query":"x","depth":"` + depth + `"}}]}`
			m := &beganMemory{latencyMemory: &latencyMemory{started: time.Now()}}
			llm := &latencyLLM{plans: []string{plan}, planCost: planCost}
			req := requestWithPaidSearch()
			req.ToolMode = ToolModePlanner
			req.Guard = &Guard{MaxUSD: 0.2, MaxCalls: 6}
			for i := range req.Tools.Enabled {
				if req.Tools.Enabled[i].ID == webSearchTool {
					req.Tools.Enabled[i].Parallel, req.Tools.Enabled[i].MaxFeeUSD, req.Tools.Enabled[i].FeeBound = true, 0.06, depthBound
				}
			}
			if _, err := (&Runtime{LLM: llm, Memory: m, Tools: paidSearch(fee)}).Answer(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			told := m.searches[0].RemainingUSD
			switch depth {
			case "deep":
				if m.began[0] < fakePlanner+fakeSearch || math.Abs(told-(0.05-planCost-fee)) > 1e-12 {
					t.Fatalf("deep: began %v told %v; want after the search, told what it left", m.began[0], told)
				}
			default:
				if m.began[0] >= fakePlanner+fakeSearch || math.Abs(told-(0.05-planCost-0.01)) > 1e-12 {
					t.Fatalf("standard: began %v told %v; want beside the search, less its bound", m.began[0], told)
				}
			}
		}
	})
}

// A deep search may take up to its service's own deadline (45 s): nothing in the loop or the guard cuts it
// short of the turn's context.
func TestALongDeepSearchIsNotCutShort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{searchCall("a", `{"query":"44s","depth":"deep"}`), call("w", "weather__query", `{"region":"1s"}`)}}, {Text: "-"}}}
		tt := newTimedTools(0.05)
		req := loopRequest("q", ToolModeLoop)
		req.Tools = depthTools()
		req.Guard = &Guard{MaxUSD: 0.03, MaxCalls: 10}
		start := time.Now()
		res, err := (&Runtime{LLM: llm, Tools: tt.run}).Answer(context.Background(), req)
		if err != nil || len(res.Tools) != 2 || !res.Tools[0].OK || res.Tools[0].Data["name"] != "44s" {
			t.Fatalf("results %+v err %v", res.Tools, err)
		}
		if took := time.Since(start); took < 44*time.Second {
			t.Fatalf("the turn ended after %v, before the search", took)
		}
	})
}
