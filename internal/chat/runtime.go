package chat

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"masamong/internal/crash"
	"masamong/internal/errclass"
	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
)

// LLM is the chat model port (model per role is chosen by the caller).
type LLM interface {
	Chat(ctx context.Context, model string, msgs []Msg, maxTokens int, temperature float64) (text string, costUSD float64, err error)
}

// Completion is the part of a model response needed by the turn runtime. FinishReason lets it
// distinguish a naturally finished answer from one cut off by the provider's output limit.
type Completion struct {
	Text         string
	CostUSD      float64
	FinishReason string
}

// DetailedLLM is optional so small test and alternate adapters can keep implementing LLM only.
type DetailedLLM interface {
	ChatDetailed(ctx context.Context, model string, msgs []Msg, maxTokens int, temperature float64) (Completion, error)
}

// Memory is the channel memory port.
type Memory interface {
	Search(ctx context.Context, req engine.SearchRequest) (search.Result, error)
}

// QueryWarmer is optional on a Memory: it starts the embedding of a query the turn's memory search may
// ask for, so the search finds it done or joins it. It spends none of the turn's calls, keeps what it
// paid on the books and fails silently (the search makes its own attempt). Canceling ctx gives it up.
type QueryWarmer interface {
	WarmQuery(ctx context.Context, query string)
}

// ToolRunner executes one product Action for the turn (never memory.search).
type ToolRunner func(ctx context.Context, tool string, params map[string]any) ToolResult

// Request is one incoming message to answer.
type Request struct {
	Message       string
	Speaker       string
	History       []Turn
	RecentRoom    []corpus.Message
	Persona       string
	Rules         string
	AgentPrompt   string
	Rooms         []string // memory scope (channel room + attached blocks)
	MemoryOn      bool
	Voice         Voice   // language pack + bot name for every prompt of the turn
	Tools         Toolset // tools the planner may use (memory.search entry optional: its text)
	Ranking       retrieval.RankingMode
	PlannerModel  string
	LoopModel     string // the role the tool loop asks ("" = PlannerModel)
	ResponseModel string
	Now           time.Time
	TZ            *time.Location
	BudgetUSD     float64
	// Earlier is what a tool the person ran themselves ("!날씨", /weather) gave when it had no answer.
	// The turn starts from it: it reaches the answer as is and is left out of the planner's menu, so the
	// planner picks something else (a quote that failed → a web search) or nothing, instead of repeating
	// the failure.
	Earlier []ToolResult
	// Guard caps the turn's optional work by what it spent (see Guard); nil means no cap.
	Guard *Guard
	// MaxHistory is how many History turns the answer sees (0 = 8). A caller that already chose the
	// conversation's lines and budgeted them (application callerConversation) passes more.
	MaxHistory int
	// ToolMode is config chat.tool_mode: planner ("" too), shadow or loop (toolloop.go).
	ToolMode string
	// LoopFinal is config chat.loop_final: when the loop stops without asking once more (toolloop.go).
	LoopFinal string
	// RoomMemory is what the room remembers that bears on this turn (roommemory.go); nil for none.
	RoomMemory *RoomMemory
	// Images are the pictures the member called the bot with, for an answer model that takes them.
	// They go with the answer's last user message only: the planner and the tool loop read the text,
	// where the caller put a tag saying a picture came with it.
	Images []Image
	// TurnStart is when the turn began (the application's ChatTurn), the zero point of Result.Spans; the
	// zero time counts from the start of Answer.
	TurnStart time.Time
}

// Result is the answer plus a body-free trace for logs/diagnostics.
type Result struct {
	Answer   string         `json:"answer"`
	Hits     []search.Hit   `json:"hits"`
	RagState string         `json:"rag_state"`
	Plan     Plan           `json:"plan"`
	Assess   Assessment     `json:"assessment"`
	Intent   search.Intent  `json:"intent"`
	Tools    []ToolResult   `json:"tools"`
	Retrieve map[string]any `json:"retrieval_trace,omitempty"`
	Timing   map[string]int `json:"timing_ms"`
	CostUSD  float64        `json:"cost_usd"`
	// ScopeDropped counts memory hits from rooms outside Request.Rooms that were removed before the
	// prompt. Search already keeps to those rooms, so anything above zero is a defect to report.
	ScopeDropped int `json:"scope_dropped,omitempty"`
	// TurnID is the turn record of this answer (set by the application), for feedback on the reply
	TurnID string `json:"turn_id,omitempty"`
	// Skipped names the steps the turn's Guard did not run (tool ids, "memory.search").
	Skipped []string `json:"skipped,omitempty"`
	// ToolLoop is what the native tool loop did (tool modes loop and shadow); nil in planner mode.
	ToolLoop *LoopTrace `json:"tool_loop,omitempty"`
	// Room is the body-free record of the room memory the answer was given; nil when none.
	Room *RoomTrace `json:"room_memory,omitempty"`
	// AnswerRetry is why the answer was asked a second time with less context (context_length: the
	// prompt was longer than the model takes); "" when it was asked once.
	AnswerRetry string `json:"answer_retry,omitempty"`
	// AnswerLoop marks an answer cut because it kept repeating itself (loopguard.go)
	AnswerLoop bool `json:"answer_loop,omitempty"`
	// Spans are the stretches that ran at once (planner, tool loop, tools, memory search, answer) in
	// milliseconds from the turn's start, and WaitedOn says which side of the planner/tool loop join was
	// still running when the other was done ("loop" | "planner"; "" with no loop). Timing sums the steps;
	// these show where the turn actually waited.
	Spans    []Span `json:"spans,omitempty"`
	WaitedOn string `json:"waited_on,omitempty"`
}

// Runtime answers turns.
type Runtime struct {
	LLM    LLM
	Memory Memory
	Tools  ToolRunner
}

func ms(t time.Time) int { return int(time.Since(t).Milliseconds()) }

// ErrEmptyMessage is returned for a blank message.
var ErrEmptyMessage = errors.New("chat: empty message")

// Answer runs planner → tools → memory → answer. A planner failure degrades to answering without
// memory (never "search everything"); a memory failure degrades to answering without memories.
func (rt *Runtime) Answer(ctx context.Context, req Request) (Result, error) {
	msg := strings.TrimSpace(req.Message)
	if msg == "" {
		return Result{}, ErrEmptyMessage
	}
	res := Result{Timing: map[string]int{}, RagState: string(retrieval.StateNotAttempted)}
	sp := newSpanLog(req.TurnStart)
	ctx = WithGuard(ctx, req.Guard)
	tz := req.TZ
	if tz == nil {
		tz = time.UTC
	}
	nowT := req.Now
	if nowT.IsZero() {
		nowT = time.Now()
	}
	v := req.Voice
	local := nowT.In(tz)
	weekday := local.Weekday().String()
	if w := v.pack().Chat.Weekdays; len(w) == 7 {
		weekday = w[local.Weekday()]
	}
	timeRef := lang.Fill(v.pack().Chat.LiveTime, "time", local.Format(time.RFC3339), "weekday", weekday, "tz", tz.String(),
		"days", dayOffsets(local, v.pack().Chat.Weekdays))
	// The planner sees memory.search only when memory is on for this channel.
	memTool, hasMem := req.Tools.find("memory.search")
	if !hasMem {
		memTool = ToolInfo{ID: "memory.search", Description: v.pack().Tools["memory.search"].Description}
	}
	others := req.Tools.without("memory.search")
	for _, e := range req.Earlier {
		others = others.without(e.Tool)
	}
	warm := rt.startWarm(ctx, req, msg)
	defer warm.wait()
	// The tool loop (modes loop and shadow) runs beside the planner. In loop mode it chooses the tools,
	// and the planner's menu shrinks to memory.search: listing only the tools that are on is how the
	// planner prompt already varies, so its text stays the evaluated one.
	loopStart := time.Now()
	loop, plannerTools := rt.startLoop(ctx, req, msg, timeRef, others)
	planOnce := func(tools Toolset) string {
		menu := tools
		if req.MemoryOn {
			menu = Toolset{Enabled: append([]ToolInfo{memTool}, tools.Enabled...), Known: tools.Known}
		}
		t0 := time.Now()
		planRaw, cost, err := rt.LLM.Chat(ctx, req.PlannerModel, BuildPlannerMessages(v, msg, req.History, "", timeRef, menu), PlannerMaxTokens, 0)
		res.CostUSD += cost
		res.Timing["plan"] += ms(t0)
		sp.add("planner", t0, time.Now())
		if err != nil {
			return ""
		}
		return planRaw
	}
	readPlan := func(raw string, tools Toolset) (Plan, Assessment, search.Intent) {
		plan := ParsePlan(raw)
		assessed := Assess(v, plan, tools, req.MemoryOn, true)
		planned, _ := plan.Intent()
		if assessed.Time != nil {
			planned.Time = assessed.Time
		}
		return plan, assessed, planned
	}
	planRaw := planOnce(plannerTools)
	plannerEnd := time.Now()
	plan, assessed, intent := readPlan(planRaw, plannerTools)
	canSearch := rt.Memory != nil && len(req.Rooms) > 0
	if assessed.MemoryState != "search" || !canSearch {
		warm.stop() // a plan made again later searches without it
	}
	// the plan is all the memory search needs: while the loop still runs it starts now, beside the loop
	var early *earlyMemory
	defer func() { early.discard(req.Guard) }()
	if assessed.MemoryState == "search" && canSearch && loop.running() && loopPriceKnown(req) {
		early = rt.startEarlyMemory(ctx, req, msg, intent, res.CostUSD, loop, nowT, sp)
	}
	loopTools, loopRan := loop.wait()
	res.WaitedOn = sp.joinLoop(loop, loopStart, plannerEnd)
	if loop != nil {
		res.ToolLoop = loop.trace
		res.CostUSD += loop.trace.CostUSD
		res.Timing["tool_loop"] = loop.trace.Ms
		if loop.err != nil && req.ToolMode == ToolModeLoop {
			// the model takes no tools (or failed): the planner chooses the tools of this turn
			plannerTools = others
			planRaw = planOnce(plannerTools)
			plan, assessed, intent = readPlan(planRaw, plannerTools)
			if early != nil && (assessed.MemoryState != "search" || !reflect.DeepEqual(intent, early.intent)) {
				// the second plan wants another search or none: what the first one found is not used
				res.CostUSD += early.discard(req.Guard)
				early = nil
			}
		}
	}
	res.Plan, res.Assess = plan, assessed
	if !res.Plan.Parsed {
		slog.Default().WarnContext(ctx, "chat: planner response rejected", "parse_state", res.Plan.ParseState)
	}
	res.Intent = intent
	t1 := time.Now()
	res.Tools = append(res.Tools, req.Earlier...)
	if loopRan {
		res.Tools = append(res.Tools, loopTools...)
	}
	// the planner's tools and the memory search run at once (runSteps): each is checked against the
	// turn's limits in plan order (the memory search last, unless it started ahead of the loop's end);
	// results keep the plan's order
	planned := make([]*ToolResult, len(res.Assess.Steps))
	var steps []step
	var toolsEnd time.Time
	var endMu sync.Mutex
	for i, st := range res.Assess.Steps {
		switch st.State {
		case "runnable":
			if rt.Tools == nil {
				break
			}
			info, _ := req.Tools.find(st.Tool)
			cancel := func() { planned[i] = &ToolResult{Tool: st.Tool, Status: "cancelled"} }
			hold := &paidHold{}
			steps = append(steps, step{key: st.Tool, reserved: true, cancel: cancel, pays: info.Pays, hold: hold, decide: func() bool {
				allowed, endPaid := req.Guard.AllowPayingContext(ctx, st.Tool, info.Pays)
				hold.end = endPaid
				if !allowed {
					if ctx.Err() != nil {
						cancel()
						return false
					}
					// the turn spent its share: the answer says it could not check, as for a tool that is down
					planned[i] = &ToolResult{Tool: st.Tool, OK: false, Status: "unavailable"}
					return false
				}
				req.Guard.Note(0) // what the tool pays for is added by the tool (Guard.Spend)
				return true
			}, run: func() {
				defer sp.begin("tool:" + st.Tool)()
				r := runTool(ctx, req.Guard, rt.Tools, st.Tool, st.Parameters)
				planned[i] = &r
				endMu.Lock()
				toolsEnd = time.Now()
				endMu.Unlock()
			}})
		case "needs_input":
			planned[i] = &ToolResult{Tool: st.Tool, OK: false, Status: "needs_input", Error: "missing: " + strings.Join(st.Missing, ",")}
		}
	}
	searchMemory := res.Assess.MemoryState == "search" && canSearch
	var mem memoryRun
	if searchMemory && early == nil {
		spent := res.CostUSD
		steps = append(steps, step{decide: func() bool { return req.Guard.AllowMoneyContext(ctx, "memory.search") },
			run: func() {
				defer sp.begin("memory")()
				// the fees of the steps decided before this one count too, whether they have ended yet or not
				req.Guard.WaitSpent(ctx)
				mem = rt.searchMemory(ctx, req, msg, intent, req.BudgetUSD-spent-req.Guard.PaidTotal(), nowT)
			}})
	}
	runSteps(ctx, req.Guard, steps)
	if early != nil {
		mem = early.wait()
		early = nil
	}
	warm.stop()
	for _, r := range planned {
		if r != nil {
			res.Tools = append(res.Tools, *r)
		}
	}
	res.Timing["tools"] = int(max(toolsEnd.Sub(t1), 0).Milliseconds())
	switch res.Assess.MemoryState {
	case "search":
		if !searchMemory || !mem.ran {
			res.RagState = "memory_unavailable"
			break
		}
		sr, err, relaxed, firstRerank := mem.sr, mem.err, mem.relaxed, mem.firstRerank
		if relaxed {
			res.CostUSD += firstRerank
			intent = mem.intent // the answer is not told "recent" about memory found outside the window
		}
		res.Timing["memory"] = mem.ms
		if err != nil {
			res.RagState = "memory_unavailable"
			res.Retrieve = map[string]any{"error": errKind(err)}
			break
		}
		res.Hits, res.RagState = sr.Hits, string(sr.State)
		if kept, dropped := inScope(res.Hits, req.Rooms); dropped > 0 {
			res.Hits, res.ScopeDropped = kept, dropped
			if len(kept) == 0 {
				res.RagState = "empty_in_allowed_scope"
			}
		}
		res.Retrieve = compactTrace(sr.Trace)
		rerank, _ := sr.Trace["rerank_cost_usd"].(float64)
		if relaxed {
			res.Retrieve["time_relaxed"] = true
			res.Retrieve["rerank_cost_usd"] = firstRerank + rerank // both searches (the first is already counted)
		}
		res.CostUSD += rerank
		req.Guard.Note(rerank)
	case "blocked_disabled":
		res.RagState = "memory_search_disabled"
	}
	t3 := time.Now()
	maxHistory := req.MaxHistory
	if maxHistory <= 0 {
		maxHistory = 8
	}
	searched, searchedState := res.Hits, res.RagState
	roomBlock, kept, roomTrace := renderRoomMemory(v, req.RoomMemory, res.Hits)
	if roomTrace.DroppedHits > 0 {
		res.Hits = kept
		if len(kept) == 0 {
			res.RagState = RagCoveredByRoomMemory
		}
	}
	if !roomTrace.Empty() {
		res.Room = &roomTrace
	}
	system := ComposeSystem(v, req.Persona, req.Rules, req.AgentPrompt)
	msgs := BuildAnswerMessages(v, AnswerInput{
		System: system, History: req.History, MaxHistory: maxHistory,
		Hits: res.Hits, RagState: res.RagState, Tools: res.Tools, Speaker: req.Speaker, Message: msg, Intent: intent,
		RecentRoom: req.RecentRoom, RoomMemory: roomBlock, Now: local,
	})
	withImages(msgs, req.Images)
	answerBudget := answerTokenLimit(msg, v.pack().Tag)
	completion, err := rt.complete(ctx, req.ResponseModel, msgs, answerBudget, 0.2)
	res.CostUSD += completion.CostUSD
	if err != nil && errclass.Of(err) == errclass.ContextLength && ctx.Err() == nil {
		// the prompt is longer than the model takes: once more with less of it. Half the conversation and
		// half the memory hits, no room memory and no ambient chat; the system prompt, the tool results and
		// the member's message stay. The hits the room memory covered come back with their state.
		slog.Default().WarnContext(ctx, "chat: answer prompt too long for the model, retrying once with less context", "reason", string(errclass.ContextLength))
		res.AnswerRetry = string(errclass.ContextLength)
		hist := req.History
		if n := min(maxHistory, len(hist)) / 2; n > 0 {
			hist = hist[len(hist)-n:]
		} else {
			hist = nil
		}
		res.Hits, res.RagState, res.Room = searched[:(len(searched)+1)/2], searchedState, nil
		msgs = BuildAnswerMessages(v, AnswerInput{
			System: system, History: hist, MaxHistory: len(hist),
			Hits: res.Hits, RagState: res.RagState, Tools: res.Tools, Speaker: req.Speaker, Message: msg, Intent: intent, Now: local,
		})
		withImages(msgs, req.Images)
		completion, err = rt.complete(ctx, req.ResponseModel, msgs, answerBudget, 0.2)
		res.CostUSD += completion.CostUSD
	}
	// the answer's time ends with the last call that makes it, the one that goes on with a cut-off answer
	// included; what the tools and the query embedding were charged is final by then
	finish := func() {
		res.Timing["answer"] = ms(t3)
		sp.add("answer", t3, time.Now())
		res.Spans = sp.spans()
		warm.wait()
		res.CostUSD += req.Guard.PaidTotal() // what the tools paid for (a search's fee) is part of the turn's cost
		_, _, res.Skipped = req.Guard.Totals()
	}
	if err != nil {
		finish()
		return res, err
	}
	answer := strings.TrimSpace(completion.Text)
	if cut, looped := cutLoop(answer); looped {
		// a loop is cut after its first round and not continued: more of it is more of the same
		slog.Default().WarnContext(ctx, "chat: the answer repeated itself; cut after its first round", "runes", utf8.RuneCountInString(answer), "kept", utf8.RuneCountInString(cut))
		answer, res.AnswerLoop = endAtSentence(cut), true
	} else if completion.FinishReason == "length" && answer != "" {
		continued := append([]Msg(nil), msgs...)
		continued = append(continued, Msg{Role: "assistant", Content: answer}, Msg{Role: "user", Content: v.pack().Chat.ContinueAnswer})
		next, continueErr := rt.complete(ctx, req.ResponseModel, continued, answerBudget, 0.2)
		res.CostUSD += next.CostUSD
		continuedText := strings.TrimSpace(next.Text)
		cut, looped := cutLoop(continuedText)
		if looped {
			continuedText, res.AnswerLoop = endAtSentence(cut), true
		}
		if continuedText != "" {
			answer += "\n" + continuedText
		}
		if continueErr != nil || continuedText == "" || next.FinishReason == "length" && !looped {
			answer = cutAtSentenceBoundary(answer) + " " + v.pack().Chat.IncompleteAnswer
		}
	}
	finish()
	res.Answer = strings.TrimSpace(answer)
	return res, nil
}

// memoryRun is what the turn's memory search found, gathered while the planner's tools ran beside it.
type memoryRun struct {
	ran         bool
	sr          search.Result
	err         error
	relaxed     bool          // searched again without the planner's time window
	firstRerank float64       // the reranker's cost of the first search, when it was searched again
	intent      search.Intent // the intent the hits were found with
	ms          int
}

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
		if req.Guard.AllowMoneyContext(ctx, "memory.search") {
			// a model call or a fee still on its way is part of what the search may count on being spent, and the
			// first call of the loop is the only thing the turn can price the rest of the loop by
			req.Guard.WaitSpent(ctx)
			defer sp.begin("memory")()
			e.mem = rt.searchMemory(ctx, req, msg, intent, earlyRemaining(req, spent, loop), nowT)
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
	return req.BudgetUSD - planCost - req.Guard.PaidTotal() - ahead
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

// searchMemory searches the turn's rooms. When the planner's time window finds nothing it searches
// once more without it: the window is often a guess (a year-less date read as this year, "recently" for
// a months-old talk); hits carry their dates.
func (rt *Runtime) searchMemory(ctx context.Context, req Request, msg string, intent search.Intent, remaining float64, nowT time.Time) (out memoryRun) {
	t2 := time.Now()
	out.ran, out.intent = true, intent
	defer func() {
		if p := recover(); p != nil { // the search runs on its own goroutine, out of reach of the turn's recover
			out.err = crash.Error(nil, "chat memory search", p)
		}
		out.ms = ms(t2)
	}()
	out.sr, out.err = rt.Memory.Search(ctx, engine.SearchRequest{Query: msg, Intent: intent, Rooms: req.Rooms, Ranking: req.Ranking,
		RemainingUSD: remaining, Now: nowT})
	sr := out.sr
	if out.err == nil && len(sr.Hits) == 0 && sr.Trace["abstain_reason"] == "empty_in_allowed_scope" && (intent.Recent || (intent.Time != nil && intent.Time.Kind != "" && intent.Time.Kind != "any")) {
		wide := intent
		wide.Time, wide.Recent = nil, false
		first, _ := sr.Trace["rerank_cost_usd"].(float64)
		again, err2 := rt.Memory.Search(ctx, engine.SearchRequest{Query: msg, Intent: wide, Rooms: req.Rooms, Ranking: req.Ranking,
			RemainingUSD: max(0, remaining-first), Now: nowT})
		if err2 == nil {
			req.Guard.Spend(first) // the first search's reranker: its cost counts, it takes no call from the tools
			out.sr, out.relaxed, out.firstRerank, out.intent = again, true, first, wide
		} else {
			slog.Default().WarnContext(ctx, "chat: memory search without the time window failed", "error", errKind(err2))
		}
	}
	return out
}

func (rt *Runtime) complete(ctx context.Context, model string, msgs []Msg, maxTokens int, temperature float64) (Completion, error) {
	if detailed, ok := rt.LLM.(DetailedLLM); ok {
		return detailed.ChatDetailed(ctx, model, msgs, maxTokens, temperature)
	}
	text, cost, err := rt.LLM.Chat(ctx, model, msgs, maxTokens, temperature)
	return Completion{Text: text, CostUSD: cost}, err
}

// withImages puts the turn's pictures on the answer's last message (the member's).
func withImages(msgs []Msg, images []Image) {
	if len(images) > 0 && len(msgs) > 0 {
		msgs[len(msgs)-1].Images = images
	}
}

// endAtSentence is text up to its last sentence end, or all of it when it has none (chat often has none).
func endAtSentence(text string) string {
	if s := cutAtSentenceBoundary(text); s != "" {
		return s
	}
	return strings.TrimSpace(text)
}

func cutAtSentenceBoundary(text string) string {
	text = strings.TrimSpace(text)
	runes := []rune(text)
	last := -1
	for i, r := range runes {
		if strings.ContainsRune(".!?。！？\n", r) {
			last = i
		}
	}
	if last < 0 {
		return ""
	}
	return strings.TrimSpace(string(runes[:last+1]))
}

// inScope keeps the hits whose source room the request allows. Search filters by room already; this is
// the last check before memory reaches the prompt, so a defect there can never put one room's memory in
// another room's answer.
func inScope(hits []search.Hit, rooms []string) ([]search.Hit, int) {
	allowed := make(map[string]bool, len(rooms))
	for _, r := range rooms {
		allowed[r] = true
	}
	kept := hits[:0:0]
	for _, h := range hits {
		if allowed[h.Room] {
			kept = append(kept, h)
		}
	}
	return kept, len(hits) - len(kept)
}

func errKind(err error) string {
	switch {
	case errors.Is(err, engine.ErrNotReady):
		return "index_not_ready"
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout"
	}
	return "error"
}

// compactTrace keeps the retrieval decisions (no bodies) for logs.
func compactTrace(t map[string]any) map[string]any {
	out := map[string]any{}
	for _, k := range []string{"applied", "ranking", "ranking_requested", "ranking_auto_reason", "auto_confidence", "recovery_performed",
		"retrievers_used", "candidate_union_n", "candidate_sources", "queries_used", "persons", "time", "rerank_model", "rerank_min_score",
		"rerank_kept_n", "rerank_cost_usd", "rerank_latency_ms", "abstained", "abstain_reason", "rerank_error"} {
		if v, ok := t[k]; ok {
			out[k] = v
		}
	}
	return out
}

// dayOffsets lists the next seven days with their day_offset ("0=10-01 Thursday, 1=10-02 Friday, …"):
// small planner models get "this weekend" or "next Monday" wrong when they count from a date (the
// scenario runs saw 0/1, 2/3 and 5/6 for the same weekend), and read it right off a list.
// ForecastDays is how many day_offset values the planner is shown: the weather tool's range
// (openmeteo.MaxForecastDays; a test in application keeps the two equal).
const ForecastDays = 14

func dayOffsets(local time.Time, names []string) string {
	parts := make([]string, 0, ForecastDays)
	for i := range ForecastDays {
		d := local.AddDate(0, 0, i)
		name := d.Weekday().String()
		if len(names) == 7 {
			name = names[d.Weekday()]
		}
		parts = append(parts, fmt.Sprintf("%d=%s %s", i, d.Format("01-02"), name))
	}
	return strings.Join(parts, ", ")
}
