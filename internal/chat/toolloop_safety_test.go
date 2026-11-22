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
