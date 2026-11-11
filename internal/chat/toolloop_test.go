package chat

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"masamong/internal/lang"
)

// loopLLM is a scripted model: the planner answers plannerOut, the answer model "ok", and each tool
// loop call takes the next scripted reply.
type loopLLM struct {
	mu         sync.Mutex
	plannerOut string
	replies    []LoopReply
	errs       []error
	notReady   bool
	offered    [][]string  // function names offered per loop call
	loopMsgs   [][]LoopMsg // messages per loop call
	plannerSys []string    // planner system prompts
}

func (f *loopLLM) Chat(_ context.Context, model string, msgs []Msg, _ int, _ float64) (string, float64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if model == "planner" {
		f.plannerSys = append(f.plannerSys, msgs[0].Content)
		out := f.plannerOut
		if out == "" {
			out = `{"memory_requirement":"none","tool_plan":[]}`
		}
		return out, 0.001, nil
	}
	return "ok", 0.002, nil
}

func (f *loopLLM) ToolsReady(string) bool { return !f.notReady }

func (f *loopLLM) ChatTools(_ context.Context, role string, msgs []LoopMsg, tools []ToolSpec, _ int) (LoopReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var names []string
	for _, t := range tools {
		names = append(names, t.Name)
	}
	f.offered = append(f.offered, names)
	f.loopMsgs = append(f.loopMsgs, append([]LoopMsg(nil), msgs...))
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		if err != nil {
			return LoopReply{}, err
		}
	}
	if len(f.replies) == 0 {
		return LoopReply{Text: "-", CostUSD: 0.0005}, nil
	}
	r := f.replies[0]
	f.replies = f.replies[1:]
	r.CostUSD = 0.0005
	return r, nil
}

func call(id, name, args string) LoopCall { return LoopCall{ID: id, Name: name, Arguments: args} }

// loopTools is a small menu: a web search (web content), the weather, a quote and a poll (writes).
func loopTools() Toolset {
	ts := Toolset{Known: map[string]bool{"memory.search": true}}
	add := func(t ToolInfo) {
		ts.Known[t.ID] = true
		ts.Enabled = append(ts.Enabled, t)
	}
	add(ToolInfo{ID: "memory.search", Description: "memory"})
	add(ToolInfo{ID: "weather.query", Description: "weather", Params: []ToolParam{{Name: "region", Hint: "city"}, {Name: "day_offset", Type: "integer", Min: ptr(0), Max: ptr(13)}}})
	add(ToolInfo{ID: "search.web", Description: "search", Web: true, Required: []string{"query"}, Params: []ToolParam{{Name: "query", Required: true}}})
	add(ToolInfo{ID: "finance.quote", Description: "quote", Required: []string{"symbol"}, Params: []ToolParam{{Name: "symbol", Required: true}, {Name: "channel_id"}}})
	add(ToolInfo{ID: "polls.create", Description: "poll", Writes: true, Required: []string{"question"}, Params: []ToolParam{{Name: "question", Required: true}}})
	return ts
}

func ptr(n int) *int { return &n }

type ranTool struct {
	tool   string
	params map[string]any
}

// recorder is a ToolRunner that records what ran; fail lists tools that fail.
type recorder struct {
	mu   sync.Mutex
	ran  []ranTool
	fail map[string]bool
}

func (r *recorder) run(_ context.Context, tool string, params map[string]any) ToolResult {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.ran = append(r.ran, ranTool{tool, params})
	if r.fail[tool] {
		return ToolResult{Tool: tool, OK: false, Status: "unavailable"}
	}
	return ToolResult{Tool: tool, OK: true, Data: map[string]any{"status": "ok", "tool": tool}}
}

func (r *recorder) tools() []string {
	var out []string
	for _, x := range r.ran {
		out = append(out, x.tool)
	}
	return out
}

func loopRequest(msg string, mode string) Request {
	p, _ := lang.Get("en")
	return Request{Message: msg, Voice: Voice{Pack: p, BotName: "Masa"}, Tools: loopTools(), MemoryOn: true, PlannerModel: "planner",
		ResponseModel: "response", ToolMode: mode, History: []Turn{{Role: "user", Speaker: "Ann", Content: "I'm in Busan this week"}}}
}

func TestToolLoopRunsSeveralRoundsAndStopsWhenDone(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{
		{Calls: []LoopCall{call("a", "finance__quote", `{"symbol":"AAPL"}`)}, Raw: json.RawMessage(`{"role":"assistant","content":null,"tool_calls":[{"id":"a","type":"function","function":{"name":"finance__quote","arguments":"{\"symbol\":\"AAPL\"}"}}],"reasoning_content":"kept"}`)},
		{Calls: []LoopCall{call("b", "weather__query", `{"region":"Busan","day_offset":1}`)}},
		{Text: "-"},
	}}
	rec := &recorder{}
	rt := &Runtime{LLM: llm, Tools: rec.run}
	out, err := rt.Answer(context.Background(), loopRequest("apple stock and will it rain tomorrow?", ToolModeLoop))
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.tools(); !slices.Equal(got, []string{"finance.quote", "weather.query"}) {
		t.Fatalf("ran %v", got)
	}
	if rec.ran[1].params["region"] != "Busan" {
		t.Fatalf("weather params %v", rec.ran[1].params)
	}
	if len(out.Tools) != 2 || out.Tools[0].Tool != "finance.quote" {
		t.Fatalf("results %+v", out.Tools)
	}
	tr := out.ToolLoop
	if tr == nil || tr.Mode != "loop" || tr.Rounds != 3 || tr.Stop != "answered" || len(tr.Steps) != 2 {
		t.Fatalf("trace %+v", tr)
	}
	// the provider's own assistant message goes back unchanged, and each call gets its tool message
	second := llm.loopMsgs[1]
	if string(second[2].Raw) == "" || !strings.Contains(string(second[2].Raw), "reasoning_content") || second[3].Role != "tool" || second[3].CallID != "a" {
		t.Fatalf("round 2 messages %+v", second)
	}
	if !strings.Contains(second[3].Content, `"untrusted_data":true`) {
		t.Fatalf("tool result not marked untrusted: %s", second[3].Content)
	}
	// the conversation reaches the loop (Busan was said earlier)
	if !strings.Contains(llm.loopMsgs[0][1].Content, "Busan") || !strings.Contains(llm.loopMsgs[0][0].Content, "Masa") {
		t.Fatalf("loop input %+v", llm.loopMsgs[0])
	}
	// in loop mode the planner is offered memory.search only
	if len(llm.plannerSys) != 1 || !strings.Contains(llm.plannerSys[0], "- memory.search:") || strings.Contains(llm.plannerSys[0], "weather.query") {
		t.Fatalf("planner menu: %s", llm.plannerSys)
	}
	if out.CostUSD < 0.0015+0.002 {
		t.Fatalf("loop cost not counted: %v", out.CostUSD)
	}
}

func TestToolLoopRefusesUnknownToolsAndScopeKeys(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{
		call("a", "shell__exec", `{"cmd":"rm"}`),
		call("b", "finance__quote", `{"symbol":"NVDA","channel_id":"999","extra":"x"}`),
	}}}}
	rec := &recorder{}
	out, err := (&Runtime{LLM: llm, Tools: rec.run}).Answer(context.Background(), loopRequest("nvidia price", ToolModeLoop))
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.ran) != 1 || rec.ran[0].tool != "finance.quote" {
		t.Fatalf("ran %+v", rec.ran)
	}
	if _, ok := rec.ran[0].params["channel_id"]; ok || rec.ran[0].params["extra"] != nil || rec.ran[0].params["symbol"] != "NVDA" {
		t.Fatalf("params not cleaned: %v", rec.ran[0].params)
	}
	if out.ToolLoop.Steps[0].Reason != "unknown_tool" || out.ToolLoop.Refused != 1 {
		t.Fatalf("trace %+v", out.ToolLoop)
	}
	// every call, refused or not, is answered so the next round is a valid conversation
	next := llm.loopMsgs[1]
	if next[len(next)-2].CallID != "a" || next[len(next)-1].CallID != "b" {
		t.Fatalf("tool messages %+v", next[len(next)-2:])
	}
}

func TestToolLoopBadArgumentsAskOnceThenDrop(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{
		{Calls: []LoopCall{call("a", "finance__quote", `{"symbol":`)}},
		{Calls: []LoopCall{call("b", "finance__quote", `not json`)}},
		{Calls: []LoopCall{call("c", "finance__quote", `{"symbol":"AAPL"}`)}},
	}}
	rec := &recorder{}
	out, err := (&Runtime{LLM: llm, Tools: rec.run}).Answer(context.Background(), loopRequest("apple price", ToolModeLoop))
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.ran) != 0 {
		t.Fatalf("a tool ran: %+v", rec.ran)
	}
	if !strings.Contains(llm.loopMsgs[1][len(llm.loopMsgs[1])-1].Content, "call again") {
		t.Fatalf("no re-ask: %+v", llm.loopMsgs[1])
	}
	if slices.Contains(llm.offered[2], "finance__quote") {
		t.Fatalf("tool still offered after two bad arguments: %v", llm.offered[2])
	}
	if !slices.Contains(out.ToolLoop.Removed, "finance.quote") {
		t.Fatalf("trace %+v", out.ToolLoop)
	}
}

func TestToolLoopRunsARepeatedCallOnce(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{
		{Calls: []LoopCall{call("a", "search__web", `{"query":"Busan Festival"}`)}},
		{Calls: []LoopCall{call("b", "search__web", `{"query":" busan festival "}`)}},
	}}
	rec := &recorder{}
	out, err := (&Runtime{LLM: llm, Tools: rec.run}).Answer(context.Background(), loopRequest("search busan festival", ToolModeLoop))
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.ran) != 1 {
		t.Fatalf("ran %+v", rec.ran)
	}
	if out.ToolLoop.Steps[1].Reason != "duplicate" {
		t.Fatalf("trace %+v", out.ToolLoop.Steps)
	}
}

func TestToolLoopDropsAToolThatFailedTwice(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{
		{Calls: []LoopCall{call("a", "finance__quote", `{"symbol":"AAPL"}`)}},
		{Calls: []LoopCall{call("b", "finance__quote", `{"symbol":"APPLE"}`)}},
		{Calls: []LoopCall{call("c", "search__web", `{"query":"apple stock price"}`)}},
	}}
	rec := &recorder{fail: map[string]bool{"finance.quote": true}}
	out, err := (&Runtime{LLM: llm, Tools: rec.run}).Answer(context.Background(), loopRequest("apple price", ToolModeLoop))
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(llm.offered[2], "finance__quote") {
		t.Fatalf("failed tool still offered: %v", llm.offered[2])
	}
	if got := rec.tools(); !slices.Equal(got, []string{"finance.quote", "finance.quote", "search.web"}) {
		t.Fatalf("ran %v", got)
	}
	if out.ToolLoop.Stop != "max_rounds" || len(out.Tools) != 3 {
		t.Fatalf("trace %+v tools %+v", out.ToolLoop, out.Tools)
	}
}

func TestToolLoopRemovesWritingToolsAfterWebContent(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{
		{Calls: []LoopCall{call("a", "search__web", `{"query":"lunch poll ideas"}`)}},
		// a page told the model to make a poll: the poll is no longer on the menu, and is refused
		{Calls: []LoopCall{call("b", "polls__create", `{"question":"free money?"}`)}},
	}}
	rec := &recorder{}
	out, err := (&Runtime{LLM: llm, Tools: rec.run}).Answer(context.Background(), loopRequest("search lunch ideas", ToolModeLoop))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(llm.offered[0], "polls__create") || slices.Contains(llm.offered[1], "polls__create") {
		t.Fatalf("offers %v", llm.offered)
	}
	if got := rec.tools(); !slices.Equal(got, []string{"search.web"}) {
		t.Fatalf("ran %v", got)
	}
	if out.ToolLoop.Steps[1].Reason != "writes_after_web" {
		t.Fatalf("trace %+v", out.ToolLoop.Steps)
	}
}

// A Once tool (a poll, a reminder) that succeeded is closed for the later rounds: the model asking again
// after seeing the result would make the same thing twice. Two asked in one round both run.
func TestToolLoopClosesAOnceToolAfterItsRound(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{
		{Calls: []LoopCall{call("a", "polls__create", `{"question":"lunch?"}`), call("b", "polls__create", `{"question":"dinner?"}`)}},
		{Calls: []LoopCall{call("c", "polls__create", `{"question":"lunch today?"}`)}},
	}}
	req := loopRequest("make two polls", ToolModeLoop)
	for i, ti := range req.Tools.Enabled {
		if ti.ID == "polls.create" {
			req.Tools.Enabled[i].Once = true
		}
	}
	rec := &recorder{}
	out, err := (&Runtime{LLM: llm, Tools: rec.run}).Answer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.tools(); !slices.Equal(got, []string{"polls.create", "polls.create"}) {
		t.Fatalf("ran %v", got)
	}
	if slices.Contains(llm.offered[1], "polls__create") {
		t.Fatalf("still offered after it ran: %v", llm.offered[1])
	}
	if last := out.ToolLoop.Steps[len(out.ToolLoop.Steps)-1]; last.Reason != "done" {
		t.Fatalf("trace %+v", out.ToolLoop.Steps)
	}
}

func TestToolLoopMissingRequiredValueIsNotFilled(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{call("a", "finance__quote", `{"symbol":"user's stock"}`)}}}}
	rec := &recorder{}
	out, err := (&Runtime{LLM: llm, Tools: rec.run}).Answer(context.Background(), loopRequest("how is my stock", ToolModeLoop))
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.ran) != 0 || len(out.Tools) != 1 || out.Tools[0].Status != "needs_input" {
		t.Fatalf("ran %+v results %+v", rec.ran, out.Tools)
	}
}

func TestToolLoopFallsBackToThePlannerWhenToolsAreRefused(t *testing.T) {
	plan := `{"memory_requirement":"none","tool_plan":[{"tool_name":"weather.query","parameters":{"region":"Busan"}}]}`
	llm := &loopLLM{plannerOut: plan, errs: []error{fmt.Errorf("%w: test-model", ErrToolsUnsupported)}}
	rec := &recorder{}
	out, err := (&Runtime{LLM: llm, Tools: rec.run}).Answer(context.Background(), loopRequest("weather in busan?", ToolModeLoop))
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.tools(); !slices.Equal(got, []string{"weather.query"}) {
		t.Fatalf("ran %v", got)
	}
	if out.ToolLoop == nil || out.ToolLoop.Stop != "fallback" || out.ToolLoop.Fallback != "unsupported" {
		t.Fatalf("trace %+v", out.ToolLoop)
	}
	// the second planner call had the whole menu
	if len(llm.plannerSys) != 2 || !strings.Contains(llm.plannerSys[1], "weather.query") {
		t.Fatalf("planner calls %d", len(llm.plannerSys))
	}
	// a model known to refuse tools is not asked again: the planner chooses at once
	llm2 := &loopLLM{plannerOut: plan, notReady: true}
	rec2 := &recorder{}
	out2, err := (&Runtime{LLM: llm2, Tools: rec2.run}).Answer(context.Background(), loopRequest("weather in busan?", ToolModeLoop))
	if err != nil {
		t.Fatal(err)
	}
	if len(llm2.offered) != 0 || len(llm2.plannerSys) != 1 || !slices.Equal(rec2.tools(), []string{"weather.query"}) || out2.ToolLoop.Fallback != "unsupported" {
		t.Fatalf("offered %v planner %d ran %v trace %+v", llm2.offered, len(llm2.plannerSys), rec2.tools(), out2.ToolLoop)
	}
}

func TestToolLoopPrefilterSkipsMessagesWithoutWords(t *testing.T) {
	llm := &loopLLM{}
	rec := &recorder{}
	out, err := (&Runtime{LLM: llm, Tools: rec.run}).Answer(context.Background(), loopRequest("👍👍 !!", ToolModeLoop))
	if err != nil {
		t.Fatal(err)
	}
	if len(llm.offered) != 0 || len(rec.ran) != 0 || out.ToolLoop.Stop != "prefilter" {
		t.Fatalf("offered %v ran %v trace %+v", llm.offered, rec.ran, out.ToolLoop)
	}
	for _, msg := range []string{"ㅋㅋ", "12", "東京"} {
		if !hasContent(msg) {
			t.Fatalf("%q has letters or digits", msg)
		}
	}
}

func TestShadowModeRecordsTheLoopButRunsThePlannersTools(t *testing.T) {
	plan := `{"memory_requirement":"none","tool_plan":[{"tool_name":"weather.query","parameters":{"region":"Seoul"}}]}`
	llm := &loopLLM{plannerOut: plan, replies: []LoopReply{
		{Calls: []LoopCall{call("a", "weather__query", `{"region":"Busan"}`), call("b", "polls__create", `{"question":"rain?"}`)}},
	}}
	rec := &recorder{}
	out, err := (&Runtime{LLM: llm, Tools: rec.run}).Answer(context.Background(), loopRequest("will it rain?", ToolModeShadow))
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.tools(); !slices.Equal(got, []string{"weather.query"}) || rec.ran[0].params["region"] != "Seoul" {
		t.Fatalf("ran %+v", rec.ran)
	}
	tr := out.ToolLoop
	if tr.Mode != "shadow" || len(tr.Steps) != 2 || tr.Steps[0].Reason != "shadow" || tr.Steps[0].Params["region"] != "Busan" {
		t.Fatalf("trace %+v", tr)
	}
	if b := tr.Brief(); b.Steps[0].Params != nil || tr.Steps[0].Params == nil {
		t.Fatal("Brief must drop arguments and leave the trace alone")
	}
	if !strings.Contains(llm.plannerSys[0], "weather.query") {
		t.Fatal("shadow mode must keep the planner's whole menu")
	}
}

func TestPlannerModeDoesNotStartTheLoop(t *testing.T) {
	llm := &loopLLM{}
	out, err := (&Runtime{LLM: llm, Tools: (&recorder{}).run}).Answer(context.Background(), loopRequest("weather?", ""))
	if err != nil {
		t.Fatal(err)
	}
	if len(llm.offered) != 0 || out.ToolLoop != nil {
		t.Fatalf("loop ran in planner mode: %v %+v", llm.offered, out.ToolLoop)
	}
}

func TestToolSchemasAreStableAndClosed(t *testing.T) {
	specs := ToolSchemas(loopTools())
	var names []string
	for _, s := range specs {
		names = append(names, s.Name)
		var obj map[string]any
		if err := json.Unmarshal(s.Parameters, &obj); err != nil {
			t.Fatal(err)
		}
		if obj["additionalProperties"] != false || obj["type"] != "object" {
			t.Fatalf("%s schema %s", s.ID, s.Parameters)
		}
		props := obj["properties"].(map[string]any)
		for k := range props {
			if forbiddenParams[k] {
				t.Fatalf("%s offers %s", s.ID, k)
			}
		}
	}
	if !slices.Equal(names, []string{"finance__quote", "polls__create", "search__web", "weather__query"}) {
		t.Fatalf("order %v", names)
	}
	if string(specs[3].Parameters) != `{"type":"object","properties":{"day_offset":{"type":"integer","minimum":0,"maximum":13},"region":{"type":"string","description":"city"}},"additionalProperties":false}` {
		t.Fatalf("weather schema %s", specs[3].Parameters)
	}
	if string(specs[0].Parameters) != `{"type":"object","properties":{"symbol":{"type":"string"}},"required":["symbol"],"additionalProperties":false}` {
		t.Fatalf("quote schema %s", specs[0].Parameters)
	}
}

// A picture that is only queued is told to the answer model in every language: one short line, nothing
// described as done. A finished picture, or a failed one, does not carry the note.
func TestAnswerSaysAQueuedPictureIsOnItsWay(t *testing.T) {
	for _, tag := range lang.Available() {
		p, _ := lang.Get(tag)
		if strings.TrimSpace(p.Chat.ToolQueued) == "" {
			t.Fatalf("%s has no tool_queued text", tag)
		}
		v := Voice{Pack: p}
		build := func(r ToolResult) string {
			m := BuildAnswerMessages(v, AnswerInput{System: "S", Message: "draw", Tools: []ToolResult{r}})
			return m[len(m)-1].Content
		}
		for _, st := range []string{"queued", "already_queued"} {
			if !strings.Contains(build(ToolResult{Tool: "images.generate", OK: true, Status: st, Data: map[string]any{"status": st}}), p.Chat.ToolQueued) {
				t.Fatalf("%s: a %s picture must carry the note", tag, st)
			}
		}
		for _, r := range []ToolResult{
			{Tool: "images.generate", OK: true, Data: map[string]any{"status": "ok", "image_id": "img_x"}},
			{Tool: "images.generate", OK: false, Status: "queued"},
			{Tool: "images.generate", OK: false, Status: "TOOL_LIMITED", Error: "busy"},
		} {
			if strings.Contains(build(r), p.Chat.ToolQueued) {
				t.Fatalf("%s: the note appeared for %+v", tag, r)
			}
		}
	}
}

func TestAnswerSaysWhatAToolAssumed(t *testing.T) {
	p, _ := lang.Get("ko")
	v := Voice{Pack: p}
	with := BuildAnswerMessages(v, AnswerInput{System: "S", Message: "날씨?", Tools: []ToolResult{{Tool: "weather.query", OK: true, Data: map[string]any{"region": "서울", "assumed_region": "서울"}}}})
	without := BuildAnswerMessages(v, AnswerInput{System: "S", Message: "날씨?", Tools: []ToolResult{{Tool: "weather.query", OK: true, Data: map[string]any{"region": "부산"}}}})
	if !strings.Contains(with[len(with)-1].Content, p.Chat.ToolAssumed) || strings.Contains(without[len(without)-1].Content, p.Chat.ToolAssumed) {
		t.Fatal("the assumed-value note must appear exactly when a result carries assumed_ values")
	}
}

// A reply built from a web search leads with its conclusion and does not list sources: the answer's tool
// block carries the note (every language), only for a usable search result. Weather, a failed search and a
// reminder do not (so the Korean golden, which has weather, is unchanged).
func TestAnswerFromASearchLeadsWithTheConclusion(t *testing.T) {
	for _, tag := range []string{"ko", "en", "ja", "es", "zh-Hans"} {
		p, _ := lang.Get(tag)
		if strings.TrimSpace(p.Chat.ToolConclusion) == "" {
			t.Fatalf("%s has no tool_conclusion text", tag)
		}
		v := Voice{Pack: p}
		build := func(r ToolResult) string {
			m := BuildAnswerMessages(v, AnswerInput{System: "S", Message: "q", Tools: []ToolResult{r}})
			return m[len(m)-1].Content
		}
		for _, tool := range []string{"search.web", "web.read", "places.search"} {
			if !strings.Contains(build(ToolResult{Tool: tool, OK: true, Data: map[string]any{"results": []any{}}}), p.Chat.ToolConclusion) {
				t.Fatalf("%s: a %s result must carry the note", tag, tool)
			}
		}
		for _, r := range []ToolResult{
			{Tool: "search.web", OK: false, Status: "TOOL_LIMITED", Error: "busy"},
			{Tool: "weather.query", OK: true, Data: map[string]any{"temp_c": 18.0}},
			{Tool: "reminders.propose", OK: true, Status: "scheduled"},
		} {
			if strings.Contains(build(r), p.Chat.ToolConclusion) {
				t.Fatalf("%s: the note appeared for %+v", tag, r)
			}
		}
	}
}
