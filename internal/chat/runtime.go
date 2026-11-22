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
	// MaxHistory is how many History turns the answer sees (0 = HistoryShown). A caller that already chose the
	// conversation's lines and budgeted them (application callerConversation) passes more, and records all of them as
	// read.
	MaxHistory int
	// ToolMode is config chat.tool_mode: planner ("" too), shadow or loop (toolloop.go).
	ToolMode string
	// LoopFinal is config chat.loop_final: when the loop stops without asking once more (toolloop.go).
	LoopFinal string
	// RoomMemory is what the room remembers that bears on this turn (roommemory.go); nil for none.
	RoomMemory *RoomMemory
	// People resolves the turn's room memory after the planner has named its targets.
	People func(targets []string) *RoomMemory
	// Images are the pictures the member called the bot with, for an answer model that takes them.
	// They go with the answer's last user message only: the planner and the tool loop read the text,
	// where the caller put a tag saying a picture came with it.
	Images []Image
	// TurnStart is when the turn began (the application's ChatTurn), the zero point of Result.Spans; the
	// zero time counts from the start of Answer.
	TurnStart time.Time
	// OnProgress hears how far the turn's web search has got (progress.go), so the connector can say a
	// long turn is still looking. It is called from the loop's or the planner path's goroutine, at most
	// once per stage, never for the shadow loop (which runs nothing), and must not block. nil = nobody
	// listens.
	OnProgress func(Progress)
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
	t := &turn{rt: rt, msg: msg, v: req.Voice, res: Result{Timing: map[string]int{}, RagState: string(retrieval.StateNotAttempted)}}
	t.sp = newSpanLog(req.TurnStart)
	ctx = WithGuard(ctx, req.Guard)
	req.OnProgress = progressOnce(req.OnProgress) // one call per stage across the loop and the planner's steps
	t.req = req
	t.nowT, t.local, t.timeRef = turnTime(req)
	defer t.pf.close(req.Guard)
	p := t.planTurn(ctx)
	planned, mem, searchMemory := t.runPlan(ctx, &p)
	t.readMemory(&p, mem, searchMemory)
	err := t.answer(ctx, &p, planned)
	return t.res, err
}

// turn is one Answer call on its way through its stages: the plan (planTurn), the plan's tools and the
// memory search (runPlan, readMemory) and the answer. Each stage adds to res. The fields other than res
// and pf are set before the first stage and only read after.
type turn struct {
	rt      *Runtime
	req     Request
	msg     string
	v       Voice
	res     Result
	sp      *spanLog
	nowT    time.Time // the turn's time (Request.Now)
	local   time.Time // nowT in the turn's zone
	timeRef string    // the time line the planner and the tool loop are given
	pf      prefetch  // the memory work started ahead of the plan's end; closed when Answer returns
}

// turnPlan is what the plan stage decided beside res.Plan and res.Assess: the memory search's intent
// (made wider by readMemory when the search found nothing in the plan's time window), the tools the loop
// ran and whether they are the turn's tools, and whether the turn has memory to search.
type turnPlan struct {
	intent    search.Intent
	loopTools []ToolResult
	loopRan   bool
	canSearch bool
}

// turnTime is the turn's time (Request.Now, the wall clock when unset), that time in the turn's zone
// (Request.TZ, UTC when unset) and the time line the planner and the tool loop are given.
func turnTime(req Request) (nowT, local time.Time, timeRef string) {
	tz := req.TZ
	if tz == nil {
		tz = time.UTC
	}
	nowT = req.Now
	if nowT.IsZero() {
		nowT = time.Now()
	}
	v := req.Voice
	local = nowT.In(tz)
	weekday := local.Weekday().String()
	if w := v.pack().Chat.Weekdays; len(w) == 7 {
		weekday = w[local.Weekday()]
	}
	timeRef = lang.Fill(v.pack().Chat.LiveTime, "time", local.Format(time.RFC3339), "weekday", weekday, "tz", tz.String(),
		"days", dayOffsets(local, v.pack().Chat.Weekdays))
	return nowT, local, timeRef
}

// planTurn asks the planner, with the tool loop beside it in modes loop and shadow, and asks it once more
// when the loop could not choose the tools. It starts the prefetch: the message's embedding, and the
// memory search of a plan made while the loop still runs. res gets the plan, its cost and the loop's trace.
func (t *turn) planTurn(ctx context.Context) turnPlan {
	req, v := t.req, t.v
	// The planner sees memory.search only when memory is on for this channel.
	memTool, hasMem := req.Tools.find("memory.search")
	if !hasMem {
		memTool = ToolInfo{ID: "memory.search", Description: v.pack().Tools["memory.search"].Description}
	}
	others := req.Tools.without("memory.search")
	for _, e := range req.Earlier {
		others = others.without(e.Tool)
	}
	t.pf.warm = t.rt.startWarm(ctx, req, t.msg)
	// The tool loop (modes loop and shadow) runs beside the planner. In loop mode it chooses the tools,
	// and the planner's menu shrinks to memory.search: listing only the tools that are on is how the
	// planner prompt already varies, so its text stays the evaluated one.
	loopStart := time.Now()
	loop, plannerTools := t.rt.startLoop(ctx, req, t.msg, t.timeRef, others)
	planRaw := t.askPlanner(ctx, memTool, plannerTools)
	plannerEnd := time.Now()
	plan, assessed, intent := t.readPlan(planRaw, plannerTools)
	canSearch := t.rt.Memory != nil && len(req.Rooms) > 0
	if assessed.MemoryState != "search" || !canSearch {
		t.pf.warm.stop() // a plan made again later searches without it
	}
	// the plan is all the memory search needs: while the loop still runs it starts now, beside the loop
	if assessed.MemoryState == "search" && canSearch && loop.running() && loopPriceKnown(req) {
		t.pf.early = t.rt.startEarlyMemory(ctx, req, t.msg, intent, t.res.CostUSD, loop, t.nowT, t.sp)
	}
	loopTools, loopRan := loop.wait()
	t.res.WaitedOn = t.sp.joinLoop(loop, loopStart, plannerEnd)
	if loop != nil {
		t.res.ToolLoop = loop.trace
		t.res.CostUSD += loop.trace.CostUSD
		t.res.Timing["tool_loop"] = loop.trace.Ms
		if loop.err != nil && req.ToolMode == ToolModeLoop {
			// the model takes no tools (or failed): the planner chooses the tools of this turn
			plannerTools = others
			planRaw = t.askPlanner(ctx, memTool, plannerTools)
			plan, assessed, intent = t.readPlan(planRaw, plannerTools)
			if t.pf.early != nil && (assessed.MemoryState != "search" || !reflect.DeepEqual(intent, t.pf.early.intent)) {
				// the second plan wants another search or none: what the first one found is not used
				t.res.CostUSD += t.pf.drop(req.Guard)
			}
		}
	}
	t.res.Plan, t.res.Assess = plan, assessed
	if !t.res.Plan.Parsed {
		slog.Default().WarnContext(ctx, "chat: planner response rejected", "parse_state", t.res.Plan.ParseState)
	}
	t.res.Intent = intent
	return turnPlan{intent: intent, loopTools: loopTools, loopRan: loopRan, canSearch: canSearch}
}

// askPlanner asks the planner model once with the tools it may choose (memory.search first when memory
// is on for the channel) and returns its raw plan, "" when the call failed. The turn gets its cost and time.
func (t *turn) askPlanner(ctx context.Context, memTool ToolInfo, tools Toolset) string {
	menu := tools
	if t.req.MemoryOn {
		menu = Toolset{Enabled: append([]ToolInfo{memTool}, tools.Enabled...), Known: tools.Known}
	}
	t0 := time.Now()
	planRaw, cost, err := t.rt.LLM.Chat(ctx, t.req.PlannerModel, BuildPlannerMessages(t.v, t.msg, t.req.History, "", t.timeRef, menu), PlannerMaxTokens, 0)
	t.res.CostUSD += cost
	t.res.Timing["plan"] += ms(t0)
	t.sp.add("planner", t0, time.Now())
	if err != nil {
		return ""
	}
	return planRaw
}

// readPlan parses and assesses a raw plan against the tools it was offered, and returns the memory
// search's intent with the time window the assessment read.
func (t *turn) readPlan(raw string, tools Toolset) (Plan, Assessment, search.Intent) {
	plan := ParsePlan(raw)
	assessed := Assess(t.v, plan, tools, t.req.MemoryOn, true)
	planned, _ := plan.Intent()
	if assessed.Time != nil {
		planned.Time = assessed.Time
	}
	return plan, assessed, planned
}

// runPlan runs the plan's tools and the memory search at once and adds the tools' results to res after
// the earlier ones and the loop's. planned is the plan's steps' results in plan order (nil for a step that
// gave none); searchMemory reports whether the memory search was the turn's to run, and mem is what it
// found (the early search's when one started).
func (t *turn) runPlan(ctx context.Context, p *turnPlan) (planned []*ToolResult, mem memoryRun, searchMemory bool) {
	req := t.req
	t1 := time.Now()
	t.res.Tools = append(t.res.Tools, req.Earlier...)
	if p.loopRan {
		t.res.Tools = append(t.res.Tools, p.loopTools...)
	}
	// the planner's tools and the memory search run at once (runSteps): each is checked against the
	// turn's limits in plan order (the memory search last, unless it started ahead of the loop's end);
	// results keep the plan's order. The memory search does wait for the tools that pay an unknown fee,
	// and counts what the others hold, so that it is told what they leave.
	planned = make([]*ToolResult, len(t.res.Assess.Steps))
	told := &roundProgress{report: req.OnProgress, round: 1} // the stage is told as searches are let in
	var toolsEnd lastEnd
	steps, searches := t.toolSteps(ctx, planned, told, &toolsEnd)
	searchMemory = t.res.Assess.MemoryState == "search" && p.canSearch
	if searchMemory && t.pf.early == nil {
		spent, intent, rt, sp, msg, nowT := t.res.CostUSD, p.intent, t.rt, t.sp, t.msg, t.nowT
		steps = append(steps, step{decide: func() bool { return req.Guard.AllowMoneyContext(ctx, "memory.search") },
			run: func() {
				// the fees of the steps decided before this one count too, whether they have ended yet or not:
				// an unknown fee is waited for, a bound still held counts in full
				req.Guard.WaitSpent(ctx)
				remaining := memoryBudget(ctx, req.Guard, func() float64 { return req.BudgetUSD - spent - feesSoFar(req.Guard) })
				defer sp.begin("memory")()
				mem = rt.searchMemory(ctx, req, msg, intent, remaining, nowT)
			}})
	}
	told.asked = searches
	runSteps(ctx, req.Guard, steps)
	if t.pf.early != nil {
		mem = t.pf.take()
	}
	t.pf.warm.stop()
	for _, r := range planned {
		if r != nil {
			t.res.Tools = append(t.res.Tools, *r)
		}
	}
	t.res.Timing["tools"] = int(max(toolsEnd.at.Sub(t1), 0).Milliseconds())
	return planned, mem, searchMemory
}

// lastEnd is when the last of the planner's tools that ran ended; each sets it as it ends.
type lastEnd struct {
	mu sync.Mutex
	at time.Time
}

func (e *lastEnd) mark() {
	e.mu.Lock()
	e.at = time.Now()
	e.mu.Unlock()
}

// toolSteps makes a step of each runnable step of the plan, whose result goes to planned at the step's
// place, and fills in the steps that need input. It returns the steps and how many of them are web searches.
func (t *turn) toolSteps(ctx context.Context, planned []*ToolResult, told *roundProgress, end *lastEnd) (steps []step, searches int) {
	req, rt, sp := t.req, t.rt, t.sp
	for i, st := range t.res.Assess.Steps {
		switch st.State {
		case "runnable":
			if rt.Tools == nil {
				break
			}
			info, _ := req.Tools.find(st.Tool)
			if st.Tool == webSearchTool {
				searches++
			}
			cancel := func() { planned[i] = &ToolResult{Tool: st.Tool, Status: "cancelled"} }
			hold := &paidHold{}
			steps = append(steps, step{key: stepKey(info, st.Tool), reserved: true, cancel: cancel, pays: info.Pays, maxFee: feeBound(info, st.Parameters), hold: hold, decide: func() bool {
				allowed, endPaid := admitTool(ctx, req.Guard, st.Tool, info, st.Parameters)
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
				if st.Tool == webSearchTool {
					told.admit(st.Parameters)
				}
				return true
			}, run: func() {
				defer sp.begin("tool:" + st.Tool)()
				r := runTool(ctx, req.Guard, rt.Tools, st.Tool, st.Parameters)
				planned[i] = &r
				end.mark()
			}})
		case "needs_input":
			planned[i] = &ToolResult{Tool: st.Tool, OK: false, Status: "needs_input", Error: "missing: " + strings.Join(st.Missing, ",")}
		}
	}
	return steps, searches
}

// readMemory puts what the memory search found on res (hits, state, trace and the reranker's cost), or
// why there is nothing. A search made again without the plan's time window leaves its wider intent in p.
func (t *turn) readMemory(p *turnPlan, mem memoryRun, searchMemory bool) {
	res, req := &t.res, t.req
	switch res.Assess.MemoryState {
	case "search":
		if !searchMemory || !mem.ran {
			res.RagState = "memory_unavailable"
			break
		}
		sr, err, relaxed, firstRerank := mem.sr, mem.err, mem.relaxed, mem.firstRerank
		if relaxed {
			res.CostUSD += firstRerank
			p.intent = mem.intent // the answer is not told "recent" about memory found outside the window
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
}

// answer asks the answer model, once more with less context when the prompt was too long for it, and
// sets res.Answer, the answer's time and cost and the turn's spans. An error is the answer call's.
func (t *turn) answer(ctx context.Context, p *turnPlan, planned []*ToolResult) error {
	res, req, v, msg, rt, sp := &t.res, t.req, t.v, t.msg, t.rt, t.sp
	t3 := time.Now()
	maxHistory := req.MaxHistory
	if maxHistory <= 0 {
		maxHistory = HistoryShown
	}
	searched, searchedState := res.Hits, res.RagState
	roomBlock := t.roomMemory()
	system := ComposeSystem(v, req.Persona, req.Rules, req.AgentPrompt)
	deep := deepSearched(res.ToolLoop, p.loopRan, res.Assess.Steps, planned)
	msgs := BuildAnswerMessages(v, AnswerInput{
		System: system, History: req.History, MaxHistory: maxHistory,
		Hits: res.Hits, RagState: res.RagState, Tools: res.Tools, Speaker: req.Speaker, Message: msg, Intent: p.intent,
		RecentRoom: req.RecentRoom, RoomMemory: roomBlock, Now: t.local, DeepSearch: deep,
	})
	withImages(msgs, req.Images)
	answerBudget := answerTokenLimit(msg, v.pack().Tag)
	if deep {
		answerBudget = answerBudget * 3 / 2 // a synthesis of several pages and aspects takes more words
	}
	completion, err := rt.complete(ctx, req.ResponseModel, msgs, answerBudget, 0.2)
	res.CostUSD += completion.CostUSD
	if err != nil && errclass.Of(err) == errclass.ContextLength && ctx.Err() == nil {
		// the prompt is longer than the model takes: once more with less of it. Half the conversation and
		// half the memory hits, no room memory and no ambient chat, and the tool results back to the room of
		// a turn without page text (toolBlockMax: the passages are cut first); the system prompt, the tool
		// results and the member's message stay. The hits the room memory covered come back with their state.
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
			Hits: res.Hits, RagState: res.RagState, Tools: res.Tools, Speaker: req.Speaker, Message: msg, Intent: p.intent, Now: t.local,
			DeepSearch: deep, ToolBudget: toolBlockMax,
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
		t.pf.warm.wait()
		res.CostUSD += req.Guard.PaidTotal() // what the tools paid for (a search's fee) is part of the turn's cost
		_, _, res.Skipped = req.Guard.Totals()
	}
	if err != nil {
		finish()
		return err
	}
	answer := t.wholeAnswer(ctx, completion, msgs, answerBudget)
	finish()
	res.Answer = strings.TrimSpace(answer)
	return nil
}

// roomMemory renders the room memory the answer is given (the people the plan named, when the caller
// resolves them) and leaves out of res the memory hits it covers.
func (t *turn) roomMemory() string {
	res, req := &t.res, t.req
	room := req.RoomMemory
	if req.People != nil {
		if resolved := req.People(res.Plan.PersonTargets); resolved != nil {
			room = resolved
		}
	}
	roomBlock, kept, roomTrace := renderRoomMemory(t.v, room, res.Hits)
	if roomTrace.DroppedHits > 0 {
		res.Hits = kept
		if len(kept) == 0 {
			res.RagState = RagCoveredByRoomMemory
		}
	}
	if !roomTrace.Empty() {
		res.Room = &roomTrace
	}
	return roomBlock
}

// wholeAnswer is the answer's text: cut after its first round when it repeats itself, and asked to go on
// once when the provider's output limit cut it off (marked incomplete when that does not finish it).
func (t *turn) wholeAnswer(ctx context.Context, completion Completion, msgs []Msg, answerBudget int) string {
	res, req, v, rt := &t.res, t.req, t.v, t.rt
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
	return answer
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

// feesSoFar is what the turn's tools have paid plus what the ones under way hold (Guard.HeldTotal). The
// holds are read first: a tool adds its fee before it lets go of its hold, so no fee is missed (one in
// between counts twice, which leaves the search less, never more).
func feesSoFar(g *Guard) float64 {
	held := g.HeldTotal()
	return held + g.PaidTotal()
}

// memoryBudget is what a memory search may count on (left). When the bounds held by the tools under way
// alone take that below what the reranker needs (retrieval's MinRemainingBudgetUSD), the search waits for
// them to end and counts what they really paid: a deep web search holds far more than it usually pays, and
// a search told what the bound leaves would go without its reranker. Waiting for the fee is what the
// search did before fees had bounds.
func memoryBudget(ctx context.Context, g *Guard, left func() float64) float64 {
	floor := retrieval.DefaultAutoPolicy.MinRemainingBudgetUSD
	if l, held := left(), g.HeldTotal(); held > 0 && l < floor && l+held >= floor {
		g.WaitHeld(ctx)
	}
	return left()
}

// deepSearched reports whether a web search of the turn that found something asked for depth deep: in the
// loop's calls when the loop chose the tools, or in the plan's steps.
func deepSearched(loop *LoopTrace, loopRan bool, steps []Step, planned []*ToolResult) bool {
	if loopRan && loop != nil {
		for _, s := range loop.Steps {
			if s.Tool == webSearchTool && s.State == "ran" && deepParams(s.Params) {
				return true
			}
		}
	}
	for i, st := range steps {
		if st.Tool == webSearchTool && i < len(planned) && planned[i] != nil && planned[i].OK && deepParams(st.Parameters) {
			return true
		}
	}
	return false
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
