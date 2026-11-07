package chat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"math"
	"strings"
	"sync"
	"testing"

	"masamong/internal/memory/engine"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
)

func TestParsePlanStrictAndIntent(t *testing.T) {
	p := ParsePlan("```json\n{\"memory_requirement\":\"required\",\"time\":{\"kind\":\"recent\"},\"question_kind\":\"person_fact\",\"person_targets\":[\"철수가\"],\"semantic_query\":\"철수가 한 말\",\"key_terms\":[\"캠핑\"],\"tool_plan\":[{\"tool_name\":\"weather\",\"parameters\":{\"region\":\"서울\",\"channel_id\":\"x\"}}]}\n```")
	if !p.Parsed || p.MemoryRequirement != "required" || !p.hasTool("memory.search") || !p.hasTool("weather.query") {
		t.Fatalf("%+v", p)
	}
	it, ok := p.Intent()
	if !ok || !it.Recent || it.Kind != "person_fact" || it.Source != "planner" || it.SemanticQuery == "" {
		t.Fatalf("%+v", it)
	}
	a := Assess(koVoice(), p, goldenTools([]string{"weather.query"}), true, true)
	if a.MemoryState != "search" || len(a.Steps) != 1 || a.Steps[0].State != "runnable" || len(a.Stripped) != 1 {
		t.Fatalf("scope keys must be stripped: %+v", a)
	}
	if _, has := a.Steps[0].Parameters["channel_id"]; has {
		t.Fatal("channel_id leaked into tool parameters")
	}
}

func TestDamagedPlanNeverSearchesEverything(t *testing.T) {
	for _, raw := range []string{"", "모르겠어", `{"memory_requirement":"none","tool_plan":[`} {
		p := ParsePlan(raw)
		a := Assess(koVoice(), p, goldenTools([]string{}), true, true)
		if a.MemoryState == "search" {
			t.Fatalf("%q → search", raw)
		}
	}
	p := ParsePlan(`{"memory_requirement":"required","semantic_query":"잘린`)
	if a := Assess(koVoice(), p, goldenTools([]string{}), true, true); a.MemoryState != "search" {
		t.Fatalf("salvaged required should search: %+v", a)
	}
}

func TestSchemaInvalidPlannerObjectsFailClosed(t *testing.T) {
	var logs bytes.Buffer
	previousLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn})))
	defer slog.SetDefault(previousLogger)
	for _, raw := range []string{`{}`, `{"error":"model busy"}`, `{"memory_requirement":false,"tool_plan":"bad"}`} {
		llm := &fakeLLM{plan: raw}
		mem := &fakeMemory{}
		var ran []string
		rt := &Runtime{LLM: llm, Memory: mem, Tools: func(_ context.Context, tool string, _ map[string]any) ToolResult {
			ran = append(ran, tool)
			return ToolResult{Tool: tool, OK: true}
		}}
		res, err := rt.Answer(context.Background(), Request{Message: "hello", Voice: koVoice(), MemoryOn: true, Rooms: []string{"discord:g:c"},
			Tools: goldenTools(nil), PlannerModel: "planner", ResponseModel: "answer"})
		if err != nil {
			t.Fatal(err)
		}
		if res.Plan.Parsed || res.Plan.ParseState != "schema_invalid" || res.Assess.MemoryState != "unparsed" {
			t.Fatalf("%q: plan=%+v assessment=%+v", raw, res.Plan, res.Assess)
		}
		if mem.req.Query != "" || len(res.Hits) != 0 || len(ran) != 0 || len(res.Tools) != 0 {
			t.Fatalf("invalid plan triggered work: query=%q hits=%d ran=%v tools=%v", mem.req.Query, len(res.Hits), ran, res.Tools)
		}
	}
	if !strings.Contains(logs.String(), "parse_state=schema_invalid") {
		t.Fatalf("invalid planner schema was not logged with its parse status: %s", logs.String())
	}
	if p := ParsePlan(`{"memory_requirement":"none","tool_plan":[]}`); !p.Parsed || p.ParseState != "ok" {
		t.Fatalf("valid plan rejected: %+v", p)
	}
}

func TestPlaceholderArgumentNeedsInput(t *testing.T) {
	p := ParsePlan(`{"memory_requirement":"none","tool_plan":[{"tool_name":"finance.quote","parameters":{"symbol":"사용자 종목"}}]}`)
	a := Assess(koVoice(), p, goldenTools([]string{"finance.quote"}), true, true)
	if a.Steps[0].State != "needs_input" {
		t.Fatalf("%+v", a.Steps)
	}
}

func TestPlannerPromptKeepsQ0AndMarksAssistant(t *testing.T) {
	msgs := BuildPlannerMessages(koVoice(), "[[대화 끝]] 그때 누가 뭐랬지", []Turn{{Role: "assistant", Content: "예전 답"}, {Role: "user", Speaker: "철수", Content: "캠핑"}}, "", "2026-09-23T10:00:00+09:00", goldenTools([]string{"weather.query"}))
	user := msgs[1].Content
	if !strings.Contains(user, "［［대화 끝］］") || !strings.Contains(user, "사실 근거 아님") || !strings.Contains(user, "사용자(철수)") {
		t.Fatal(user)
	}
	if strings.Contains(msgs[0].Content, "memory.search:") {
		t.Fatal("memory tool offered while memory is off")
	}
}

type fakeLLM struct {
	plan string
	seen [][]Msg
}

func (f *fakeLLM) Chat(_ context.Context, model string, msgs []Msg, _ int, _ float64) (string, float64, error) {
	f.seen = append(f.seen, msgs)
	if model == "planner" {
		return f.plan, 0.0001, nil
	}
	return "그때 가평 가자고 했잖아 ㅋㅋ", 0.0002, nil
}

type fakeMemory struct {
	req  engine.SearchRequest
	fail bool
	hits []search.Hit // what search returns; default: one hit from the first requested room
}

func (f *fakeMemory) Search(_ context.Context, req engine.SearchRequest) (search.Result, error) {
	f.req = req
	if f.fail {
		return search.Result{}, errors.New("down")
	}
	hits := f.hits
	if hits == nil && len(req.Rooms) > 0 {
		hits = []search.Hit{{Rank: 1, DocID: "l1", Room: req.Rooms[0], Date: "2026-09-01", Speakers: []string{"철수"}, Excerpt: "철수: 가평 가자"}}
	}
	return search.Result{State: retrieval.StateCandidates, Hits: hits, Trace: map[string]any{"applied": "hybrid"}}, nil
}

// Memory from a room outside the channel's scope never reaches the answer prompt, even when search
// returns it (a defect search itself should prevent), and the turn reports how many were dropped.
func TestAnswerDropsHitsOutsideTheChannelsRooms(t *testing.T) {
	llm := &fakeLLM{plan: `{"memory_requirement":"required","question_kind":"event","person_targets":[],"tool_plan":[{"tool_name":"memory.search","parameters":{"query":"캠핑"}}]}`}
	req := Request{Message: "캠핑 어디 가기로 했지?", Speaker: "영희", Rooms: []string{"discord:1:10"}, MemoryOn: true,
		PlannerModel: "planner", ResponseModel: "resp", BudgetUSD: 0.05, Voice: koVoice()}
	mem := &fakeMemory{hits: []search.Hit{
		{Rank: 1, DocID: "own", Room: "discord:1:10", Excerpt: "철수: 가평 가자"},
		{Rank: 2, DocID: "other", Room: "discord:2:20", Excerpt: "민수: 비밀 이야기"},
	}}
	res, err := (&Runtime{LLM: llm, Memory: mem}).Answer(context.Background(), req)
	if err != nil || len(res.Hits) != 1 || res.Hits[0].DocID != "own" || res.ScopeDropped != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if prompt := llm.seen[len(llm.seen)-1][1].Content; strings.Contains(prompt, "비밀 이야기") {
		t.Fatal("another room's memory reached the prompt")
	}
	// only foreign hits: the answer is told nothing came up in its scope
	mem.hits = mem.hits[1:]
	res, _ = (&Runtime{LLM: llm, Memory: mem}).Answer(context.Background(), req)
	if len(res.Hits) != 0 || res.ScopeDropped != 1 || res.RagState != "empty_in_allowed_scope" {
		t.Fatalf("%+v", res)
	}
}

func TestAnswerUsesScopeAndFriendRules(t *testing.T) {
	llm := &fakeLLM{plan: `{"memory_requirement":"required","question_kind":"event","person_targets":[],"tool_plan":[{"tool_name":"memory.search","parameters":{"query":"캠핑"}}]}`}
	mem := &fakeMemory{}
	rt := &Runtime{LLM: llm, Memory: mem}
	res, err := rt.Answer(context.Background(), Request{Message: "캠핑 어디 가기로 했지?", Speaker: "영희", Rooms: []string{"discord:1:10"},
		MemoryOn: true, PlannerModel: "planner", ResponseModel: "resp", BudgetUSD: 0.05, Voice: koVoice()})
	if err != nil || res.Answer == "" || res.RagState != "candidates" || len(res.Hits) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if mem.req.Query != "캠핑 어디 가기로 했지?" || len(mem.req.Rooms) != 1 || mem.req.Intent.MemorySearchQuery != "캠핑" {
		t.Fatalf("q0 must stay verbatim and scope passed: %+v", mem.req)
	}
	answerPrompt := llm.seen[len(llm.seen)-1]
	if !strings.Contains(answerPrompt[0].Content, "친구처럼") || !strings.Contains(answerPrompt[1].Content, "<기억>") {
		t.Fatal("friend rules / memory block missing")
	}
	if res.CostUSD <= 0 {
		t.Fatal("cost not summed")
	}
}

func TestAnswerSurvivesMemoryFailureAndSkipsWhenOff(t *testing.T) {
	llm := &fakeLLM{plan: `{"memory_requirement":"required","tool_plan":[]}`}
	rt := &Runtime{LLM: llm, Memory: &fakeMemory{fail: true}}
	res, err := rt.Answer(context.Background(), Request{Message: "그때 뭐였지", Rooms: []string{"r"}, MemoryOn: true, PlannerModel: "planner", ResponseModel: "resp"})
	if err != nil || res.RagState != "memory_unavailable" || res.Answer == "" {
		t.Fatalf("%+v %v", res, err)
	}
	mem := &fakeMemory{}
	rt = &Runtime{LLM: llm, Memory: mem}
	res, _ = rt.Answer(context.Background(), Request{Message: "그때 뭐였지", Rooms: []string{"r"}, MemoryOn: false, PlannerModel: "planner", ResponseModel: "resp"})
	if res.RagState != "memory_search_disabled" || mem.req.Query != "" {
		t.Fatalf("memory off must not search: %+v", res)
	}
}

func TestToolResultsStayWholeJSONWhenClipped(t *testing.T) {
	long := strings.Repeat("검색 결과 본문 \"인용\" ", 400)
	usable := []ToolResult{
		{Tool: "search.web", OK: true, Data: map[string]any{"results": long}},
		{Tool: "weather.query", OK: true, Status: "ok", Data: map[string]any{"temp_c": 21}},
	}
	other := []ToolResult{{Tool: "images.generate", OK: false, Status: "failed", Error: "quota"}}
	out := toolJSON(usable, other)
	if n := len([]rune(out)); n > toolBlockMax {
		t.Fatalf("over the limit: %d", n)
	}
	var got struct {
		Usable     []map[string]any `json:"usable"`
		NotSuccess []map[string]any `json:"not_success"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("not valid JSON: %v\n%s", err, out)
	}
	if len(got.Usable) != 2 || got.Usable[0]["truncated"] != true || got.Usable[1]["data"] == nil {
		t.Fatalf("each usable tool kept, only the long one clipped: %+v", got.Usable)
	}
	if len(got.NotSuccess) != 1 || got.NotSuccess[0]["error"] != "quota" {
		t.Fatalf("a failure after a long result is kept: %+v", got.NotSuccess)
	}
	// results that fit are rendered as before
	small := toolJSON(usable[1:], nil)
	want, _ := json.Marshal(map[string]any{"usable": usable[1:], "not_success": []ToolResult(nil)})
	if small != string(want) {
		t.Fatalf("small block changed: %s", small)
	}
}

// notingLLM notes each call on the turn's guard, as the application's model adapter does.
type notingLLM struct{ fakeLLM }

func (n *notingLLM) Chat(ctx context.Context, model string, msgs []Msg, mt int, temp float64) (string, float64, error) {
	text, cost, err := n.fakeLLM.Chat(ctx, model, msgs, mt, temp)
	GuardFrom(ctx).Note(cost)
	return text, cost, err
}

// The per-turn limits hold back optional steps once the turn has spent its share or made its calls;
// the reply itself is always made.
func TestGuardSkipsOptionalStepsButAlwaysAnswers(t *testing.T) {
	plan := `{"memory_requirement":"none","tool_plan":[{"tool_name":"images.generate","parameters":{"prompt":"고양이"}},{"tool_name":"weather.query","parameters":{"region":"서울"}}]}`
	run := func(g *Guard) (Result, []string) {
		var ran []string
		var mu sync.Mutex
		tools := func(ctx context.Context, tool string, _ map[string]any) ToolResult {
			mu.Lock()
			ran = append(ran, tool)
			mu.Unlock()
			if tool == "images.generate" {
				GuardFrom(ctx).Spend(0.04) // a picture costs more than a typical turn's share
			}
			return ToolResult{Tool: tool, OK: true, Data: map[string]any{"ok": true}}
		}
		llm := &notingLLM{fakeLLM{plan: plan}}
		menu := goldenTools([]string{"images.generate", "weather.query"})
		for i := range menu.Enabled {
			menu.Enabled[i].Pays = menu.Enabled[i].ID == "images.generate" // as the application marks it
		}
		res, err := (&Runtime{LLM: llm, Tools: tools}).Answer(context.Background(), Request{Message: "고양이 그려주고 서울 날씨도", Speaker: "영희",
			PlannerModel: "planner", ResponseModel: "resp", Voice: koVoice(), Tools: menu, Guard: g})
		if err != nil || res.Answer == "" {
			t.Fatalf("no answer: %+v %v", res, err)
		}
		return res, ran
	}
	if res, ran := run(nil); len(ran) != 2 || len(res.Skipped) != 0 {
		t.Fatalf("no guard: ran %v skipped %v", ran, res.Skipped)
	}
	// the picture runs (the turn had spent next to nothing); the weather after it is held back
	res, ran := run(&Guard{MaxUSD: 0.03})
	if len(ran) != 1 || ran[0] != "images.generate" || len(res.Skipped) != 1 || res.Skipped[0] != "weather.query" {
		t.Fatalf("money cap: ran %v skipped %v", ran, res.Skipped)
	}
	if last := res.Tools[len(res.Tools)-1]; last.Tool != "weather.query" || last.OK || last.Status != "unavailable" {
		t.Fatalf("the answer must hear the skipped tool as unavailable: %+v", last)
	}
	// two calls: the planner and the first tool
	if res, ran := run(&Guard{MaxCalls: 2}); len(ran) != 1 || len(res.Skipped) != 1 {
		t.Fatalf("call cap: ran %v skipped %v", ran, res.Skipped)
	}
	// generous limits change nothing
	if res, ran := run(&Guard{MaxUSD: 1, MaxCalls: 20}); len(ran) != 2 || len(res.Skipped) != 0 {
		t.Fatalf("generous: ran %v skipped %v", ran, res.Skipped)
	}
}

// The memory search is held back by the money cap only: a turn whose calls are used up by the planner
// and its other steps still searches the memory the planner asked for (21 of 21 turns with two web
// searches lost it). A spent money cap still skips it, and the skip is recorded.
func TestMemorySearchIgnoresTheCallCountButNotTheMoneyCap(t *testing.T) {
	run := func(g *Guard) (Result, *fakeMemory) {
		mem := &fakeMemory{}
		llm := &notingLLM{fakeLLM{plan: `{"memory_requirement":"required","question_kind":"event","person_targets":[],"tool_plan":[{"tool_name":"memory.search","parameters":{"query":"캠핑"}}]}`}}
		res, err := (&Runtime{LLM: llm, Memory: mem}).Answer(context.Background(), Request{Message: "캠핑 어디 가기로 했지?", Speaker: "영희",
			Rooms: []string{"discord:1:10"}, MemoryOn: true, PlannerModel: "planner", ResponseModel: "resp", BudgetUSD: 0.05, Voice: koVoice(), Guard: g})
		if err != nil || res.Answer == "" {
			t.Fatalf("no answer: %+v %v", res, err)
		}
		return res, mem
	}
	// one call allowed, and the planner already made it
	res, mem := run(&Guard{MaxCalls: 1})
	if mem.req.Query == "" || res.RagState != "candidates" || len(res.Skipped) != 0 {
		t.Fatalf("call count held the memory search back: %+v", res)
	}
	// the money cap is spent by the planner: the search is skipped and the skip is on record
	res, mem = run(&Guard{MaxUSD: 0.00005})
	if mem.req.Query != "" || res.RagState != "memory_unavailable" || len(res.Skipped) != 1 || res.Skipped[0] != "memory.search" {
		t.Fatalf("money cap must still skip the search: %+v", res)
	}
}

// A long result list loses its last items first: the hits the service ranked first stay whole.
func TestToolResultsKeepTheFirstHitsWhole(t *testing.T) {
	var items []map[string]any
	for i := 0; i < 5; i++ {
		items = append(items, map[string]any{"title": "기사 " + strings.Repeat("제목", 10), "snippet": strings.Repeat(string(rune('가'+i)), 700)})
	}
	usable := []ToolResult{{Tool: "search.web", OK: true, Status: "ok", Data: map[string]any{"results": items, "answer": "요약"}},
		{Tool: "weather.query", OK: true, Data: map[string]any{"temp_c": 18}}}
	out := toolJSON(usable, nil)
	var got struct {
		Usable []struct {
			Tool      string         `json:"tool"`
			Data      map[string]any `json:"data"`
			Truncated bool           `json:"truncated"`
		} `json:"usable"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil || len([]rune(out)) > toolBlockMax {
		t.Fatalf("invalid or too long (%d): %v", len([]rune(out)), err)
	}
	res := got.Usable[0].Data["results"].([]any)
	first := res[0].(map[string]any)["snippet"].(string)
	last, _ := res[len(res)-1].(map[string]any)["snippet"].(string)
	if !got.Usable[0].Truncated || first != strings.Repeat("가", 700) || !strings.HasSuffix(last, "…") || got.Usable[0].Data["answer"] != "요약" {
		t.Fatalf("results %d, first whole %v, last cut %v, truncated %v", len(res), first == strings.Repeat("가", 700), strings.HasSuffix(last, "…"), got.Usable[0].Truncated)
	}
	if got.Usable[1].Data["temp_c"] != float64(18) {
		t.Fatalf("a small result keeps its data: %+v", got.Usable[1])
	}
}

// timedMemory finds nothing inside a time window (as when the planner guessed the wrong year) and
// finds the memory without one. floor: the window search was emptied by the rerank floor instead;
// wideEmpty: nothing without the window either; wideFails: the second search errs.
type timedMemory struct {
	reqs                        []engine.SearchRequest
	floor, wideEmpty, wideFails bool
}

func (f *timedMemory) Search(_ context.Context, req engine.SearchRequest) (search.Result, error) {
	f.reqs = append(f.reqs, req)
	windowed := req.Intent.Recent || req.Intent.Time != nil
	switch {
	case windowed && f.floor:
		return search.Result{State: retrieval.StateFor(true, 0, true), Trace: map[string]any{"abstain_reason": "rerank_floor", "rerank_cost_usd": 0.0004}}, nil
	case windowed || f.wideEmpty:
		return search.Result{State: retrieval.StateFor(true, 0, false), Trace: map[string]any{"abstain_reason": "empty_in_allowed_scope", "rerank_cost_usd": 0.0004}}, nil
	case f.wideFails:
		return search.Result{}, errors.New("down")
	}
	return search.Result{State: retrieval.StateCandidates, Hits: []search.Hit{{Rank: 1, DocID: "l1", Room: req.Rooms[0], Date: "2021-05-01", Excerpt: "철수: 가평 가자"}},
		Trace: map[string]any{"rerank_cost_usd": 0.0003}}, nil
}

// A time window that leaves nothing is searched once more without it; a window that finds memory, a
// search the rerank floor emptied, or a plan without a window is not.
func TestEmptyTimeWindowIsSearchedOnceMoreWithoutIt(t *testing.T) {
	plan := func(time string) string {
		return `{"memory_requirement":"required","question_kind":"event","person_targets":[],` + time + `"tool_plan":[{"tool_name":"memory.search","parameters":{"query":"캠핑"}}]}`
	}
	req := Request{Message: "요즘 캠핑 어디 가기로 했지?", Speaker: "영희", Rooms: []string{"discord:1:10"}, MemoryOn: true,
		PlannerModel: "planner", ResponseModel: "resp", BudgetUSD: 0.05, Voice: koVoice()}
	run := func(p string, mem Memory) (Result, *fakeLLM) {
		llm := &fakeLLM{plan: plan(p)}
		res, err := (&Runtime{LLM: llm, Memory: mem}).Answer(context.Background(), req)
		if err != nil {
			t.Fatal(err)
		}
		return res, llm
	}

	mem := &timedMemory{}
	res, llm := run(`"time_scope":"recent",`, mem)
	if len(mem.reqs) != 2 || len(res.Hits) != 1 || res.RagState != "candidates" || res.Retrieve["time_relaxed"] != true {
		t.Fatalf("searches %d hits %d state %s trace %v", len(mem.reqs), len(res.Hits), res.RagState, res.Retrieve)
	}
	if !mem.reqs[0].Intent.Recent || mem.reqs[1].Intent.Recent || mem.reqs[1].Intent.Time != nil || !res.Intent.Recent {
		t.Fatalf("first search keeps the window, the second drops it, the turn's intent is unchanged: %+v", mem.reqs)
	}
	if got := res.Retrieve["rerank_cost_usd"].(float64); math.Abs(got-0.0007) > 1e-9 {
		t.Fatalf("trace shows both reranks: %v", got)
	}
	if math.Abs(res.CostUSD-(0.0001+0.0002+0.0007)) > 1e-9 {
		t.Fatalf("plan + answer + both reranks, each once: %v", res.CostUSD)
	}
	if mem.reqs[1].RemainingUSD >= mem.reqs[0].RemainingUSD {
		t.Fatal("the second search spends what the first left")
	}
	if strings.Contains(llm.seen[len(llm.seen)-1][1].Content, "recent") {
		t.Fatal("the answer is told 'recent' about memory found outside the window")
	}

	// an explicit range the planner resolved wrong (e.g. the wrong year) is relaxed the same way
	mem = &timedMemory{}
	res, _ = run(`"time":{"kind":"between","from":"2025-05-01","to":"2025-05-31"},`, mem)
	if len(mem.reqs) != 2 || mem.reqs[0].Intent.Time == nil || len(res.Hits) != 1 {
		t.Fatalf("between: searches %d hits %d", len(mem.reqs), len(res.Hits))
	}

	// nothing without the window either: still empty, marked as tried
	mem = &timedMemory{wideEmpty: true}
	res, _ = run(`"time_scope":"recent",`, mem)
	if len(mem.reqs) != 2 || res.RagState != "empty_in_allowed_scope" || res.Retrieve["time_relaxed"] != true {
		t.Fatalf("wide empty: %d %s %v", len(mem.reqs), res.RagState, res.Retrieve)
	}

	// the second search fails: the first (empty) result stands, its cost counted once
	mem = &timedMemory{wideFails: true}
	res, _ = run(`"time_scope":"recent",`, mem)
	if len(mem.reqs) != 2 || res.RagState != "empty_in_allowed_scope" || res.Retrieve["time_relaxed"] != nil || math.Abs(res.CostUSD-(0.0001+0.0002+0.0004)) > 1e-9 {
		t.Fatalf("wide fails: %d %s %v %v", len(mem.reqs), res.RagState, res.Retrieve, res.CostUSD)
	}

	// the rerank floor emptied it: relevance, not the window — no second search
	mem = &timedMemory{floor: true}
	run(`"time_scope":"recent",`, mem)
	if len(mem.reqs) != 1 {
		t.Fatalf("floor: %d searches", len(mem.reqs))
	}

	// no window: one search, even when it is empty
	mem = &timedMemory{wideEmpty: true}
	res, _ = run(``, mem)
	if len(mem.reqs) != 1 || res.Retrieve["time_relaxed"] != nil {
		t.Fatalf("no window: %d %v", len(mem.reqs), res.Retrieve)
	}
}
