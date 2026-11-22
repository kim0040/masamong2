package chat

import (
	"context"
	"time"

	"masamong/internal/crash"
	"masamong/internal/memory/search"
)

// earlyMemory is a memory search started from the plan while the tool loop still runs.
type earlyMemory struct {
	intent search.Intent
	cancel context.CancelFunc
	done   chan struct{}
	mem    memoryRun
}

// startEarlyMemory starts the search the plan asked for. Its admission is the one it has at its usual
// place (the money cap only), and with or without a cap it starts once the model call and the paying steps
// in flight have ended (WaitSpent): the loop's first call is what prices the rest of it. spent is what the turn's models cost so far (the planner's): the search's own
// budget counts the loop for what it cost or, while the loop runs, for what its rounds can still cost
// (earlyRemaining).
func (rt *Runtime) startEarlyMemory(ctx context.Context, req Request, msg string, intent search.Intent, spent float64, loop *loopRun, nowT time.Time, sp *spanLog) *earlyMemory {
	ctx, cancel := context.WithCancel(ctx)
	e := &earlyMemory{intent: intent, cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(e.done)
		defer func() {
			// searchMemory recovers the search itself; this covers the admission and budget steps before it,
			// which also run on this goroutine, out of reach of the turn's recover
			if p := recover(); p != nil {
				e.mem = memoryRun{ran: true, intent: intent, err: crash.Error(nil, "chat early memory", p)}
			}
		}()
		if req.Guard.AllowMoneyContext(ctx, "memory.search") {
			// a model call or a fee still on its way is part of what the search may count on being spent, and the
			// first call of the loop is the only thing the turn can price the rest of the loop by. The tools of a
			// second plan (a loop that failed, shadow mode) are not decided yet, and are not counted.
			req.Guard.WaitSpent(ctx)
			remaining := memoryBudget(ctx, req.Guard, func() float64 { return earlyRemaining(req, spent, loop) })
			defer sp.begin("memory")()
			e.mem = rt.searchMemory(ctx, req, msg, intent, remaining, nowT)
		}
	}()
	return e
}

// loopPriceKnown reports whether the loop's rounds can be priced from what the turn has paid for models so
// far: they are asked of the planner's model, whose calls the turn has already seen. chat.tool_model set to
// another model has a price nothing known to the turn bounds, so a search beside such a loop would be told
// a budget that cannot be shown to be no larger than the one it would have after the loop; it starts
// after the loop, as before.
func loopPriceKnown(req Request) bool {
	return req.LoopModel == "" || req.LoopModel == req.PlannerModel
}

// feeAllowanceUSD is what a round of the loop that is still to come is counted to pay a service
// (ToolInfo.Pays) in earlyRemaining: a web search costs about 0.007 (Exa's costDollars), rounded up. A fee
// that is not known yet is not known; this is the estimate.
const feeAllowanceUSD = 0.01

// payingTools reports whether the set offers a tool that pays for itself (other than the memory search).
func payingTools(ts Toolset) bool {
	for _, t := range ts.Enabled {
		if t.Pays && t.ID != "memory.search" {
			return true
		}
	}
	return false
}

// earlyRemaining is what a memory search that starts beside the tool loop may count on having left: the
// budget less what the turn's models and the fees of its tools have cost (the search after the loop is
// told the same, less what the loop then also costs). A loop that has ended counts for what it cost. One
// that still runs counts for all of its rounds, each at the price of the dearest model call the turn has
// had so far (at least the planner's) plus, when the toolset has a tool that pays for itself, a fee
// allowance, and for a second planner call, which a loop that fails brings. Nothing better is known of a
// call that has not been made, so the estimate errs towards less.
func earlyRemaining(req Request, planCost float64, loop *loopRun) float64 {
	ahead := loop.costAhead(max(planCost, req.Guard.MaxCall()))
	if loop.running() {
		ahead += planCost // the planner asked again when the loop fails
		if payingTools(req.Tools) {
			ahead += MaxLoopRounds * feeAllowanceUSD
		}
	}
	return req.BudgetUSD - planCost - feesSoFar(req.Guard) - ahead
}

// wait is the search's result, once it has ended.
func (e *earlyMemory) wait() memoryRun {
	<-e.done
	e.cancel()
	return e.mem
}

// discard gives the search up and waits for it to end. It returns what it spent by then, also added to the
// guard's spending (not its call count). A nil search is no search.
func (e *earlyMemory) discard(g *Guard) float64 {
	if e == nil {
		return 0
	}
	e.cancel()
	mem := e.wait()
	if !mem.ran {
		return 0
	}
	rerank, _ := mem.sr.Trace["rerank_cost_usd"].(float64)
	g.Spend(rerank) // its cost only: a search nobody used takes no call from the tools still to come
	return rerank + mem.firstRerank
}

// warmRun is the embedding of the turn's message started ahead of the plan (QueryWarmer).
type warmRun struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// startWarm starts it where the turn's memory search may run: memory on for the channel and a room to
// search, the conditions the search itself has. The message goes to the embedding provider the search
// would send it to, also on a turn whose plan then searches nothing.
func (rt *Runtime) startWarm(ctx context.Context, req Request, msg string) *warmRun {
	warmer, ok := rt.Memory.(QueryWarmer)
	if !ok || !req.MemoryOn || len(req.Rooms) == 0 {
		return nil
	}
	ctx, cancel := context.WithCancel(ctx)
	w := &warmRun{cancel: cancel, done: make(chan struct{})}
	go func() {
		defer close(w.done)
		defer func() {
			if p := recover(); p != nil { // it runs on its own goroutine, out of reach of the turn's recover
				_ = crash.Error(nil, "chat query embedding", p)
			}
		}()
		warmer.WarmQuery(ctx, msg)
	}()
	return w
}

// stop gives the embedding up without waiting for it; a search that joined it makes its own.
func (w *warmRun) stop() {
	if w != nil {
		w.cancel()
	}
}

// wait gives it up and returns when it has ended, so nothing of the turn runs after the turn.
func (w *warmRun) wait() {
	if w != nil {
		w.cancel()
		<-w.done
	}
}
