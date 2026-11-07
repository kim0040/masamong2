package chat

import (
	"context"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"masamong/internal/memory/engine"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
)

// paidSearch is a web search that charges its fee: the cap's view of it (Spend) and the turn's cost (Paid),
// as search.web does.
func paidSearch(fee float64) ToolRunner {
	return func(ctx context.Context, tool string, _ map[string]any) ToolResult {
		time.Sleep(fakeSearch)
		g := GuardFrom(ctx)
		g.Spend(fee)
		g.Paid(fee)
		return ToolResult{Tool: tool, OK: true, Data: map[string]any{"status": "ok"}}
	}
}

// requestWithPaidSearch is latencyRequest whose web search pays for itself.
func requestWithPaidSearch() Request {
	req := latencyRequest()
	req.Guard = &Guard{MaxUSD: 0.2, MaxCalls: 6} // room for the fee: the cap is not what these tests are about
	tools := req.Tools.Enabled
	req.Tools = Toolset{Known: req.Tools.Known, Enabled: append([]ToolInfo(nil), tools...)}
	for i := range req.Tools.Enabled {
		if req.Tools.Enabled[i].ID == "search.web" {
			req.Tools.Enabled[i].Pays = true
		}
	}
	return req
}

// beganMemory notes when each search began.
type beganMemory struct {
	*latencyMemory
	began []time.Duration
}

func (m *beganMemory) Search(ctx context.Context, req engine.SearchRequest) (search.Result, error) {
	m.mu.Lock()
	m.began = append(m.began, time.Since(m.started))
	m.mu.Unlock()
	return m.latencyMemory.Search(ctx, req)
}

// The search that starts beside the loop is not told more than the one after it would be told. Rounds
// dearer than the planner's call (a longer prompt, a model without the planner's discount) and a search
// fee still on its way are what the planner's price cannot know: the search waits for the fee, and counts
// the rounds still to come at the dearest model call the turn has seen.
func TestEarlySearchIsNotToldMoreThanTheLoopAndFeesLeave(t *testing.T) {
	const planCost = 0.0001
	cases := []struct {
		name           string
		loopCost, fee  float64
		sequentialLeft func() float64 // what the search after the loop would be told: 2 rounds ran
	}{
		{"dear rounds", 0.008, 0.004, nil},
		{"dear fee", 0.0001, 0.03, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := &latencyMemory{started: time.Now()}
				llm := &latencyLLM{plans: []string{planMemory}, rounds: searchRound(), roundTime: []time.Duration{fakeRound1, fakeRound2}, planCost: planCost, loopCost: c.loopCost}
				req := requestWithPaidSearch()
				res, err := (&Runtime{LLM: llm, Memory: plainMemory{m}, Tools: paidSearch(c.fee)}).Answer(context.Background(), req)
				if err != nil {
					t.Fatal(err)
				}
				sequential := 0.05 - planCost - 2*c.loopCost - c.fee
				if got := m.searches[0].RemainingUSD; got > sequential+1e-12 {
					t.Fatalf("told %v, more than the %v the search after the loop would be told", got, sequential)
				}
				if want := planCost + 2*c.loopCost + c.fee; math.Abs(res.CostUSD-want) > 1e-12 {
					t.Fatalf("turn cost %v, want %v", res.CostUSD, want)
				}
			})
		})
	}
}

// A loop that asks another model than the planner's has a price nothing the turn has seen bounds: the
// search waits for it, so that it is told what is really left.
func TestSearchBesideAnotherModelsLoopWaitsForTheLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const planCost, loopCost = 0.0001, 0.008
		m := &beganMemory{latencyMemory: &latencyMemory{started: time.Now()}}
		llm := &latencyLLM{plans: []string{planMemory}, rounds: searchRound(), roundTime: []time.Duration{fakeRound1, fakeRound2}, planCost: planCost, loopCost: loopCost}
		req := latencyRequest()
		req.LoopModel = "response"
		if _, err := (&Runtime{LLM: llm, Memory: m, Tools: latencyTools}).Answer(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		loopEnd := fakeRound1 + fakeSearch + fakeRound2
		if len(m.began) != 1 || m.began[0] < loopEnd {
			t.Fatalf("the search began at %v, before the loop ended at %v", m.began, loopEnd)
		}
		if got, want := m.searches[0].RemainingUSD, 0.05-planCost-2*loopCost; math.Abs(got-want) > 1e-12 {
			t.Fatalf("told %v, want %v", got, want)
		}
	})
}

// The search after the loop is told what the tools' fees left out of the budget too.
func TestSearchAfterTheLoopCountsTheFeesPaid(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const planCost, loopCost, fee = 0.0001, 0.0001, 0.03
		m := &latencyMemory{started: time.Now()}
		// both rounds and the search end before the planner does: the memory step starts after the loop
		llm := &latencyLLM{plans: []string{planMemory}, rounds: searchRound(), roundTime: []time.Duration{100 * time.Millisecond, 100 * time.Millisecond}, planCost: planCost, loopCost: loopCost}
		req := requestWithPaidSearch()
		req.Guard = &Guard{MaxCalls: 6} // no money cap: the memory search is not held up by the fee
		if _, err := (&Runtime{LLM: llm, Memory: plainMemory{m}, Tools: paidSearch(fee)}).Answer(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if got, want := m.searches[0].RemainingUSD, 0.05-planCost-2*loopCost-fee; math.Abs(got-want) > 1e-12 {
			t.Fatalf("told %v, want %v: the fee is left out", got, want)
		}
	})
}

// A search that is dropped after it paid still costs what it paid, in the turn's cost and in the guard's spending,
// and takes no call from the tools still to come. The first search's reranker, when it searched twice, is on the
// guard already (searchMemory) and comes back for the turn's cost. A search that never ran cost nothing.
func TestDiscardedSearchKeepsItsCostAndTakesNoCall(t *testing.T) {
	discarded := func(mem memoryRun) *earlyMemory {
		done := make(chan struct{})
		close(done)
		return &earlyMemory{cancel: func() {}, done: done, mem: mem}
	}
	g := &Guard{MaxCalls: 1}
	sr := search.Result{Trace: map[string]any{"rerank_cost_usd": 0.0003}}
	if got := discarded(memoryRun{ran: true, sr: sr, firstRerank: 0.0002}).discard(g); math.Abs(got-0.0005) > 1e-12 {
		t.Fatalf("turn cost %v, want both reranks", got)
	}
	calls, spent, _ := g.Totals()
	if calls != 0 || math.Abs(spent-0.0003) > 1e-12 {
		t.Fatalf("guard: %d calls, spent %v", calls, spent)
	}
	if got := discarded(memoryRun{}).discard(g); got != 0 {
		t.Fatalf("a search that never ran cost %v", got)
	}
}

// A search that widened its window pays two reranks: the first goes on the guard when it widens (searchMemory),
// the second when the search is dropped, and the turn counts each once.
func TestDroppedWidenedSearchCountsBothReranksOnce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		req := latencyRequest()
		req.Guard = &Guard{MaxCalls: 8}
		rt := &Runtime{Memory: &windowMemory{}}
		mem := rt.searchMemory(context.Background(), req, "camping", search.Intent{Recent: true}, 0.05, time.Now())
		if !mem.relaxed {
			t.Fatalf("the search did not widen: %+v", mem)
		}
		done := make(chan struct{})
		close(done)
		cost := (&earlyMemory{cancel: func() {}, done: done, mem: mem}).discard(req.Guard)
		calls, spent, _ := req.Guard.Totals()
		if want := 0.0005; math.Abs(cost-want) > 1e-12 || math.Abs(spent-want) > 1e-12 || calls != 0 {
			t.Fatalf("turn cost %v, guard spent %v, %d calls; want both %v and no call", cost, spent, calls, want)
		}
	})
}

// However the race between a search that has just been let go and the second plan ends, what the search paid is
// in the turn's cost and in the guard's spending alike, and takes no call.
func TestAReplanLeavesTheTurnCostAndTheGuardAgreeing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const planRecent = `{"memory_requirement":"required","question_kind":"event","person_targets":[],"semantic_query":"camping","time_scope":"recent","tool_plan":[]}`
		llm := &latencyLLM{plans: []string{planRecent, planNoMemory}, loopErr: ErrToolsUnsupported, loopErrAt: time.Second}
		req := latencyRequest()
		req.Guard = &Guard{MaxCalls: 8}
		res, err := (&Runtime{LLM: llm, Memory: &windowMemory{}}).Answer(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		calls, spent, _ := req.Guard.Totals()
		if math.Abs(res.CostUSD-spent) > 1e-12 {
			t.Fatalf("turn cost %v, guard spent %v", res.CostUSD, spent)
		}
		// two planner calls, the loop's one and the answer
		if calls != 4 {
			t.Fatalf("guard counted %d calls", calls)
		}
	})
}

// A cut-off answer goes on in a second call: the answer's time and span run to its end.
type continuingLLM struct {
	*latencyLLM
	mu    sync.Mutex
	calls int
}

func (c *continuingLLM) ChatDetailed(ctx context.Context, model string, msgs []Msg, max int, temp float64) (Completion, error) {
	if model == "planner" {
		text, cost, err := c.Chat(ctx, model, msgs, max, temp)
		return Completion{Text: text, CostUSD: cost}, err
	}
	c.mu.Lock()
	c.calls++
	n := c.calls
	c.mu.Unlock()
	if n == 1 {
		return Completion{Text: "That is the first part.", FinishReason: "length"}, sleepCtx(ctx, fakeAnswer)
	}
	return Completion{Text: "And the rest.", FinishReason: "stop"}, sleepCtx(ctx, 20*time.Second)
}

func TestAnswerTimeIncludesTheContinuation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		llm := &continuingLLM{latencyLLM: &latencyLLM{plans: []string{planNoMemory}}}
		req := loopRequest("tell me a long story", ToolModePlanner)
		res, err := (&Runtime{LLM: llm}).Answer(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if res.Answer != "That is the first part.\nAnd the rest." {
			t.Fatalf("answer %q", res.Answer)
		}
		want := int((fakeAnswer + 20*time.Second).Milliseconds())
		if res.Timing["answer"] < want {
			t.Fatalf("answer time %d ms, want at least %d: the continuation is left out", res.Timing["answer"], want)
		}
		for _, s := range res.Spans {
			if s.Name == "answer" {
				if s.EndMS-s.StartMS < want {
					t.Fatalf("answer span %d–%d ms, shorter than the %d ms of both calls", s.StartMS, s.EndMS, want)
				}
				return
			}
		}
		t.Fatalf("no answer span in %+v", res.Spans)
	})
}

// Under a money cap, a step is not admitted while a step that pays for itself is under way: its fee is
// not in the spending yet. WaitSpent returns when it has ended; MaxCall is the dearest model call so far.
func TestMoneyCapWaitsForAStepThatPays(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		g := &Guard{MaxUSD: 0.03}
		ok, end := g.AllowPayingContext(context.Background(), "search.web", true)
		if !ok {
			t.Fatal("the first step is allowed")
		}
		g.Begin()(0.004)
		started := time.Now()
		var admitted, waited time.Duration
		go func() {
			if !g.AllowMoneyContext(context.Background(), "memory.search") {
				t.Error("refused")
			}
			admitted = time.Since(started)
		}()
		go func() {
			g.WaitSpent(context.Background())
			waited = time.Since(started)
		}()
		time.Sleep(2 * time.Second)
		g.Spend(0.01)
		g.Paid(0.01)
		end()
		end() // once
		synctest.Wait()
		if admitted != 2*time.Second || waited != 2*time.Second {
			t.Fatalf("admitted after %v, WaitSpent returned after %v, want both 2s", admitted, waited)
		}
		if got := g.MaxCall(); got != 0.004 {
			t.Fatalf("dearest call %v", got)
		}
		if got := g.child().MaxCall(); got != 0.004 {
			t.Fatalf("the child sees the turn's: %v", got)
		}
	})
}

// windowMemory finds nothing in the planner's time window (paying 0.0002 for its rerank) and something
// without it (0.0003).
type windowMemory struct {
	mu    sync.Mutex
	calls int
}

func (w *windowMemory) Search(ctx context.Context, req engine.SearchRequest) (search.Result, error) {
	if err := sleepCtx(ctx, fakeEmbed); err != nil {
		return search.Result{}, err
	}
	w.mu.Lock()
	w.calls++
	n := w.calls
	w.mu.Unlock()
	if n == 1 {
		return search.Result{State: retrieval.StateEmptyInScope, Trace: map[string]any{"abstain_reason": "empty_in_allowed_scope", "rerank_cost_usd": 0.0002}}, nil
	}
	return search.Result{State: retrieval.StateCandidates, Hits: []search.Hit{{Rank: 1, DocID: "d", Room: req.Rooms[0], Date: "2026-09-01", Excerpt: "x"}},
		Trace: map[string]any{"rerank_cost_usd": 0.0003}}, nil
}

// Without a money cap nothing holds the search back while the loop's first call is under way, and only that call
// says what the loop costs: the search waits for it, so a loop dearer than the planner is not priced as the planner.
func TestEarlySearchWithoutACapWaitsForTheLoopsFirstCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const planCost, loopCost = 0.0001, 0.008
		m := &beganMemory{latencyMemory: &latencyMemory{started: time.Now()}}
		llm := &latencyLLM{plans: []string{planMemory}, rounds: searchRound(), roundTime: []time.Duration{fakeRound1, fakeRound2}, planCost: planCost, loopCost: loopCost}
		req := latencyRequest()
		req.Guard = &Guard{MaxCalls: 6}
		req.BudgetUSD = 0.03
		if _, err := (&Runtime{LLM: llm, Memory: m, Tools: latencyTools}).Answer(context.Background(), req); err != nil {
			t.Fatal(err)
		}
		if got, sequential := m.searches[0].RemainingUSD, 0.03-planCost-2*loopCost; got > sequential+1e-12 {
			t.Fatalf("told %v, more than the %v the search after the loop would be told", got, sequential)
		}
		if m.began[0] < fakeRound1 {
			t.Fatalf("the search began at %v, before the loop's first call ended at %v", m.began[0], fakeRound1)
		}
	})
}

// With the planner choosing the tools, the memory search is decided after the tools it plans beside and is
// told what is left once their fees are in, not what was left when the steps were laid out.
func TestPlannedSearchCountsTheFeeOfThePlannedTool(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		const planCost, fee = 0.0001, 0.03
		m := &latencyMemory{started: time.Now()}
		plan := `{"memory_requirement":"required","question_kind":"event","person_targets":[],"semantic_query":"camping","tool_plan":[{"tool_name":"search.web","parameters":{"query":"x"}}]}`
		llm := &latencyLLM{plans: []string{plan}, planCost: planCost}
		req := requestWithPaidSearch()
		req.ToolMode = ToolModePlanner
		for _, cap := range []float64{0, 0.2} {
			req.Guard = &Guard{MaxUSD: cap, MaxCalls: 6}
			llm.planCalls = 0
			m.searches = nil
			if _, err := (&Runtime{LLM: llm, Memory: plainMemory{m}, Tools: paidSearch(fee)}).Answer(context.Background(), req); err != nil {
				t.Fatal(err)
			}
			if len(m.searches) != 1 {
				t.Fatalf("cap %v: %d searches", cap, len(m.searches))
			}
			if got, want := m.searches[0].RemainingUSD, 0.05-planCost-fee; math.Abs(got-want) > 1e-12 {
				t.Fatalf("cap %v: told %v, want %v", cap, got, want)
			}
		}
	})
}
