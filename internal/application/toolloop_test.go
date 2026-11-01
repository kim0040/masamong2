package application_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/tools"
)

// loopServer is an OpenAI-compatible host for the tool loop: a request carrying tools gets the next
// scripted assistant message (tool calls or text), the planner gets plan, the answer model a reply.
// refuseTools answers every tools request the way a host without tool support does.
type loopServer struct {
	mu          sync.Mutex
	plan        string
	script      []map[string]any // assistant messages for tools requests
	refuseTools bool
	toolReqs    []map[string]any
	plannerReqs []map[string]any
	answerReqs  []map[string]any
}

func (f *loopServer) start(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		defer f.mu.Unlock()
		reply := func(msg map[string]any, finish string) {
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": msg, "finish_reason": finish}},
				"usage": map[string]any{"cost": 0.0001}})
		}
		if _, ok := body["tools"]; ok {
			f.toolReqs = append(f.toolReqs, body)
			if f.refuseTools {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"error":{"message":"This model does not support tool use","type":"invalid_request_error"}}`))
				return
			}
			if len(f.script) == 0 {
				reply(map[string]any{"role": "assistant", "content": "-"}, "stop")
				return
			}
			msg := f.script[0]
			f.script = f.script[1:]
			reply(msg, "tool_calls")
			return
		}
		if body["model"] == "fake/planner" {
			f.plannerReqs = append(f.plannerReqs, body)
			reply(map[string]any{"role": "assistant", "content": f.plan}, "stop")
			return
		}
		f.answerReqs = append(f.answerReqs, body)
		reply(map[string]any{"role": "assistant", "content": "알겠어"}, "stop")
	}))
	t.Cleanup(srv.Close)
	return srv
}

func toolCallMsg(calls ...[2]string) map[string]any {
	var tc []map[string]any
	for i, c := range calls {
		tc = append(tc, map[string]any{"id": "call_" + string(rune('a'+i)), "type": "function",
			"function": map[string]any{"name": c[0], "arguments": c[1]}})
	}
	return map[string]any{"role": "assistant", "content": nil, "tool_calls": tc}
}

func loopSvc(t *testing.T, mode string, f *loopServer) *application.Service {
	svc := setupSvc(t)
	srv := f.start(t)
	t.Setenv("FAKE_LOOP_KEY", "k")
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["openrouter"] = config.Provider{Kind: config.KindOpenAI, BaseURL: srv.URL, CredentialRef: "env:FAKE_LOOP_KEY"}
		d.Providers["openmeteo"] = config.Provider{Kind: config.KindOpenMeteo, Disabled: true} // the weather answers "unconfigured"
		d.Models["planner"] = config.Model{Provider: "openrouter", Model: "fake/planner"}
		d.Models["response"] = config.Model{Provider: "openrouter", Model: "fake/response"}
		d.Memory.Retrieve = "off"
		d.Tools = map[string]tools.Settings{"search.web": {Chat: boolp(false)}}
		d.Chat.ToolMode = mode
	})
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "c", "persona_ref": "friendly_ko"}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	return svc
}

func toolStates(res application.ChatResultForTest) map[string]string {
	out := map[string]string{}
	for _, tr := range res.Tools {
		st := tr.Status
		if st == "" {
			st, _ = tr.Data["status"].(string)
		}
		out[tr.Tool] = st
	}
	return out
}

func turnBody(t *testing.T, svc *application.Service, id string) map[string]any {
	t.Helper()
	turn := out(t, svc, "turns.get", map[string]any{"id": id})["turn"].(map[string]any)
	body, _ := turn["body"].(map[string]any)
	if body == nil {
		t.Fatalf("turn without body: %v", turn)
	}
	return body
}

func TestChatTurnToolLoopRunsToolsThroughTheService(t *testing.T) {
	f := &loopServer{plan: `{"memory_requirement":"none","tool_plan":[{"tool_name":"finance.quote","parameters":{"symbol":"AAPL"}}]}`,
		script: []map[string]any{
			toolCallMsg([2]string{"weather__query", `{"region":"부산","scope":"discord:other"}`}, [2]string{"polls__create", `{"question":"점심 뭐 먹지?"}`}),
			{"role": "assistant", "content": "-"},
		}}
	svc := loopSvc(t, "loop", f)
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "부산 날씨 어때? 점심 투표도 만들어줘",
		Speaker: "영희", SpeakerID: "u1", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	states := toolStates(res)
	// the weather ran through Execute (its provider is off: "unconfigured"); the poll was planned but a
	// dry-run turn makes nothing; the planner's own choice (a quote) did not run in loop mode
	if states["weather.query"] != "unconfigured" || states["polls.create"] != "dry_run" || states["finance.quote"] != "" {
		t.Fatalf("tools %+v", states)
	}
	if len(f.toolReqs) != 2 || len(f.plannerReqs) != 1 {
		t.Fatalf("tool requests %d, planner requests %d", len(f.toolReqs), len(f.plannerReqs))
	}
	first := f.toolReqs[0]
	if first["tool_choice"] != "auto" || first["model"] != "fake/planner" {
		t.Fatalf("loop request %v", first)
	}
	var names []string
	for _, tl := range first["tools"].([]any) {
		fn := tl.(map[string]any)["function"].(map[string]any)
		names = append(names, fn["name"].(string))
		raw, _ := json.Marshal(fn["parameters"])
		if strings.Contains(string(raw), `"scope"`) || strings.Contains(string(raw), "channel_id") {
			t.Fatalf("%s offers scope keys: %s", fn["name"], raw)
		}
	}
	if !slices.IsSorted(names) || slices.Contains(names, "memory__search") || slices.Contains(names, "search__web") {
		t.Fatalf("offered %v", names)
	}
	// round 2 carries the assistant message back and one tool message per call
	msgs := f.toolReqs[1]["messages"].([]any)
	if len(msgs) != 5 || msgs[2].(map[string]any)["tool_calls"] == nil || msgs[3].(map[string]any)["tool_call_id"] != "call_a" {
		t.Fatalf("round 2 messages %v", msgs)
	}
	if c, _ := msgs[3].(map[string]any)["content"].(string); !strings.Contains(c, `"untrusted_data":true`) || strings.Contains(c, "discord:other") {
		t.Fatalf("tool message %q", c)
	}
	// the planner was offered no tools (memory is off here, so nothing at all)
	planner, _ := json.Marshal(f.plannerReqs[0]["messages"])
	if strings.Contains(string(planner), "- weather.query:") || strings.Contains(string(planner), "- finance.quote:") {
		t.Fatalf("planner menu in loop mode: %s", planner)
	}
	body := turnBody(t, svc, res.TurnID)
	loop, _ := body["tool_loop"].(map[string]any)
	if loop == nil || loop["mode"] != "loop" || loop["stop"] != "answered" || len(loop["calls"].([]any)) != 2 {
		t.Fatalf("turn record tool_loop: %v", body["tool_loop"])
	}
}

func TestChatTurnShadowModeRecordsWithoutRunning(t *testing.T) {
	f := &loopServer{plan: `{"memory_requirement":"none","tool_plan":[{"tool_name":"weather.query","parameters":{"region":"서울"}}]}`,
		script: []map[string]any{toolCallMsg([2]string{"weather__query", `{"region":"부산"}`}, [2]string{"polls__create", `{"question":"비 와?"}`})}}
	svc := loopSvc(t, "shadow", f)
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "비 와?",
		Speaker: "영희", SpeakerID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	states := toolStates(res)
	if len(states) != 1 || states["weather.query"] != "unconfigured" || res.Tools[0].Data["region"] != "서울" {
		t.Fatalf("shadow mode ran %+v", res.Tools)
	}
	body := turnBody(t, svc, res.TurnID)
	shadow, _ := body["tool_shadow"].(map[string]any)
	if shadow == nil || shadow["mode"] != "shadow" || body["tool_loop"] != nil {
		t.Fatalf("turn record: %v", body)
	}
	calls := shadow["calls"].([]any)
	if len(calls) != 2 || calls[1].(map[string]any)["tool"] != "polls.create" || calls[1].(map[string]any)["reason"] != "shadow" {
		t.Fatalf("shadow calls %v", calls)
	}
}

func TestChatTurnFallsBackToThePlannerWhenTheModelTakesNoTools(t *testing.T) {
	f := &loopServer{plan: `{"memory_requirement":"none","tool_plan":[{"tool_name":"weather.query","parameters":{"region":"부산"}}]}`, refuseTools: true}
	svc := loopSvc(t, "loop", f)
	for i, id := range []string{"m1", "m2"} {
		res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id, Message: "부산 날씨 어때?",
			Speaker: "영희", SpeakerID: "u1", DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		if toolStates(res)["weather.query"] != "unconfigured" {
			t.Fatalf("turn %d: the planner's weather did not run: %+v", i, res.Tools)
		}
		if res.ToolLoop == nil || res.ToolLoop.Fallback != "unsupported" {
			t.Fatalf("turn %d trace %+v", i, res.ToolLoop)
		}
	}
	// the refusal is remembered: the second turn did not ask with tools again
	if len(f.toolReqs) != 1 {
		t.Fatalf("tools requests %d", len(f.toolReqs))
	}
	// the planner chose with the whole menu (once in the turn that found out, once in the next)
	last, _ := json.Marshal(f.plannerReqs[len(f.plannerReqs)-1]["messages"])
	if !strings.Contains(string(last), "weather.query") {
		t.Fatalf("fallback planner menu: %s", last)
	}
}

// A model that has answered with tools in this process and then refuses them once (a reseller routed
// that request to an upstream without tools) is asked with tools again on the next turn: the refusal
// is that upstream's, and remembering it turned the loop off for every later turn (seen live).
func TestOneRefusalFromAModelThatTookToolsIsNotRemembered(t *testing.T) {
	f := &loopServer{plan: `{"memory_requirement":"none","tool_plan":[]}`}
	svc := loopSvc(t, "loop", f)
	turn := func(id string) application.ChatResultForTest {
		res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: id, Message: "부산 날씨 어때?",
			Speaker: "영희", SpeakerID: "u1", DryRun: true})
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if res := turn("m1"); res.ToolLoop == nil || res.ToolLoop.Fallback != "" {
		t.Fatalf("first turn: %+v", res.ToolLoop)
	}
	f.mu.Lock()
	f.refuseTools = true
	f.mu.Unlock()
	if res := turn("m2"); res.ToolLoop == nil || res.ToolLoop.Fallback != "unsupported" {
		t.Fatalf("refusing turn: %+v", res.ToolLoop)
	}
	f.mu.Lock()
	f.refuseTools = false
	before := len(f.toolReqs)
	f.mu.Unlock()
	if res := turn("m3"); res.ToolLoop == nil || res.ToolLoop.Fallback != "" || len(f.toolReqs) != before+1 {
		t.Fatalf("after one refusal the model must be asked with tools again: %+v, tool requests %d→%d", res.ToolLoop, before, len(f.toolReqs))
	}
}

func TestChatTurnInjectedToolFailure(t *testing.T) {
	f := &loopServer{script: []map[string]any{
		toolCallMsg([2]string{"finance__fx", `{"base":"USD","symbols":"KRW"}`}),
		toolCallMsg([2]string{"finance__fx", `{"base":"usd","symbols":"krw"}`}), // the same call: not run again
	}}
	svc := loopSvc(t, "loop", f)
	defer application.FailChatToolForTest(svc, "c", "finance.fx")()
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", MessageID: "m1", Message: "달러 환율 얼마야?",
		Speaker: "영희", SpeakerID: "u1", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != 1 || res.Tools[0].Status != "unavailable" || res.ToolLoop.Steps[1].Reason != "duplicate" {
		t.Fatalf("tools %+v trace %+v", res.Tools, res.ToolLoop)
	}
}
