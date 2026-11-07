package chat

import (
	"context"
	"slices"
	"testing"
)

// finalRequest marks the tools the product marks Final (tools.Builtin): the weather and a quote.
func finalRequest(msg string, mode string) Request {
	req := loopRequest(msg, ToolModeLoop)
	req.LoopFinal = mode
	for i, t := range req.Tools.Enabled {
		req.Tools.Enabled[i].Final = t.ID == "weather.query" || t.ID == "finance.quote"
	}
	return req
}

func finalAsks(t *testing.T, req Request, llm *loopLLM, tools ToolRunner) (Result, int) {
	t.Helper()
	out, err := (&Runtime{LLM: llm, Tools: tools}).Answer(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	return out, len(llm.offered)
}

// One tool whose result is the answer: the model is not asked again.
func TestFinalRoundEndsTheLoop(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{call("a", "weather__query", `{"region":"Busan"}`)}}}}
	rec := &recorder{}
	out, asks := finalAsks(t, finalRequest("weather in Busan?", ""), llm, rec.run)
	if asks != 1 || out.ToolLoop.Rounds != 1 || out.ToolLoop.Stop != "final" {
		t.Fatalf("asked %d times, trace %+v", asks, out.ToolLoop)
	}
	if len(out.Tools) != 1 || !out.Tools[0].OK || out.Tools[0].Tool != "weather.query" {
		t.Fatalf("tools %+v", out.Tools)
	}
}

// A message that asks for several things gets them in one round, all of them run and reach the answer.
func TestFinalRoundKeepsEveryCallOfTheRound(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{call("a", "weather__query", `{"region":"Busan"}`), call("b", "finance__quote", `{"symbol":"AAPL"}`)}}}}
	rec := &recorder{}
	out, asks := finalAsks(t, finalRequest("weather in Busan and the apple stock?", ""), llm, rec.run)
	if asks != 1 || out.ToolLoop.Stop != "final" {
		t.Fatalf("asked %d times, trace %+v", asks, out.ToolLoop)
	}
	if got := rec.tools(); len(got) != 2 || len(out.Tools) != 2 || !out.Tools[0].OK || !out.Tools[1].OK {
		t.Fatalf("ran %v, tools %+v", got, out.Tools)
	}
}

// What a next round would have asked because of a result is given up: the price of the option, and why
// chat.loop_final can be turned off. Models that plan in steps show here (scenarios_multi_ko.json).
func TestFinalRoundDoesNotAskForAStepThatDependsOnItsResult(t *testing.T) {
	replies := func() []LoopReply {
		return []LoopReply{{Calls: []LoopCall{call("a", "weather__query", `{"region":"Busan"}`)}}, {Calls: []LoopCall{call("b", "search__web", `{"query":"indoor dates"}`)}}, {Text: "-"}}
	}
	rec := &recorder{}
	if _, asks := finalAsks(t, finalRequest("if it rains in Busan find me an indoor date", ""), &loopLLM{replies: replies()}, rec.run); asks != 1 || len(rec.tools()) != 1 {
		t.Fatalf("default: asked %d times, ran %v", asks, rec.tools())
	}
	rec = &recorder{}
	if _, asks := finalAsks(t, finalRequest("if it rains in Busan find me an indoor date", LoopFinalOff), &loopLLM{replies: replies()}, rec.run); asks != 3 || len(rec.tools()) != 2 {
		t.Fatalf("off: asked %d times, ran %v", asks, rec.tools())
	}
}

// Anything but a found answer from every call of the round means there is more to do: the model is asked
// again, as it was before this option, so it can try another source or finish what it started.
func TestFinalRoundWantsEveryCallToHaveFoundIt(t *testing.T) {
	ok := func(data map[string]any) ToolRunner {
		return func(_ context.Context, tool string, _ map[string]any) ToolResult {
			return ToolResult{Tool: tool, OK: true, Data: data}
		}
	}
	weather := call("a", "weather__query", `{"region":"Busan"}`)
	quote := call("b", "finance__quote", `{"symbol":"AAPL"}`)
	for name, c := range map[string]struct {
		calls []LoopCall
		tools ToolRunner
		mode  string
		mark  func(*Request)
	}{
		"failed":            {calls: []LoopCall{weather}, tools: (&recorder{fail: map[string]bool{"weather.query": true}}).run},
		"one of two failed": {calls: []LoopCall{weather, quote}, tools: (&recorder{fail: map[string]bool{"finance.quote": true}}).run},
		// the statuses of internal/application: features.go (places, weather, quote) and chatView
		"no_results": {calls: []LoopCall{weather}, tools: ok(map[string]any{"status": "no_results"})},
		"no_area":    {calls: []LoopCall{weather}, tools: ok(map[string]any{"status": "no_area"})},
		"empty":      {calls: []LoopCall{weather}, tools: ok(map[string]any{"status": "empty"})},
		"stale":      {calls: []LoopCall{weather}, tools: ok(map[string]any{"status": "stale"})},
		"partial":    {calls: []LoopCall{quote}, tools: ok(map[string]any{"status": "partial"})},
		// a quote the service could not give, answered by a web search (quoteBySearch)
		"a search's answer": {calls: []LoopCall{quote}, tools: ok(map[string]any{"status": "ok", "live": false, "source": "search"})},
		"queued status": {calls: []LoopCall{weather}, tools: func(_ context.Context, tool string, _ map[string]any) ToolResult {
			return ToolResult{Tool: tool, OK: true, Status: "queued"}
		}},
		"beside a tool that is not final": {calls: []LoopCall{weather, call("c", "search__web", `{"query":"x"}`)}, tools: (&recorder{}).run},
		"beside a refused call":           {calls: []LoopCall{weather, call("c", "nope", `{}`)}, tools: (&recorder{}).run},
		"beside bad arguments":            {calls: []LoopCall{weather, call("c", "finance__quote", `{"symbol":`)}, tools: (&recorder{}).run},
		"beside a missing value":          {calls: []LoopCall{weather, call("c", "finance__quote", `{"symbol":"unknown"}`)}, tools: (&recorder{}).run},
		"off":                             {calls: []LoopCall{weather}, tools: (&recorder{}).run, mode: LoopFinalOff},
	} {
		t.Run(name, func(t *testing.T) {
			llm := &loopLLM{replies: []LoopReply{{Calls: c.calls}, {Text: "-"}}}
			out, asks := finalAsks(t, finalRequest("q", c.mode), llm, c.tools)
			if asks != 2 || out.ToolLoop.Stop != "answered" {
				t.Fatalf("asked %d times, stop %q", asks, out.ToolLoop.Stop)
			}
		})
	}
}

// A round over the call cap holds a call back: it did not find anything, so the model hears of it.
func TestFinalRoundWantsNoCallHeldBack(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{call("a", "weather__query", `{"region":"Busan"}`), call("b", "finance__quote", `{"symbol":"AAPL"}`)}}, {Text: "-"}}}
	req := finalRequest("q", "")
	req.Guard = &Guard{MaxCalls: 1}
	out, asks := finalAsks(t, req, llm, (&recorder{}).run)
	if asks != 2 || out.Tools[1].Status != "unavailable" {
		t.Fatalf("asked %d times, tools %+v", asks, out.Tools)
	}
}

// Shadow mode only records: nothing ran, so nothing was found.
func TestFinalRoundLeavesShadowModeAlone(t *testing.T) {
	llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{call("a", "weather__query", `{"region":"Busan"}`)}}, {Text: "-"}}}
	req := finalRequest("q", "")
	req.ToolMode = ToolModeShadow
	out, _ := finalAsks(t, req, llm, (&recorder{}).run)
	if out.ToolLoop.Stop != "answered" || out.ToolLoop.Rounds != 2 {
		t.Fatalf("trace %+v", out.ToolLoop)
	}
}

// searchResults is search.web's result (features.go search) with n items of text.
func searchResults(n int) map[string]any {
	items := make([]any, 0, n)
	for range n {
		items = append(items, map[string]any{"title": "t", "snippet": "text", "url": "https://example.com", "datetime": ""})
	}
	return map[string]any{"status": "ok", "live": true, "source": "brave", "provider": "brave", "query": "q", "results": items, "paid_provider_call": true}
}

// chat.loop_final: search lets a web search that found enough settle the turn, and only then. Off by
// default: a question may want to read on or search again, in words nothing here can read.
func TestSearchSettlesTheTurnOnlyWhenAskedAndEnough(t *testing.T) {
	blank := searchResults(3)
	for _, it := range blank["results"].([]any) {
		it.(map[string]any)["snippet"] = " "
	}
	empty := searchResults(0)
	empty["status"] = "empty"
	web := call("a", "search__web", `{"query":"q"}`)
	for name, c := range map[string]struct {
		mode string
		msg  string
		data map[string]any
		call LoopCall
		asks int
	}{
		"enough":                    {mode: LoopFinalSearch, msg: "q", data: searchResults(searchEnough), call: web, asks: 1},
		"more than enough":          {mode: LoopFinalSearch, msg: "q", data: searchResults(5), call: web, asks: 1},
		"off by default":            {mode: "", msg: "q", data: searchResults(5), call: web, asks: 2},
		"tools":                     {mode: LoopFinalTools, msg: "q", data: searchResults(5), call: web, asks: 2},
		"too few":                   {mode: LoopFinalSearch, msg: "q", data: searchResults(searchEnough - 1), call: web, asks: 2},
		"no text":                   {mode: LoopFinalSearch, msg: "q", data: blank, call: web, asks: 2},
		"nothing found":             {mode: LoopFinalSearch, msg: "q", data: empty, call: web, asks: 2},
		"a deep look":               {mode: LoopFinalSearch, msg: "q", data: searchResults(5), call: call("a", "search__web", `{"query":"q","depth":"deep"}`), asks: 2},
		"a standard look":           {mode: LoopFinalSearch, msg: "q", data: searchResults(5), call: call("a", "search__web", `{"query":"q","depth":"standard"}`), asks: 1},
		"a link":                    {mode: LoopFinalSearch, msg: "what does https://example.com/a say", data: searchResults(5), call: web, asks: 2},
		"a link without its scheme": {mode: LoopFinalSearch, msg: "www.example.com 내용 뭐야", data: searchResults(5), call: web, asks: 2},
	} {
		t.Run(name, func(t *testing.T) {
			req := finalRequest(c.msg, c.mode)
			for i, ti := range req.Tools.Enabled {
				if ti.ID == "search.web" {
					req.Tools.Enabled[i].Params = append(req.Tools.Enabled[i].Params, ToolParam{Name: "depth"})
				}
			}
			llm := &loopLLM{replies: []LoopReply{{Calls: []LoopCall{c.call}}, {Text: "-"}}}
			_, asks := finalAsks(t, req, llm, func(_ context.Context, tool string, _ map[string]any) ToolResult {
				return ToolResult{Tool: tool, OK: true, Data: c.data}
			})
			if asks != c.asks {
				t.Fatalf("asked %d times, want %d", asks, c.asks)
			}
		})
	}
}

// With search settling turns, a search beside a weather call ends the loop when both found theirs, and a
// page to read beside it does not.
func TestSearchSettlesBesideOtherTools(t *testing.T) {
	tools := func(_ context.Context, tool string, _ map[string]any) ToolResult {
		if tool == "search.web" {
			return ToolResult{Tool: tool, OK: true, Data: searchResults(5)}
		}
		return ToolResult{Tool: tool, OK: true, Data: map[string]any{"status": "ok"}}
	}
	both := []LoopCall{call("a", "search__web", `{"query":"q"}`), call("b", "weather__query", `{"region":"Busan"}`)}
	llm := &loopLLM{replies: []LoopReply{{Calls: both}, {Text: "-"}}}
	if out, asks := finalAsks(t, finalRequest("q", LoopFinalSearch), llm, tools); asks != 1 || len(out.Tools) != 2 {
		t.Fatalf("asked %d times, tools %+v", asks, out.Tools)
	}
	req := finalRequest("q", LoopFinalSearch)
	req.Tools.Enabled = append(req.Tools.Enabled, ToolInfo{ID: "web.read", Description: "read", Web: true, Required: []string{"url"}, Params: []ToolParam{{Name: "url", Required: true}}})
	req.Tools.Known["web.read"] = true
	llm = &loopLLM{replies: []LoopReply{{Calls: []LoopCall{both[0], call("c", "web__read", `{"url":"https://example.com"}`)}}, {Text: "-"}}}
	if _, asks := finalAsks(t, req, llm, tools); asks != 2 {
		t.Fatalf("asked %d times beside a page to read", asks)
	}
	if !slices.Contains(llm.offered[1], "search__web") {
		t.Fatalf("offered %v", llm.offered[1])
	}
}
