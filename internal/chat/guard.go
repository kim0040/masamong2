package chat

import (
	"context"
	"sync"

	"masamong/internal/crash"
)

// Guard caps the extra work of one turn (limits.per_request_budget_usd, limits.max_external_attempts).
// Models reserve a call at start (Begin) and add cost at completion; tools and memory searches are
// noted with what they cost. Calls that
// make the reply itself (the intent and answer models, and the other chat model standing in when one
// fails) always run: a turn that already spent its share still answers. The optional work in between
// (each planned tool, the memory search) is skipped once the turn has spent MaxUSD or made MaxCalls
// external calls, and the answer is told the tool was not run.
//
// The cap is checked before a step with what was spent so far, so a single step can take a turn past
// it (a picture costs more than a typical turn's share); what comes after it is then skipped. Steps
// that run at once (runSteps) are checked in model order. Under a money cap admission waits for all
// models in flight to finish, following cancellation. A step that pays for
// itself (ToolInfo.Pays) is waited for before the steps after it are checked, so the money cap sees
// what it spent as it did when the steps ran one after another.
//
// A paying step whose fee has a known bound (ToolInfo.MaxFeeUSD) holds that bound instead
// (AllowHoldingContext): the steps after it count the hold as spent and do not wait for it, so several
// web searches run at once. A step that the holds alone would refuse waits until one is released and is
// then checked against what was really spent. Since a bound is never less than the fee, the steps
// allowed are exactly the ones allowed when the steps ran one after another.
type Guard struct {
	MaxUSD   float64 // 0: no money cap
	MaxCalls int     // 0: no call cap

	mu      sync.Mutex
	spent   float64
	calls   int
	models  int
	changed chan struct{}
	skipped []string
	paid    float64 // what tools paid for that no model-call total holds (a web search), see Paid
	paying  int     // steps that pay for themselves (ToolInfo.Pays) under way: their fee is not in spent yet
	held    float64 // the bounds held by steps under way (AllowHoldingContext): their fees are not in spent yet
	holds   int     // how many steps hold a bound
	maxCall float64 // the dearest model call that has ended so far
	parent  *Guard  // a step's own guard (child) passes everything on to the turn's
}

// Begin reserves a model call at its start. The returned end adds its final cost exactly once,
// including when a failed or panicking call reports no cost. Optional work sees calls in flight.
func (g *Guard) Begin() func(float64) {
	if g == nil {
		return func(float64) {}
	}
	g.mu.Lock()
	g.calls++
	g.models++
	g.mu.Unlock()
	parentEnd := g.parent.Begin()
	var once sync.Once
	return func(usd float64) {
		once.Do(func() {
			g.mu.Lock()
			g.spent += usd
			g.maxCall = max(g.maxCall, usd)
			g.models--
			if g.changed != nil {
				close(g.changed)
				g.changed = nil
			}
			g.mu.Unlock()
			parentEnd(usd)
		})
	}
}

// BeginPaid says that a step which pays for itself (ToolInfo.Pays) is under way: the fee it adds to the
// guard comes when it ends, so what was spent is not final before that. The returned end must be called
// once the step is over, whatever came of it. Under a money cap admission waits for it as for a model.
func (g *Guard) BeginPaid() func() {
	if g == nil {
		return func() {}
	}
	g.mu.Lock()
	g.paying++
	g.mu.Unlock()
	parentEnd := g.parent.BeginPaid()
	var once sync.Once
	return func() {
		once.Do(func() {
			g.mu.Lock()
			g.paying--
			if g.changed != nil {
				close(g.changed)
				g.changed = nil
			}
			g.mu.Unlock()
			parentEnd()
		})
	}
}

// WaitSpent returns when no model call and no step that pays for itself is under way, or ctx ends: from then
// on what the turn has been charged so far is known (PaidTotal, MaxCall), and a search can be told a budget
// that counts it. Without a money cap AllowMoneyContext does not wait for them, so this does. A step that
// holds a bound (AllowHoldingContext) is not waited for: HeldTotal counts it.
func (g *Guard) WaitSpent(ctx context.Context) {
	if g == nil {
		return
	}
	for ; g.parent != nil; g = g.parent {
	}
	for {
		g.mu.Lock()
		if (g.paying == 0 && g.models == 0) || ctx.Err() != nil {
			g.mu.Unlock()
			return
		}
		if g.changed == nil {
			g.changed = make(chan struct{})
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return
		}
	}
}

// MaxCall is the cost of the dearest model call of the turn that has ended so far: what the next call of
// the same model can be expected to cost at the most, when nothing else is known about it.
func (g *Guard) MaxCall() float64 {
	if g == nil {
		return 0
	}
	for ; g.parent != nil; g = g.parent {
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.maxCall
}

// AllowContext waits for model costs under a money cap, then checks the optional step's limits.
// Waiting follows the turn context; cancellation is not a budget refusal.
func (g *Guard) AllowContext(ctx context.Context, step string) bool {
	ok, _ := g.allowContext(ctx, step, true, false, 0)
	return ok
}

// AllowPayingContext is AllowContext for a step that may pay for itself (ToolInfo.Pays). When it is
// allowed it is under way in the same breath (BeginPaid), so a step that looks at the money in between
// cannot miss its fee; end must be called once the step is over. end is never nil.
func (g *Guard) AllowPayingContext(ctx context.Context, step string, pays bool) (ok bool, end func()) {
	return g.allowContext(ctx, step, true, pays, 0)
}

// AllowHoldingContext is AllowPayingContext for a step that pays at most maxUSD (ToolInfo.MaxFeeUSD > 0).
// Allowed, it holds maxUSD until release: the steps checked after it count the hold as spent instead of
// waiting for its fee, so it runs beside them and makes no one wait (it is not under way the way a
// BeginPaid step is). The step still reports what it really paid with Spend and Paid. release must be
// called once the step is over, whatever came of it (cancelled, failed or panicked); a second call does
// nothing. release is never nil.
func (g *Guard) AllowHoldingContext(ctx context.Context, step string, maxUSD float64) (ok bool, release func()) {
	return g.allowContext(ctx, step, true, false, max(maxUSD, 0))
}

// WaitHeld returns when no step holds a bound any more (AllowHoldingContext), or ctx ends: from then on
// what they paid is in PaidTotal.
func (g *Guard) WaitHeld(ctx context.Context) {
	if g == nil {
		return
	}
	for ; g.parent != nil; g = g.parent {
	}
	for {
		g.mu.Lock()
		if g.holds == 0 || ctx.Err() != nil {
			g.mu.Unlock()
			return
		}
		if g.changed == nil {
			g.changed = make(chan struct{})
		}
		changed := g.changed
		g.mu.Unlock()
		select {
		case <-changed:
		case <-ctx.Done():
			return
		}
	}
}

// HeldTotal is what the steps under way hold of the turn's money (AllowHoldingContext): fees on their way
// that PaidTotal does not have yet. A step adds its fee before it lets go of its hold, so a caller that
// reads HeldTotal before PaidTotal counts every fee at least once.
func (g *Guard) HeldTotal() float64 {
	if g == nil {
		return 0
	}
	for ; g.parent != nil; g = g.parent {
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.held
}

// AllowMoneyContext is AllowContext for a step the call count does not hold back: only the money cap
// applies. The memory search is one: its own calls (embedding, rerank, the one retry without a time
// window) are few and fixed, and a turn that spent its calls on the planner and web searches would
// otherwise never reach the memory the planner asked for.
func (g *Guard) AllowMoneyContext(ctx context.Context, step string) bool {
	ok, _ := g.allowContext(ctx, step, false, false, 0)
	return ok
}

// allowContext checks one step. pays makes an allowed step count as under way (paying); hold > 0 makes
// it hold that much instead (held).
func (g *Guard) allowContext(ctx context.Context, step string, countCalls, pays bool, hold float64) (bool, func()) {
	noEnd := func() {}
	if g == nil {
		return ctx.Err() == nil, noEnd
	}
	if g.parent != nil {
		return g.parent.allowContext(ctx, step, countCalls, pays, hold)
	}
	for {
		g.mu.Lock()
		if ctx.Err() != nil {
			g.mu.Unlock()
			return false, noEnd
		}
		// a fee or a model cost still to come is waited for; so are the holds when they alone would refuse
		// the step: once one is let go, its real fee may leave room, as it would have one step after another
		if g.MaxUSD > 0 && (g.models > 0 || g.paying > 0 || g.holds > 0 && g.spent < g.MaxUSD && g.spent+g.held >= g.MaxUSD) {
			if g.changed == nil {
				g.changed = make(chan struct{})
			}
			changed := g.changed
			g.mu.Unlock()
			select {
			case <-changed:
				continue
			case <-ctx.Done():
				return false, noEnd
			}
		}
		allowed := g.allowLocked(step, countCalls)
		end := noEnd
		switch {
		case allowed && pays:
			g.paying++
			var once sync.Once
			end = func() {
				once.Do(func() {
					g.mu.Lock()
					g.paying--
					g.changedLocked()
					g.mu.Unlock()
				})
			}
		case allowed && hold > 0:
			g.held += hold
			g.holds++
			var once sync.Once
			end = func() {
				once.Do(func() {
					g.mu.Lock()
					g.held -= hold
					if g.holds--; g.holds == 0 {
						g.held = 0 // no rounding left behind
					}
					g.changedLocked()
					g.mu.Unlock()
				})
			}
		}
		g.mu.Unlock()
		return allowed, end
	}
}

// changedLocked wakes the admissions and WaitSpent calls that wait for something under way to end.
func (g *Guard) changedLocked() {
	if g.changed != nil {
		close(g.changed)
		g.changed = nil
	}
}

// releaseCall returns an optional reservation when cancellation prevented it from starting.
func (g *Guard) releaseCall() {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.calls--
	g.mu.Unlock()
	g.parent.releaseCall()
}

// Note records one external call and its cost (0 when the provider reported none).
func (g *Guard) Note(usd float64) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.calls++
	g.spent += usd
	g.mu.Unlock()
	g.parent.Note(usd)
}

// Spend adds a cost to a call already noted (a reranker's share of the memory search).
func (g *Guard) Spend(usd float64) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.spent += usd
	g.mu.Unlock()
	g.parent.Spend(usd)
}

// Paid adds what a tool paid a provider for, other than a model call the turn already counts (a web
// search's fee), to the cost of the turn: Result.CostUSD includes PaidTotal. The caller also gives the same
// amount to Spend, which is the cap's view of it; Paid is only the turn's cost, so nothing is counted twice.
func (g *Guard) Paid(usd float64) {
	if g == nil {
		return
	}
	g.mu.Lock()
	g.paid += usd
	g.mu.Unlock()
	g.parent.Paid(usd)
}

// PaidTotal is what the turn's tools paid for (Paid).
func (g *Guard) PaidTotal() float64 {
	if g == nil {
		return 0
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.paid
}

// child is the guard of one step that runs beside others: what the step notes and spends reaches the
// turn's guard too, and the child's own Totals are only the step's, so the call ledger can give each
// tool what it spent. Its limits are the turn's.
func (g *Guard) child() *Guard {
	if g == nil {
		return nil
	}
	return &Guard{parent: g}
}

// capsMoney reports whether what a step spends can change what the steps after it are allowed.
func (g *Guard) capsMoney() bool {
	for ; g != nil; g = g.parent {
		if g.MaxUSD > 0 {
			return true
		}
	}
	return false
}

// Allow reports whether an optional step may still run; a refused step is remembered by name.
func (g *Guard) Allow(step string) bool {
	if g == nil {
		return true
	}
	if g.parent != nil {
		return g.parent.Allow(step)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.allowLocked(step, true)
}

// allowLocked refuses a step once the turn has spent its money, the bounds held by the steps under way
// counted as spent, or made its calls.
func (g *Guard) allowLocked(step string, countCalls bool) bool {
	if (g.MaxUSD > 0 && g.spent+g.held >= g.MaxUSD) || (countCalls && g.MaxCalls > 0 && g.calls >= g.MaxCalls) {
		g.skipped = append(g.skipped, step)
		return false
	}
	return true
}

// Totals are the turn's external calls and spending so far, and the steps the cap skipped (a child's:
// its own step's calls and spending only).
func (g *Guard) Totals() (calls int, usd float64, skipped []string) {
	if g == nil {
		return 0, 0, nil
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.calls, g.spent, append([]string(nil), g.skipped...)
}

type guardKey struct{}

// WithGuard carries a turn's guard to the calls made for it (model adapters, tools).
func WithGuard(ctx context.Context, g *Guard) context.Context {
	if g == nil {
		return ctx
	}
	return context.WithValue(ctx, guardKey{}, g)
}

// GuardFrom is the guard of the turn ctx belongs to, or nil outside a turn.
func GuardFrom(ctx context.Context) *Guard {
	g, _ := ctx.Value(guardKey{}).(*Guard)
	return g
}

// ToolParallel is how many of a turn's optional steps (the tool calls of one loop round, or the
// planner's tools and the memory search) run at once. Calls of the same tool run in model order, except
// those of a tool that may run beside itself (ToolInfo.Parallel: several web searches of one round).
// Hosts with tight limits also use the process governor (providers.Client.WithProviderGate).
const ToolParallel = 4

// stepKey is what a tool call waits on in runSteps: its predecessor of the same tool, or nothing for a
// tool whose calls may run at once.
func stepKey(info ToolInfo, tool string) string {
	if info.Parallel {
		return ""
	}
	return tool
}

// admitTool checks one tool call against the turn's limits: a call whose fee has a bound (feeBound) holds
// it (AllowHoldingContext), one that pays otherwise counts as under way (AllowPayingContext). end must be
// called once the call is over; it is never nil.
func admitTool(ctx context.Context, g *Guard, tool string, info ToolInfo, params map[string]any) (ok bool, end func()) {
	if bound := feeBound(info, params); info.Pays && bound > 0 {
		return g.AllowHoldingContext(ctx, tool, bound)
	}
	return g.AllowPayingContext(ctx, tool, info.Pays)
}

// step is one optional step of a turn: decide checks the turn's limits (and notes the call), run does
// the work.
type step struct {
	decide   func() bool
	run      func()
	pays     bool      // spends money of its own (ToolInfo.Pays)
	maxFee   float64   // the most it pays (ToolInfo.MaxFeeUSD); 0 = not known
	hold     *paidHold // set by decide when it made the step count as under way or hold its bound (admitTool)
	key      string    // calls of the same tool wait for their predecessor in model order ("" = none)
	cancel   func()
	reserved bool
}

// paidHold carries the end of a paying step's BeginPaid, or the release of its hold, from its decide to
// runSteps.
type paidHold struct{ end func() }

// runSteps decides steps in model order and runs at most ToolParallel together, serializing each key.
// When the turn has a money cap, a step that pays an unknown fee is waited for before the next step is
// decided; one with a bound holds it instead (Guard), so the allowed calls are never more permissive than
// sequential execution. It follows cancellation and returns when every started step has finished.
func runSteps(ctx context.Context, g *Guard, steps []step) {
	slots := make(chan struct{}, ToolParallel)
	var wg sync.WaitGroup
	last := map[string]<-chan struct{}{}
	for _, s := range steps {
		if ctx.Err() != nil {
			if s.cancel != nil {
				s.cancel()
			}
			continue
		}
		if !s.decide() {
			continue
		}
		previous := last[s.key]
		done := make(chan struct{})
		if s.key != "" {
			last[s.key] = done
		}
		endPaid := func() {}
		switch {
		case s.hold != nil && s.hold.end != nil:
			endPaid = s.hold.end // decide has made it count (or hold) already
		case s.pays:
			endPaid = g.BeginPaid() // its fee is not spent yet: the steps that look at the money wait for it
		}
		cancel := func() {
			endPaid()
			if s.reserved {
				g.releaseCall()
			}
			if s.cancel != nil {
				s.cancel()
			}
		}
		// Acquire first occurrences in model order, so a later call cannot steal an earlier call's
		// execution slot. Repeated tools wait in their own goroutine, leaving other tools free to run.
		if previous == nil {
			select {
			case slots <- struct{}{}:
			case <-ctx.Done():
				cancel()
				close(done)
				continue
			}
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer close(done)
			if previous != nil {
				select {
				case <-previous:
				case <-ctx.Done():
					cancel()
					return
				}
				select {
				case slots <- struct{}{}:
				case <-ctx.Done():
					cancel()
					return
				}
			}
			defer func() { <-slots }()
			defer endPaid()
			if ctx.Err() != nil {
				cancel()
				return
			}
			s.run()
		}()
		if s.pays && s.maxFee <= 0 && g.capsMoney() {
			wg.Wait() // its fee is not known: what it spends decides the steps after it
		}
	}
	wg.Wait()
}

// runTool runs one tool call with its own guard (Guard.child) and turns a panic into a failed result:
// the call runs on its own goroutine, out of reach of the turn's recover.
func runTool(ctx context.Context, g *Guard, run ToolRunner, tool string, params map[string]any) (r ToolResult) {
	defer func() {
		if p := recover(); p != nil {
			_ = crash.Error(nil, "chat tool "+tool, p)
			r = ToolResult{Tool: tool, OK: false, Status: "unavailable"}
		}
	}()
	return run(WithGuard(ctx, g.child()), tool, params)
}
