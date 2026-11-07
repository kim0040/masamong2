package chat

import (
	"context"
	"math"
	"slices"
	"testing"
	"time"
)

// delayLLM is the scripted loop model with a delay in front of the planner and the tool loop, so a test
// decides which side of the join finishes last.
type delayLLM struct {
	*loopLLM
	planner, loop time.Duration
}

func (d delayLLM) Chat(ctx context.Context, model string, msgs []Msg, n int, temp float64) (string, float64, error) {
	if model == "planner" {
		time.Sleep(d.planner)
	}
	return d.loopLLM.Chat(ctx, model, msgs, n, temp)
}

func (d delayLLM) ChatTools(ctx context.Context, role string, msgs []LoopMsg, tools []ToolSpec, n int) (LoopReply, error) {
	time.Sleep(d.loop)
	return d.loopLLM.ChatTools(ctx, role, msgs, tools, n)
}

func spanOf(t *testing.T, spans []Span, name string) Span {
	t.Helper()
	i := slices.IndexFunc(spans, func(s Span) bool { return s.Name == name })
	if i < 0 {
		t.Fatalf("no span %q in %+v", name, spans)
	}
	return spans[i]
}

// The join of the planner and the tool loop records which of them the turn waited for, and the spans
// put the two side by side from the turn's start.
func TestSpansShowWhichSideOfTheJoinWasWaitedOn(t *testing.T) {
	const slow, fast = 120 * time.Millisecond, 5 * time.Millisecond
	for _, c := range []struct {
		name          string
		planner, loop time.Duration
		want          string
	}{{"loop is slower", fast, slow, "loop"}, {"planner is slower", slow, fast, "planner"}} {
		t.Run(c.name, func(t *testing.T) {
			req := loopRequest("will it rain?", ToolModeLoop)
			req.TurnStart = time.Now().Add(-50 * time.Millisecond) // 50 ms of the turn passed before Answer
			out, err := (&Runtime{LLM: delayLLM{loopLLM: &loopLLM{}, planner: c.planner, loop: c.loop}, Tools: (&recorder{}).run}).Answer(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if out.WaitedOn != c.want {
				t.Fatalf("waited on %q, want %q (spans %+v)", out.WaitedOn, c.want, out.Spans)
			}
			planner, loop, answer := spanOf(t, out.Spans, "planner"), spanOf(t, out.Spans, "tool_loop"), spanOf(t, out.Spans, "answer")
			// offsets count from the turn's start, not from Answer
			if planner.StartMS < 50 || loop.StartMS < 50 {
				t.Errorf("spans start before the turn's own 50 ms: %+v %+v", planner, loop)
			}
			if min(planner.EndMS-planner.StartMS, loop.EndMS-loop.StartMS) < int(fast.Milliseconds()) {
				t.Errorf("a span is shorter than the delay it waited: %+v %+v", planner, loop)
			}
			slower := max(planner.EndMS, loop.EndMS)
			if answer.StartMS < slower-2 { // the answer starts after the join released
				t.Errorf("answer starts at %d ms before the join released at %d ms", answer.StartMS, slower)
			}
			if out.Timing["plan"] == 0 && c.planner > 0 || out.Timing["tool_loop"] == 0 && c.loop > 0 {
				t.Errorf("the old timing keys keep their meaning: %+v", out.Timing)
			}
		})
	}
}

// Without a tool loop there is no join to wait on; the planner's tool and the memory search still show.
func TestSpansWithoutLoopNameThePlannersTools(t *testing.T) {
	plan := `{"memory_requirement":"none","tool_plan":[{"tool_name":"weather.query","parameters":{"region":"Busan"}}]}`
	llm := &loopLLM{plannerOut: plan}
	req := loopRequest("will it rain?", ToolModePlanner)
	out, err := (&Runtime{LLM: llm, Tools: (&recorder{}).run}).Answer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	if out.WaitedOn != "" {
		t.Errorf("no loop ran, waited on %q", out.WaitedOn)
	}
	tool := spanOf(t, out.Spans, "tool:weather.query")
	if tool.EndMS < tool.StartMS || spanOf(t, out.Spans, "planner").EndMS > tool.StartMS+1 {
		t.Errorf("the tool starts after the planner: %+v", out.Spans)
	}
	for _, s := range out.Spans {
		if s.Name == "tool_loop" {
			t.Errorf("a loop span without a loop: %+v", out.Spans)
		}
	}
}

// What a tool paid a provider for joins the turn's cost once, however the work was split between steps.
func TestPaidToolFeeIsInTheTurnCostOnce(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{call("a", "search__web", `{"query":"news"}`)}}, {Text: "-"}}}
	paying := func(ctx context.Context, tool string, _ map[string]any) ToolResult {
		GuardFrom(ctx).Spend(0.007) // what the application's search does for a service that reports a fee
		GuardFrom(ctx).Paid(0.007)
		return ToolResult{Tool: tool, OK: true, Data: map[string]any{"status": "ok"}}
	}
	req := loopRequest("what happened today?", ToolModeLoop)
	req.Guard = &Guard{}
	out, err := (&Runtime{LLM: llm, Tools: paying}).Answer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	// planner 0.001 + loop 0.0005 × 2 rounds + answer 0.002 + the search fee 0.007
	if want := 0.001 + 0.0005*2 + 0.002 + 0.007; math.Abs(out.CostUSD-want) > 1e-9 {
		t.Fatalf("turn cost %g, want %g", out.CostUSD, want)
	}
	if got := req.Guard.PaidTotal(); math.Abs(got-0.007) > 1e-12 {
		t.Fatalf("paid total %g", got)
	}
}
