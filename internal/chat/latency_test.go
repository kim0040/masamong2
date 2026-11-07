package chat

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"masamong/internal/memory/engine"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
)

// The turn's clock in these tests is fake (testing/synctest): sleeps take no real time and the elapsed
// time is exact, so a turn's length says which steps overlapped. A goroutine left behind by Answer fails
// the bubble. The durations are the medians of the operations report on 836 turns: the planner 3.4s, the
// loop's first round 1.9s, a web search 1.65s, the second round about 4s, the memory step 4.2s of which
// the query embedding is 3.7s, and the answer 2.3s.
const (
	fakePlanner  = 3400 * time.Millisecond
	fakeRound1   = 1900 * time.Millisecond
	fakeSearch   = 1650 * time.Millisecond
	fakeRound2   = 4000 * time.Millisecond
	fakeEmbed    = 3700 * time.Millisecond
	fakeRetrieve = 500 * time.Millisecond // the memory step without the embedding
	fakeAnswer   = 2300 * time.Millisecond
)

const (
	planMemory   = `{"memory_requirement":"required","question_kind":"event","person_targets":[],"semantic_query":"camping","tool_plan":[]}`
	planMemoryB  = `{"memory_requirement":"required","question_kind":"event","person_targets":[],"semantic_query":"hiking","tool_plan":[]}`
	planNoMemory = `{"memory_requirement":"none","question_kind":"other","person_targets":[],"tool_plan":[]}`
)

// sleepCtx waits d or until ctx ends.
func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// latencyLLM takes the time the report measured and reserves its calls on the turn's guard the way the
// application's adapters do (chat.GuardFrom(ctx).Begin()).
type latencyLLM struct {
	mu        sync.Mutex
	plans     []string // one per planner call
	rounds    []LoopReply
	roundTime []time.Duration
	loopErr   error
	loopErrAt time.Duration
	planCost  float64
	loopCost  float64
	planCalls int
}

func (f *latencyLLM) Chat(ctx context.Context, model string, _ []Msg, _ int, _ float64) (string, float64, error) {
	end := GuardFrom(ctx).Begin()
	if model != "planner" {
		defer end(0)
		return "ok", 0, sleepCtx(ctx, fakeAnswer)
	}
	defer end(f.planCost)
	f.mu.Lock()
	plan := f.plans[min(f.planCalls, len(f.plans)-1)]
	f.planCalls++
	f.mu.Unlock()
	return plan, f.planCost, sleepCtx(ctx, fakePlanner)
}

func (f *latencyLLM) ToolsReady(string) bool { return true }

func (f *latencyLLM) ChatTools(ctx context.Context, _ string, _ []LoopMsg, _ []ToolSpec, _ int) (LoopReply, error) {
	end := GuardFrom(ctx).Begin()
	defer end(f.loopCost)
	if f.loopErr != nil {
		_ = sleepCtx(ctx, f.loopErrAt)
		return LoopReply{}, f.loopErr
	}
	f.mu.Lock()
	r := LoopReply{Text: "-"}
	wait := fakeRound1
	if len(f.rounds) > 0 {
		r, f.rounds = f.rounds[0], f.rounds[1:]
	}
	if len(f.roundTime) > 0 {
		wait, f.roundTime = f.roundTime[0], f.roundTime[1:]
	}
	f.mu.Unlock()
	r.CostUSD = f.loopCost
	return r, sleepCtx(ctx, wait)
}

// latencyMemory takes the memory step's time. The query embedding it starts is one call whoever asks
// first, as in engine.Engine: WarmQuery may start it and Search then joins it.
type latencyMemory struct {
	mu       sync.Mutex
	embed    chan struct{}
	embeds   int
	canceled int
	searches []engine.SearchRequest
	warmedAt []time.Duration
	started  time.Time
	cost     float64
	finished bool
}

func (m *latencyMemory) embedOnce(ctx context.Context) error {
	m.mu.Lock()
	if m.embed != nil {
		done := m.embed
		m.mu.Unlock()
		select {
		case <-done:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	done := make(chan struct{})
	m.embed = done
	m.embeds++
	m.mu.Unlock()
	if err := sleepCtx(ctx, fakeEmbed); err != nil {
		m.mu.Lock()
		m.canceled++
		m.embed = nil
		m.mu.Unlock()
		close(done)
		return err
	}
	close(done)
	return nil
}

func (m *latencyMemory) Search(ctx context.Context, req engine.SearchRequest) (search.Result, error) {
	m.mu.Lock()
	m.searches = append(m.searches, req)
	m.mu.Unlock()
	if err := m.embedOnce(ctx); err != nil {
		return search.Result{}, err
	}
	if err := sleepCtx(ctx, fakeRetrieve); err != nil {
		return search.Result{}, err
	}
	m.mu.Lock()
	m.finished = true
	m.mu.Unlock()
	return search.Result{State: retrieval.StateCandidates, Hits: []search.Hit{{Rank: 1, DocID: req.Intent.SemanticQuery, Room: req.Rooms[0], Date: "2026-09-01", Excerpt: "x"}},
		Trace: map[string]any{"rerank_cost_usd": m.cost}}, nil
}

// plainMemory has no WarmQuery: the embedding is part of its Search, as it is for an adapter without one.
type plainMemory struct{ m *latencyMemory }

func (p plainMemory) Search(ctx context.Context, req engine.SearchRequest) (search.Result, error) {
	return p.m.Search(ctx, req)
}

// warmMemory is latencyMemory with the optional WarmQuery port.
type warmMemory struct{ *latencyMemory }

func (w warmMemory) WarmQuery(ctx context.Context, _ string) {
	w.mu.Lock()
	w.warmedAt = append(w.warmedAt, time.Since(w.started))
	w.mu.Unlock()
	_ = w.embedOnce(ctx)
}

func latencyRequest() Request {
	req := loopRequest("what did we say about camping and how is the weather in Busan?", ToolModeLoop)
	req.Rooms = []string{"discord:1:10"}
	// the defaults: limits.per_request_budget_usd and limits.max_external_attempts of the lite profile
	req.Guard = &Guard{MaxUSD: 0.03, MaxCalls: 6}
	req.BudgetUSD = 0.05
	return req
}

func searchRound() []LoopReply {
	return []LoopReply{{Calls: []LoopCall{call("a", "search__web", `{"query":"x"}`)}}, {Text: "-"}}
}

func latencyTools(_ context.Context, tool string, _ map[string]any) ToolResult {
	time.Sleep(fakeSearch)
	return ToolResult{Tool: tool, OK: true, Data: map[string]any{"status": "ok"}}
}

// A turn with a web search and a memory search: the memory step used to start when the tool loop ended;
// it now starts when the planner ends and overlaps the loop.
//
//	before: planner 0-3.4 (round 1 0-1.9, search 3.4-5.05, round 2 5.05-9.05), memory 9.05-13.25, answer 13.25-15.55
//	after:  planner 0-3.4 (round 1 0-1.9, search 3.4-5.05, round 2 5.05-9.05), memory 3.4-7.6, answer 9.05-11.35
func TestMemorySearchOverlapsTheToolLoop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &latencyMemory{started: time.Now()}
		llm := &latencyLLM{plans: []string{planMemory}, rounds: searchRound(), roundTime: []time.Duration{fakeRound1, fakeRound2}}
		started := time.Now()
		res, err := (&Runtime{LLM: llm, Memory: plainMemory{m}, Tools: latencyTools}).Answer(context.Background(), latencyRequest())
		if err != nil {
			t.Fatal(err)
		}
		want := fakePlanner + fakeSearch + fakeRound2 + fakeAnswer
		if got := time.Since(started); got != want {
			t.Fatalf("turn took %v, want %v: the memory step did not overlap the loop (sum of steps %v)", got, want, want+fakeEmbed+fakeRetrieve)
		}
		if res.RagState != "candidates" || len(res.Hits) != 1 || len(res.Tools) != 1 || !res.Tools[0].OK {
			t.Fatalf("state %s hits %d tools %+v", res.RagState, len(res.Hits), res.Tools)
		}
		if got := time.Duration(res.Timing["memory"]) * time.Millisecond; got != fakeEmbed+fakeRetrieve {
			t.Fatalf("memory timing %v", got)
		}
	})
}

// A chat turn that needs memory and no tool: the embedding starts with the turn, so it is nearly done
// when the planner asks for the search.
//
//	before: planner 0-3.4 (round 1 0-1.9), memory 3.4-7.6, answer 7.6-9.9
//	after:  planner 0-3.4 (round 1 0-1.9, embedding 0-3.7), memory 3.4-4.2, answer 4.2-6.5
func TestQueryEmbeddingStartsWithTheTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &latencyMemory{started: time.Now()}
		llm := &latencyLLM{plans: []string{planMemory}}
		started := time.Now()
		res, err := (&Runtime{LLM: llm, Memory: warmMemory{m}}).Answer(context.Background(), latencyRequest())
		if err != nil {
			t.Fatal(err)
		}
		want := fakeEmbed + fakeRetrieve + fakeAnswer
		if got := time.Since(started); got != want {
			t.Fatalf("turn took %v, want %v", got, want)
		}
		if m.embeds != 1 || len(m.searches) != 1 {
			t.Fatalf("%d embeddings and %d searches, want one of each", m.embeds, len(m.searches))
		}
		if len(res.Hits) != 1 {
			t.Fatalf("hits %+v", res.Hits)
		}
	})
}

// A weather question that also wants memory: the tool's one result ends the loop, and the embedding and
// the search overlap it. The tool takes the time of the web search above.
//
//	before: planner 0-3.4 (round 1 0-1.9, weather 3.4-5.05, round 2 5.05-9.05), memory 9.05-13.25, answer 13.25-15.55
//	after:  planner 0-3.4 (round 1 0-1.9, weather 3.4-5.05, embedding 0-3.7), memory 3.4-4.2, answer 5.05-7.35
func TestFinalToolEndsTheLoopBesideTheMemorySearch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &latencyMemory{started: time.Now()}
		llm := &latencyLLM{plans: []string{planMemory}, rounds: []LoopReply{{Calls: []LoopCall{call("a", "weather__query", `{"region":"Busan"}`)}}, {Text: "-"}},
			roundTime: []time.Duration{fakeRound1, fakeRound2}}
		req := latencyRequest()
		req.Message = "camping talk and Busan weather?" // a short message: a long one is asked again (shortAsk)
		for i, ti := range req.Tools.Enabled {
			req.Tools.Enabled[i].Final = ti.ID == "weather.query"
		}
		started := time.Now()
		res, err := (&Runtime{LLM: llm, Memory: warmMemory{m}, Tools: latencyTools}).Answer(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := time.Since(started), fakePlanner+fakeSearch+fakeAnswer; got != want {
			t.Fatalf("turn took %v, want %v", got, want)
		}
		if len(res.Hits) != 1 || res.ToolLoop.Stop != "final" || m.embeds != 1 {
			t.Fatalf("hits %d, trace %+v, embeddings %d", len(res.Hits), res.ToolLoop, m.embeds)
		}
	})
}

// A planner that picks no memory cancels the embedding started ahead of it, at once: the answer does not
// wait for it, and nothing is left running when the turn ends.
func TestPlannerWithoutMemoryCancelsTheEmbedding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &latencyMemory{started: time.Now()}
		llm := &latencyLLM{plans: []string{planNoMemory}}
		req := latencyRequest()
		started := time.Now()
		res, err := (&Runtime{LLM: llm, Memory: warmMemory{m}}).Answer(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		if got, want := time.Since(started), fakePlanner+fakeAnswer; got != want {
			t.Fatalf("turn took %v, want %v", got, want)
		}
		if m.embeds != 1 || m.canceled != 1 || len(m.searches) != 0 {
			t.Fatalf("embeds %d canceled %d searches %d", m.embeds, m.canceled, len(m.searches))
		}
		if res.RagState == "candidates" || len(res.Hits) != 0 {
			t.Fatalf("state %s", res.RagState)
		}
		// the guard's calls: planner, loop round, answer. The embedding spends none of them.
		if calls, _, _ := req.Guard.Totals(); calls != 3 {
			t.Fatalf("guard counted %d calls", calls)
		}
	})
}

// The embedding only starts where the search may run: memory on for the channel, a room to search.
func TestEmbeddingAheadFollowsTheMemoryConditions(t *testing.T) {
	for name, change := range map[string]func(*Request){
		"memory off": func(r *Request) { r.MemoryOn = false },
		"no rooms":   func(r *Request) { r.Rooms = nil },
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := &latencyMemory{started: time.Now()}
				req := latencyRequest()
				change(&req)
				if _, err := (&Runtime{LLM: &latencyLLM{plans: []string{planNoMemory}}, Memory: warmMemory{m}}).Answer(context.Background(), req); err != nil {
					t.Fatal(err)
				}
				if m.embeds != 0 || len(m.warmedAt) != 0 {
					t.Fatalf("embedded %d times", m.embeds)
				}
			})
		})
	}
}

// A failed embedding ahead of the plan changes nothing: the search makes its own.
type failingWarm struct{ *latencyMemory }

func (failingWarm) WarmQuery(context.Context, string) { panic("embedding down") }

func TestFailedEmbeddingAheadIsIgnored(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		m := &latencyMemory{started: time.Now()}
		res, err := (&Runtime{LLM: &latencyLLM{plans: []string{planMemory}}, Memory: failingWarm{m}}).Answer(context.Background(), latencyRequest())
		if err != nil {
			t.Fatal(err)
		}
		if res.RagState != "candidates" || m.embeds != 1 {
			t.Fatalf("state %s embeds %d", res.RagState, m.embeds)
		}
	})
}

// When the loop fails its first round the planner plans again with every tool on its menu. The search
// started from the first plan stands if the second plan wants the same one; if not it is dropped.
func TestReplanKeepsOrDropsTheEarlySearch(t *testing.T) {
	const loopFailsAt = fakePlanner + 1600*time.Millisecond
	cases := []struct {
		name     string
		second   string
		searches []string // semantic queries asked, in order
		hit      string
		state    string
	}{
		{name: "same search", second: planMemory, searches: []string{"camping"}, hit: "camping", state: "candidates"},
		{name: "other search", second: planMemoryB, searches: []string{"camping", "hiking"}, hit: "hiking", state: "candidates"},
		{name: "no search", second: planNoMemory, searches: []string{"camping"}, state: "not_attempted"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				m := &latencyMemory{started: time.Now()}
				llm := &latencyLLM{plans: []string{planMemory, c.second}, loopErr: ErrToolsUnsupported, loopErrAt: loopFailsAt}
				res, err := (&Runtime{LLM: llm, Memory: plainMemory{m}, Tools: latencyTools}).Answer(context.Background(), latencyRequest())
				if err != nil {
					t.Fatal(err)
				}
				var asked []string
				for _, s := range m.searches {
					asked = append(asked, s.Intent.SemanticQuery)
				}
				want := c.searches
				// The first plan's search waits for the model in flight (the money cap). When the loop fails, it
				// and the second planner call race for the moment that frees: a search dropped before it began
				// was never asked. Either order is right, as long as the later plan's search is the one used.
				if c.second != planMemory && (len(asked) == 0 || asked[0] != "camping") {
					want = want[1:]
				}
				if len(asked) != len(want) {
					t.Fatalf("searches %v, want %v", asked, want)
				}
				for i := range asked {
					if asked[i] != want[i] {
						t.Fatalf("searches %v, want %v", asked, want)
					}
				}
				if res.RagState != c.state {
					t.Fatalf("state %q, want %q", res.RagState, c.state)
				}
				if c.hit == "" && len(res.Hits) != 0 || c.hit != "" && (len(res.Hits) != 1 || res.Hits[0].DocID != c.hit) {
					t.Fatalf("hits %+v, want %q", res.Hits, c.hit)
				}
				if llm.planCalls != 2 || res.ToolLoop.Fallback != "unsupported" {
					t.Fatalf("planner calls %d, trace %+v", llm.planCalls, res.ToolLoop)
				}
			})
		})
	}
}

// What the search is told it may spend (its rerank gate) is never more than the turn would have left if
// the search ran after the loop, the way it used to. A finished loop counts for what it cost, one still
// running for what its rounds can cost.
func TestEarlySearchIsNotToldMoreThanIsLeft(t *testing.T) {
	const planCost, loopCost = 0.002, 0.002 // the loop asks the planner's model (chat.tool_model default)
	sequential := func(rounds int) float64 { return 0.05 - planCost - float64(rounds)*loopCost }
	t.Run("loop still running", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			m := &latencyMemory{started: time.Now()}
			llm := &latencyLLM{plans: []string{planMemory}, rounds: searchRound(), roundTime: []time.Duration{fakeRound1, fakeRound2}, planCost: planCost, loopCost: loopCost}
			if _, err := (&Runtime{LLM: llm, Memory: plainMemory{m}, Tools: latencyTools}).Answer(context.Background(), latencyRequest()); err != nil {
				t.Fatal(err)
			}
			if got := m.searches[0].RemainingUSD; got > sequential(MaxLoopRounds)+1e-12 {
				t.Fatalf("told %v, more than %v", got, sequential(MaxLoopRounds))
			}
		})
	})
	t.Run("loop finished", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			m := &latencyMemory{started: time.Now()}
			llm := &latencyLLM{plans: []string{planMemory}, planCost: planCost, loopCost: loopCost}
			if _, err := (&Runtime{LLM: llm, Memory: plainMemory{m}, Tools: latencyTools}).Answer(context.Background(), latencyRequest()); err != nil {
				t.Fatal(err)
			}
			if got := m.searches[0].RemainingUSD; math.Abs(got-sequential(1)) > 1e-12 {
				t.Fatalf("told %v, want %v", got, sequential(1))
			}
		})
	})
}

// The memory search keeps its admission: the call count does not hold it back, the money cap does, and
// the cap waits for the models in flight.
func TestEarlySearchKeepsItsAdmission(t *testing.T) {
	t.Run("call count spent", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			m := &latencyMemory{started: time.Now()}
			llm := &latencyLLM{plans: []string{planMemory}, rounds: searchRound(), roundTime: []time.Duration{fakeRound1, fakeRound2}}
			req := latencyRequest()
			req.Guard = &Guard{MaxCalls: 2} // the planner and the first round
			res, err := (&Runtime{LLM: llm, Memory: plainMemory{m}, Tools: latencyTools}).Answer(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if res.RagState != "candidates" || len(res.Hits) != 1 {
				t.Fatalf("state %s", res.RagState)
			}
		})
	})
	t.Run("money spent", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			m := &latencyMemory{started: time.Now()}
			// the loop's first round outlasts the planner, and what it cost is what takes the turn over the cap
			llm := &latencyLLM{plans: []string{planMemory}, roundTime: []time.Duration{fakePlanner + time.Second}, loopCost: 0.04}
			req := latencyRequest()
			res, err := (&Runtime{LLM: llm, Memory: plainMemory{m}}).Answer(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if len(m.searches) != 0 || res.RagState != "memory_unavailable" {
				t.Fatalf("searches %d state %s", len(m.searches), res.RagState)
			}
			if _, _, skipped := req.Guard.Totals(); len(skipped) != 1 || skipped[0] != "memory.search" {
				t.Fatalf("skipped %v", skipped)
			}
		})
	})
}

// A turn cancelled while its search, its embedding and its loop are running stops them all and leaves
// nothing behind (the bubble fails on a goroutine that never ends).
func TestCancelledTurnStopsTheEarlyWork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		m := &latencyMemory{started: time.Now()}
		llm := &latencyLLM{plans: []string{planMemory}, rounds: searchRound(), roundTime: []time.Duration{fakeRound1, fakeRound2}}
		time.AfterFunc(fakePlanner+100*time.Millisecond, cancel)
		started := time.Now()
		_, err := (&Runtime{LLM: llm, Memory: warmMemory{m}, Tools: latencyTools}).Answer(ctx, latencyRequest())
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err %v", err)
		}
		// the fake web search runs out its 1.65s like a tool that does not look at ctx in time
		if got, most := time.Since(started), fakePlanner+fakeSearch; got > most {
			t.Fatalf("turn took %v after it was cancelled at %v", got, fakePlanner+100*time.Millisecond)
		}
	})
}
