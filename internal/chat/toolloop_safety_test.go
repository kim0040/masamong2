package chat

import (
	"context"
	"slices"
	"strings"
	"testing"
)

// panicLoopLLM is a model whose tool-calling reply makes the adapter panic (a bug in decoding a
// provider's answer); the planner still answers.
type panicLoopLLM struct{ loopLLM }

func (*panicLoopLLM) ChatTools(context.Context, string, []LoopMsg, []ToolSpec, int) (LoopReply, error) {
	var m map[string]int
	m["x"]++ // stands in for any bug in decoding a provider reply
	return LoopReply{}, nil
}

// A panic inside the tool loop happens on the goroutine startLoop starts, out of reach of the turn's
// recover: it is caught there, and the turn goes on with the planner's tools.
func TestToolLoopPanicFallsBackToThePlanner(t *testing.T) {
	plan := `{"memory_requirement":"none","tool_plan":[{"tool_name":"weather.query","parameters":{"region":"Busan"}}]}`
	llm := &panicLoopLLM{loopLLM: loopLLM{plannerOut: plan}}
	rec := &recorder{}
	out, err := (&Runtime{LLM: llm, Tools: rec.run}).Answer(context.Background(), loopRequest("weather in busan?", ToolModeLoop))
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.tools(); !slices.Equal(got, []string{"weather.query"}) {
		t.Fatalf("ran %v", got)
	}
	if out.ToolLoop == nil || out.ToolLoop.Stop != "fallback" || !strings.HasPrefix(out.ToolLoop.Fallback, "error:") {
		t.Fatalf("trace %+v", out.ToolLoop)
	}
	// the loop alone, as the turn starts it
	rt := &Runtime{LLM: llm, Tools: rec.run}
	req := loopRequest("what is the weather", ToolModeLoop)
	lr, _ := rt.startLoop(context.Background(), req, req.Message, "", req.Tools)
	if res, theTurns := lr.wait(); res != nil || theTurns || lr.err == nil {
		t.Fatalf("results %v turn's %v err %v", res, theTurns, lr.err)
	}
}

// panicSecondRoundLLM answers the first round, then panics inside the second tool-calling call
// (a bug decoding a provider's reply that arrives after a tool already ran).
type panicSecondRoundLLM struct {
	loopLLM
	calls int
}

func (p *panicSecondRoundLLM) ChatTools(ctx context.Context, role string, msgs []LoopMsg, tools []ToolSpec, maxTokens int) (LoopReply, error) {
	p.calls++
	if p.calls > 1 {
		var m map[string]int
		m["x"]++ // stands in for any bug in decoding a provider reply
	}
	return p.loopLLM.ChatTools(ctx, role, msgs, tools, maxTokens)
}

// A panic in a later round keeps what the loop already did: its results still reach the answer, and the
// planner that chooses the tools in its place must not run a write the loop already ran (a second poll).
func TestToolLoopPanicKeepsTheEarlierResults(t *testing.T) {
	// the planner, asked after the loop dies, wants the poll the loop already made
	plan := `{"memory_requirement":"none","tool_plan":[{"tool_name":"polls.create","parameters":{"question":"lunch?"}}]}`
	llm := &panicSecondRoundLLM{loopLLM: loopLLM{plannerOut: plan, replies: []LoopReply{
		{Calls: []LoopCall{call("a", "polls__create", `{"question":"lunch?"}`)}},
	}}}
	rec := &recorder{}
	out, err := (&Runtime{LLM: llm, Tools: rec.run}).Answer(context.Background(), loopRequest("make a lunch poll", ToolModeLoop))
	if err != nil {
		t.Fatal(err)
	}
	// the write ran exactly once: the fallback plan's poll step must not run again
	if got := rec.tools(); !slices.Equal(got, []string{"polls.create"}) {
		t.Fatalf("ran %v", got)
	}
	// the first result is kept: the answer sees the poll the loop made, not nothing
	if len(out.Tools) != 1 || out.Tools[0].Tool != "polls.create" || !out.Tools[0].OK {
		t.Fatalf("results %+v", out.Tools)
	}
	if out.ToolLoop == nil || out.ToolLoop.Stop != "fallback" || !strings.HasPrefix(out.ToolLoop.Fallback, "error:") {
		t.Fatalf("trace %+v", out.ToolLoop)
	}
	// the planner that replaced the loop was offered the menu without the tool that already ran
	if len(llm.plannerSys) != 2 || strings.Contains(llm.plannerSys[1], "polls.create") || !strings.Contains(llm.plannerSys[1], "weather.query") {
		t.Fatalf("planner calls %d: %v", len(llm.plannerSys), llm.plannerSys)
	}
}

// A tool the loop only asked for without its required input (needs_input) never ran: when a panic in a
// later round hands the turn to the planner, the planner's menu keeps it, and its valid call runs once.
func TestToolLoopPanicKeepsAToolThatNeverRan(t *testing.T) {
	plan := `{"memory_requirement":"none","tool_plan":[{"tool_name":"polls.create","parameters":{"question":"lunch?"}}]}`
	llm := &panicSecondRoundLLM{loopLLM: loopLLM{plannerOut: plan, replies: []LoopReply{
		{Calls: []LoopCall{call("a", "polls__create", `{}`)}},
	}}}
	rec := &recorder{}
	out, err := (&Runtime{LLM: llm, Tools: rec.run}).Answer(context.Background(), loopRequest("make a lunch poll", ToolModeLoop))
	if err != nil {
		t.Fatal(err)
	}
	if got := rec.tools(); !slices.Equal(got, []string{"polls.create"}) {
		t.Fatalf("ran %v", got)
	}
	if len(llm.plannerSys) != 2 || !strings.Contains(llm.plannerSys[1], "polls.create") {
		t.Fatalf("the planner's menu lost the tool that never ran: %v", llm.plannerSys)
	}
	// the loop's own needs_input result stays in the turn beside the planner's run
	var ok, needs int
	for _, r := range out.Tools {
		if r.Tool == "polls.create" && r.OK {
			ok++
		}
		if r.Tool == "polls.create" && r.Status == "needs_input" {
			needs++
		}
	}
	if ok != 1 || needs != 1 {
		t.Fatalf("results %+v", out.Tools)
	}
}

// reminderMenu offers reminders.update with its closed choice of op, as the application declares it.
func reminderMenu() Toolset {
	ts := Toolset{Known: map[string]bool{"reminders.update": true, "polls.create": true, "search.web": true}}
	ts.Enabled = []ToolInfo{
		{ID: "reminders.update", Description: "reminders", Writes: true,
			Params: []ToolParam{{Name: "op", Enum: []string{"list", "cancel", "pause", "resume"}}, {Name: "id"}, {Name: "match"}}},
		{ID: "polls.create", Description: "poll", Writes: true, Required: []string{"question"},
			Params: []ToolParam{{Name: "question", Required: true}, {Name: "options_text"}, {Name: "options", Type: "array"}}},
		{ID: "search.web", Description: "web", Required: []string{"query"},
			Params: []ToolParam{{Name: "query", Required: true}, {Name: "size", Type: "integer"}}},
	}
	return ts
}

// A value outside a closed choice is refused before the call runs: the schema offers only the choice's
// values, but a model may still send another (op confirm would set a reminder for the person). A value
// that differs only in case or spaces is the choice.
func TestToolLoopRefusesAValueOutsideTheChoice(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{
		{Calls: []LoopCall{call("a", "reminders__update", `{"op":"confirm","id":"rm_1"}`)}},
		{Calls: []LoopCall{call("b", "reminders__update", `{"op":" LIST "}`)}},
	}}
	rec := &recorder{}
	req := loopRequest("show my reminders", ToolModeLoop)
	req.Tools = reminderMenu()
	out, err := (&Runtime{LLM: llm, Tools: rec.run}).Answer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.ran) != 1 || rec.ran[0].params["op"] != "list" {
		t.Fatalf("ran %+v", rec.ran)
	}
	if s := out.ToolLoop.Steps[0]; s.State != "refused" || s.Reason != "bad_arguments" {
		t.Fatalf("trace %+v", out.ToolLoop.Steps)
	}
}

// A planned step carries only the parameters the tool declares: an invented provider choice or a
// poll's id never reaches the tool, and the options the planner prompts describe (a declared list)
// stay. What was dropped is named in stripped.
func TestPlannerStepsKeepOnlyDeclaredParameters(t *testing.T) {
	p := ParsePlan(`{"memory_requirement":"none","tool_plan":[` +
		`{"tool_name":"search.web","parameters":{"query":"news","provider":"tavily"}},` +
		`{"tool_name":"polls.create","parameters":{"question":"lunch?","options":["a","b"],"id":"poll_other"}}]}`)
	a := Assess(koVoice(), p, reminderMenu(), true, true)
	if len(a.Steps) != 2 {
		t.Fatalf("steps %+v", a.Steps)
	}
	if got := a.Steps[0].Parameters; len(got) != 1 || got["query"] != "news" {
		t.Fatalf("search step %+v", got)
	}
	got := a.Steps[1].Parameters
	if len(got) != 2 || got["question"] != "lunch?" {
		t.Fatalf("poll step %+v", got)
	}
	if opts, _ := got["options"].([]any); len(opts) != 2 {
		t.Fatalf("options %+v", got)
	}
	if !slices.Equal(a.Stripped, []string{"search.web.provider", "polls.create.id"}) {
		t.Fatalf("stripped %v", a.Stripped)
	}
}

// Planner mode passes a closed choice only a value inside it, as the tool loop does: op confirm never
// reaches the tool. A key the tool does not declare (a poll's id) is stripped with it.
func TestPlannerStepsKeepClosedChoices(t *testing.T) {
	p := ParsePlan(`{"memory_requirement":"none","tool_plan":[` +
		`{"tool_name":"polls.create","parameters":{"question":"lunch?","id":"poll_other"}},` +
		`{"tool_name":"reminders.update","parameters":{"op":"confirm","id":"rm_1"}},` +
		`{"tool_name":"reminders.update","parameters":{"op":"Cancel","id":"rm_2"}}]}`)
	a := Assess(koVoice(), p, reminderMenu(), true, true)
	if len(a.Steps) != 3 {
		t.Fatalf("steps %+v", a.Steps)
	}
	if got := a.Steps[0].Parameters; len(got) != 1 || got["question"] != "lunch?" {
		t.Fatalf("poll step %+v", got)
	}
	if _, has := a.Steps[1].Parameters["op"]; has || a.Steps[1].Parameters["id"] != "rm_1" {
		t.Fatalf("confirm step %+v", a.Steps[1].Parameters)
	}
	if a.Steps[2].Parameters["op"] != "cancel" {
		t.Fatalf("cancel step %+v", a.Steps[2].Parameters)
	}
	if !slices.Equal(a.Stripped, []string{"polls.create.id", "reminders.update.op"}) {
		t.Fatalf("stripped %v", a.Stripped)
	}
}

// A quote the quote service could not give is answered by a web search under finance.quote's name: the
// result says so (source search), and the loop closes the writing tools after it as after any web content.
func TestToolLoopClosesWritingToolsAfterAQuoteFromSearch(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{
		{Calls: []LoopCall{call("a", "finance__quote", `{"symbol":"SMALLCO"}`)}},
		// a snippet told the model to make a poll: the poll is no longer on the menu, and is refused
		{Calls: []LoopCall{call("b", "polls__create", `{"question":"free money?"}`)}},
	}}
	var ran []string
	run := func(_ context.Context, tool string, _ map[string]any) ToolResult {
		ran = append(ran, tool)
		if tool == "finance.quote" {
			return ToolResult{Tool: tool, OK: true, Data: map[string]any{"status": "ok", "source": WebSource, "note": "quote_from_search"}}
		}
		return ToolResult{Tool: tool, OK: true, Data: map[string]any{"status": "ok"}}
	}
	out, err := (&Runtime{LLM: llm, Tools: run}).Answer(context.Background(), loopRequest("how is smallco stock", ToolModeLoop))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ran, []string{"finance.quote"}) {
		t.Fatalf("ran %v", ran)
	}
	if slices.Contains(llm.offered[1], "polls__create") || out.ToolLoop.Steps[1].Reason != "writes_after_web" {
		t.Fatalf("offers %v trace %+v", llm.offered, out.ToolLoop.Steps)
	}
	// a live quote (another source) leaves them open
	llm2 := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{call("a", "finance__quote", `{"symbol":"AAPL"}`)}}}}
	rec := &recorder{}
	if _, err := (&Runtime{LLM: llm2, Tools: rec.run}).Answer(context.Background(), loopRequest("how is apple stock", ToolModeLoop)); err != nil {
		t.Fatal(err)
	}
	if len(llm2.offered) < 2 || !slices.Contains(llm2.offered[1], "polls__create") {
		t.Fatalf("offers after a live quote %v", llm2.offered)
	}
}
