package chat

import (
	"context"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/memory/engine"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
)

// barrier lets its n callers on only once all of them have arrived; one that waits five seconds in vain
// gets false (the steps ran one after another).
type barrier struct {
	n, arrived int
	mu         sync.Mutex
	all        chan struct{}
}

func newBarrier(n int) *barrier { return &barrier{n: n, all: make(chan struct{})} }

func (b *barrier) wait() bool {
	b.mu.Lock()
	if b.arrived++; b.arrived == b.n {
		close(b.all)
	}
	b.mu.Unlock()
	select {
	case <-b.all:
		return true
	case <-time.After(5 * time.Second):
		return false
	}
}

// The calls of one round run at once; each tool's own spending is what the call ledger reads off its
// guard (no share of a call running beside it), and the results keep the model's order even when a
// later call finishes first.
func TestToolLoopRunsARoundsCallsAtOnce(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{
		{Calls: []LoopCall{call("a", "finance__quote", `{"symbol":"AAPL"}`), call("b", "weather__query", `{"region":"Busan"}`)}},
		{Text: "-"},
	}}
	b := newBarrier(2)
	spend := map[string]float64{"finance.quote": 0.01, "weather.query": 0.02}
	var mu sync.Mutex
	own := map[string]float64{}
	tools := func(ctx context.Context, tool string, _ map[string]any) ToolResult {
		_, before, _ := GuardFrom(ctx).Totals()
		together := b.wait()
		if tool == "finance.quote" {
			time.Sleep(30 * time.Millisecond) // the first call finishes last
		}
		GuardFrom(ctx).Spend(spend[tool])
		_, after, _ := GuardFrom(ctx).Totals() // as the application's call ledger reads it
		mu.Lock()
		own[tool] = after - before
		mu.Unlock()
		return ToolResult{Tool: tool, OK: together}
	}
	g := &Guard{MaxUSD: 1, MaxCalls: 20}
	req := loopRequest("apple stock and the weather in Busan?", ToolModeLoop)
	req.Guard = g
	out, err := (&Runtime{LLM: llm, Tools: tools}).Answer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 2 || out.Tools[0].Tool != "finance.quote" || out.Tools[1].Tool != "weather.query" {
		t.Fatalf("results not in the model's order: %+v", out.Tools)
	}
	if !out.Tools[0].OK || !out.Tools[1].OK {
		t.Fatalf("the round's calls ran one after another: %+v", out.Tools)
	}
	if own["finance.quote"] != 0.01 || own["weather.query"] != 0.02 {
		t.Fatalf("each call's own cost: %v", own)
	}
	if calls, spent, _ := g.Totals(); calls != 2 || math.Abs(spent-0.03) > 1e-12 {
		t.Fatalf("turn totals: %d calls, %v spent", calls, spent)
	}
	next := llm.loopMsgs[1]
	if len(next) != 5 || next[3].CallID != "a" || next[4].CallID != "b" {
		t.Fatalf("tool messages out of order: %+v", next)
	}
}

// The turn's limits are decided in the model's order before anything runs: the same calls are allowed
// as when they ran one after another. A call over the cap gets the same "unavailable" result.
func TestToolLoopRoundKeepsTheTurnsLimits(t *testing.T) {
	round := []LoopCall{call("a", "finance__quote", `{"symbol":"AAPL"}`), call("b", "weather__query", `{"region":"Busan"}`),
		call("c", "finance__quote", `{"symbol":"MSFT"}`)}
	run := func(g *Guard, pays bool) (Result, *recorder) {
		llm := &loopLLM{replies: []LoopReply{{Calls: round}, {Text: "-"}}}
		rec := &recorder{}
		tools := func(ctx context.Context, tool string, p map[string]any) ToolResult {
			if tool == "finance.quote" {
				GuardFrom(ctx).Spend(0.05) // more than the turn's share
			}
			return rec.run(ctx, tool, p)
		}
		req := loopRequest("apple, msft and the weather", ToolModeLoop)
		for i := range req.Tools.Enabled {
			req.Tools.Enabled[i].Pays = pays && req.Tools.Enabled[i].ID == "finance.quote"
		}
		req.Guard = g
		out, err := (&Runtime{LLM: llm, Tools: tools}).Answer(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		return out, rec
	}
	limited := func(out Result) []string {
		var ids []string
		for _, s := range out.ToolLoop.Steps {
			if s.Reason == "limit" {
				ids = append(ids, s.Tool)
			}
		}
		return ids
	}
	// a call cap of two: the third call is held back
	out, rec := run(&Guard{MaxCalls: 2}, false)
	if got := rec.tools(); len(got) != 2 || !slices.Equal(limited(out), []string{"finance.quote"}) {
		t.Fatalf("call cap: ran %v, limited %v", got, limited(out))
	}
	if last := out.Tools[2]; last.Tool != "finance.quote" || last.OK || last.Status != "unavailable" {
		t.Fatalf("held-back call: %+v", out.Tools)
	}
	// a money cap: the quote pays for itself, so the calls after it wait for what it spent and are held back
	out, rec = run(&Guard{MaxUSD: 0.03}, true)
	if got := rec.tools(); !slices.Equal(got, []string{"finance.quote"}) || !slices.Equal(limited(out), []string{"weather.query", "finance.quote"}) {
		t.Fatalf("money cap: ran %v, limited %v", got, limited(out))
	}
}

// cancelLLM cancels the turn as soon as the loop model has asked for its calls.
type cancelLLM struct {
	*loopLLM
	cancel context.CancelFunc
}

func (c cancelLLM) ChatTools(ctx context.Context, role string, msgs []LoopMsg, tools []ToolSpec, n int) (LoopReply, error) {
	r, err := c.loopLLM.ChatTools(ctx, role, msgs, tools, n)
	c.cancel()
	return r, err
}

// A turn cancelled before its round runs runs nothing: every call says cancelled.
func TestToolLoopRoundStopsWhenTheTurnIsCancelled(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	llm := cancelLLM{&loopLLM{replies: []LoopReply{{Calls: []LoopCall{call("a", "finance__quote", `{"symbol":"AAPL"}`), call("b", "weather__query", `{"region":"Busan"}`)}}}}, cancel}
	rec := &recorder{}
	l := ToolLoop{LLM: llm, Run: rec.run, Role: "planner", Voice: loopRequest("x", ToolModeLoop).Voice}
	results, _, err := l.Do(ctx, loopTools(), nil, "apple and the weather", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.ran) != 0 || len(results) != 2 || results[0].Status != "cancelled" || results[1].Status != "cancelled" {
		t.Fatalf("ran %v, results %+v", rec.tools(), results)
	}
}

// A tool that panics fails alone: it runs on its own goroutine, out of reach of the turn's recover.
func TestToolLoopRoundSurvivesAPanickingTool(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{call("a", "finance__quote", `{"symbol":"AAPL"}`), call("b", "weather__query", `{"region":"Busan"}`)}}, {Text: "-"}}}
	tools := func(_ context.Context, tool string, _ map[string]any) ToolResult {
		if tool == "finance.quote" {
			panic("boom")
		}
		return ToolResult{Tool: tool, OK: true}
	}
	out, err := (&Runtime{LLM: llm, Tools: tools}).Answer(context.Background(), loopRequest("apple and the weather", ToolModeLoop))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 2 || out.Tools[0].OK || out.Tools[0].Status != "unavailable" || !out.Tools[1].OK {
		t.Fatalf("results %+v", out.Tools)
	}
}

// waitingMemory finds one memory, but only once the planner's tools are running beside it.
type waitingMemory struct {
	b        *barrier
	together bool
}

func (m *waitingMemory) Search(_ context.Context, req engine.SearchRequest) (search.Result, error) {
	m.together = m.b.wait()
	return search.Result{State: retrieval.StateCandidates, Hits: []search.Hit{{Rank: 1, DocID: "l1", Room: req.Rooms[0], Date: "2026-09-01", Excerpt: "철수: 가평 가자"}},
		Trace: map[string]any{"rerank_cost_usd": 0.0003}}, nil
}

// The planner's tools run at once with each other and with the memory search; results keep the plan's
// order and the trace keeps its tools and memory timings.
func TestPlannerToolsAndMemorySearchRunAtOnce(t *testing.T) {
	plan := `{"memory_requirement":"required","question_kind":"event","person_targets":[],"tool_plan":[` +
		`{"tool_name":"weather.query","parameters":{"region":"서울"}},{"tool_name":"finance.quote","parameters":{"symbol":"AAPL"}},` +
		`{"tool_name":"memory.search","parameters":{"query":"캠핑"}}]}`
	b := newBarrier(3)
	tools := func(_ context.Context, tool string, _ map[string]any) ToolResult {
		together := b.wait()
		if tool == "weather.query" {
			time.Sleep(30 * time.Millisecond) // the first tool finishes last
		}
		return ToolResult{Tool: tool, OK: together}
	}
	mem := &waitingMemory{b: b}
	res, err := (&Runtime{LLM: &fakeLLM{plan: plan}, Memory: mem, Tools: tools}).Answer(context.Background(), Request{
		Message: "서울 날씨랑 애플 주가, 그리고 캠핑 어디 가기로 했지?", Speaker: "영희", Rooms: []string{"discord:1:10"}, MemoryOn: true,
		PlannerModel: "planner", ResponseModel: "resp", BudgetUSD: 0.05, Voice: koVoice(), Tools: goldenTools([]string{"weather.query", "finance.quote"})})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 2 || res.Tools[0].Tool != "weather.query" || res.Tools[1].Tool != "finance.quote" {
		t.Fatalf("results not in the plan's order: %+v", res.Tools)
	}
	if !res.Tools[0].OK || !res.Tools[1].OK || !mem.together {
		t.Fatalf("the tools and the memory search ran one after another: %+v memory together %v", res.Tools, mem.together)
	}
	if res.RagState != "candidates" || len(res.Hits) != 1 || math.Abs(res.CostUSD-(0.0001+0.0003+0.0002)) > 1e-9 {
		t.Fatalf("memory: state %s hits %d cost %v", res.RagState, len(res.Hits), res.CostUSD)
	}
	if res.Timing["tools"] < 30 {
		t.Fatalf("timing %v", res.Timing)
	}
	if _, ok := res.Timing["memory"]; !ok {
		t.Fatalf("timing %v", res.Timing)
	}
}

// All three tools must reach the barrier, independent of the machine's scheduling speed.
func TestToolLoopRoundLatency(t *testing.T) {
	round := []LoopCall{call("a", "finance__quote", `{"symbol":"AAPL"}`), call("b", "weather__query", `{"region":"Busan"}`),
		call("c", "search__web", `{"query":"busan festival"}`)}
	llm := &loopLLM{replies: []LoopReply{{Calls: round}, {Text: "-"}}}
	b := newBarrier(3)
	tools := func(_ context.Context, tool string, _ map[string]any) ToolResult {
		return ToolResult{Tool: tool, OK: b.wait()}
	}
	out, err := (&Runtime{LLM: llm, Tools: tools}).Answer(context.Background(), loopRequest("apple, weather and festivals in Busan", ToolModeLoop))
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 3 || !out.Tools[0].OK || !out.Tools[1].OK || !out.Tools[2].OK {
		t.Fatal("the three tools did not reach the barrier together")
	}
}

// The compatibility fallback reproduces the old production adapters' completion-time Note, so the
// exact same regression test can execute (and fail) on the original commit.
func beginTestModel(g *Guard) func(float64) {
	if b, ok := any(g).(interface{ Begin() func(float64) }); ok {
		return b.Begin()
	}
	return func(cost float64) { g.Note(cost) }
}

func TestGuardMoneyWaitFollowsCancellation(t *testing.T) {
	g := &Guard{MaxUSD: 1}
	end := beginTestModel(g)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		if a, ok := any(g).(interface {
			AllowContext(context.Context, string) bool
		}); ok {
			done <- a.AllowContext(ctx, "optional")
		} else {
			done <- g.Allow("optional")
		}
	}()
	select {
	case <-done:
		t.Error("money admission did not wait for the model in flight")
	case <-time.After(30 * time.Millisecond):
	}
	cancel()
	select {
	case allowed := <-done:
		if allowed {
			t.Error("cancelled admission was allowed")
		}
	case <-time.After(time.Second):
		t.Error("money admission ignored cancellation")
	}
	end(0.2)
	end(0.3) // End is idempotent even when a completion also has a deferred panic cleanup.
	if calls, cost, skipped := g.Totals(); calls != 1 || math.Abs(cost-0.2) > 1e-12 || len(skipped) != 0 {
		t.Errorf("model end or cancellation changed totals: calls=%d cost=%g skipped=%d", calls, cost, len(skipped))
	}
}

type slowGuardPlanner struct {
	*loopLLM
	started, release, done chan struct{}
	cost                   float64
}

func (f *slowGuardPlanner) Chat(ctx context.Context, model string, msgs []Msg, n int, temp float64) (string, float64, error) {
	if model != "planner" {
		return f.loopLLM.Chat(ctx, model, msgs, n, temp)
	}
	end := beginTestModel(GuardFrom(ctx))
	close(f.started)
	<-f.release
	end(f.cost)
	close(f.done)
	return `{"memory_requirement":"none","tool_plan":[]}`, f.cost, nil
}
func (f *slowGuardPlanner) ChatTools(ctx context.Context, role string, msgs []LoopMsg, tools []ToolSpec, n int) (LoopReply, error) {
	<-f.started
	end := beginTestModel(GuardFrom(ctx))
	r, err := f.loopLLM.ChatTools(ctx, role, msgs, tools, n)
	end(0)
	return r, err
}

func TestOptionalAdmissionIncludesSlowPlanner(t *testing.T) {
	for _, money := range []bool{false, true} {
		t.Run(fmt.Sprintf("money=%v", money), func(t *testing.T) {
			g := &Guard{MaxCalls: 3}
			if money {
				g.MaxCalls, g.MaxUSD = 0, 0.01
			}
			f := &slowGuardPlanner{loopLLM: &loopLLM{replies: []LoopReply{{Calls: []LoopCall{
				call("a", "weather__query", `{"region":"a"}`), call("b", "finance__quote", `{"symbol":"B"}`),
			}}, {Text: "-"}}}, started: make(chan struct{}), release: make(chan struct{}), done: make(chan struct{}), cost: 0.02}
			var ran atomic.Int32
			both := make(chan struct{})
			go func() {
				// The old parallel admissions let both start while the planner is still in flight.
				select {
				case <-both:
				case <-time.After(100 * time.Millisecond):
				}
				close(f.release)
			}()
			tools := func(_ context.Context, name string, _ map[string]any) ToolResult {
				if ran.Add(1) == 2 {
					close(both)
				}
				<-f.done // the planner ends before the first tool finishes
				return ToolResult{Tool: name, OK: true}
			}
			req := loopRequest("q", ToolModeLoop)
			req.Guard = g
			out, err := (&Runtime{LLM: f, Tools: tools}).Answer(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			want := int32(1)
			if money {
				want = 0
			}
			if ran.Load() != want || len(out.Tools) != 2 || out.Tools[1].Status != "unavailable" {
				t.Errorf("slow planner: ran %d optional calls, expected %d and second refused", ran.Load(), want)
			}
		})
	}
}

func fiveTools() (Toolset, []LoopCall) {
	ts := Toolset{Known: map[string]bool{}}
	var calls []LoopCall
	for i, name := range []string{"weather.query", "finance.quote", "finance.fx", "help.version", "polls.create"} {
		ts.Known[name] = true
		ts.Enabled = append(ts.Enabled, ToolInfo{ID: name, Params: []ToolParam{{Name: "value"}}})
		calls = append(calls, call(fmt.Sprint(i), strings.ReplaceAll(name, ".", "__"), `{"value":"x"}`))
	}
	return ts, calls
}

// Make the same slot-saturation regression executable against both versions of runSteps.
func runTestSteps(ctx context.Context, g *Guard, steps []step) {
	switch run := any(runSteps).(type) {
	case func(context.Context, *Guard, []step):
		run(ctx, g, steps)
	case func(*Guard, []step):
		run(g, steps)
	default:
		panic("unexpected runSteps signature")
	}
}

func TestRunStepsCancellationWhileWaitingForSlot(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make([]chan struct{}, 5)
	for i := range started {
		started[i] = make(chan struct{})
	}
	release, done, waiting := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var polls atomic.Int32
	var steps []step
	for i := range started {
		steps = append(steps, step{decide: func() bool {
			// The next admission waits for the previous start, so the first four slots are
			// deterministically occupied even on the original goroutine-based scheduler.
			if i > 0 {
				<-started[i-1]
			}
			if i == 4 {
				close(waiting)
			}
			return true
		}, run: func() {
			close(started[i])
			if i == 4 {
				polls.Add(1)
			}
			<-release
		}})
	}
	go func() { runTestSteps(ctx, nil, steps); close(done) }()
	for i := range 4 {
		select {
		case <-started[i]:
		case <-time.After(3 * time.Second):
			t.Fatal("first four slots did not fill")
		}
	}
	select {
	case <-started[4]:
		t.Fatal("fifth call exceeded four slots")
	default:
	}
	select {
	case <-waiting:
	case <-time.After(3 * time.Second):
		t.Fatal("fifth call was not admitted to the slot wait")
	}
	cancel()
	close(release)
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled scheduler did not finish")
	}
	if polls.Load() != 0 {
		t.Error("the waiting fifth call ran its local side effect after cancellation")
	}
}

func TestToolLoopCapsFourAndCancelsWaitingCall(t *testing.T) {
	for _, cancelTurn := range []bool{false, true} {
		t.Run(fmt.Sprintf("cancel=%v", cancelTurn), func(t *testing.T) {
			ts, calls := fiveTools()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			g := &Guard{}
			entered := make(chan string, 5)
			release := make(chan struct{})
			var active, most, polls atomic.Int32
			tools := func(_ context.Context, name string, _ map[string]any) ToolResult {
				n := active.Add(1)
				for m := most.Load(); n > m && !most.CompareAndSwap(m, n); m = most.Load() {
				}
				if name == "polls.create" {
					polls.Add(1)
				}
				entered <- name
				<-release
				active.Add(-1)
				return ToolResult{Tool: name, OK: true}
			}
			llm := &loopLLM{replies: []LoopReply{{Calls: calls}, {Text: "-"}}}
			l := ToolLoop{LLM: llm, Run: tools, Role: "planner", Voice: koVoice(), Guard: g}
			done := make(chan []ToolResult, 1)
			go func() { results, _, _ := l.Do(ctx, ts, nil, "q", ""); done <- results }()
			for range 4 {
				select {
				case <-entered:
				case <-time.After(3 * time.Second):
					t.Fatal("four tools did not start")
				}
			}
			if cancelTurn {
				cancel()
			}
			close(release)
			var results []ToolResult
			select {
			case results = <-done:
			case <-time.After(3 * time.Second):
				t.Fatal("round did not finish")
			}
			if most.Load() != 4 || len(results) != 5 {
				t.Fatalf("max active=%d results=%d", most.Load(), len(results))
			}
			for i, r := range results {
				if r.Tool != ts.Enabled[i].ID {
					t.Error("result order changed")
				}
			}
			if cancelTurn {
				if polls.Load() != 0 || results[4].Status != "cancelled" {
					t.Error("waiting local side effect ran after cancellation")
				}
				if calls, _, _ := g.Totals(); calls != 4 {
					t.Errorf("unstarted reservation not returned: %d", calls)
				}
			} else if polls.Load() != 1 {
				t.Error("fifth runnable call never ran")
			}
		})
	}
}
