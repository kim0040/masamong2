package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/tools"
)

type liveScenario struct {
	ID          string                       `json:"id"`
	Category    string                       `json:"category"`
	Turns       []string                     `json:"turns"`
	ExpectTools []string                     `json:"expect_tools"`
	NoTool      bool                         `json:"no_tool"`
	Params      map[string]map[string]string `json:"params"`
	AnswerAny   []string                     `json:"answer_any"`
	AnswerNone  []string                     `json:"answer_none"`
	NoMemory    bool                         `json:"no_memory"` // a channel with no memory attached
	// Command runs an action first, as a "!" or slash command would, and the last turn's message goes
	// through CommandToChat with its result; ExpectCard means the command's own card should stay.
	Command    *scenarioCommand `json:"command"`
	ExpectCard bool             `json:"expect_card"`
	// ForbidTools may not be planned or run; ExpectAsk means the reply asks back (a question mark, and
	// no tool but memory.search ran); FailTools answer "unavailable" in this scenario's channel, to see
	// whether another source is tried.
	ForbidTools []string `json:"forbid_tools"`
	ExpectAsk   bool     `json:"expect_ask"`
	FailTools   []string `json:"fail_tools"`
	// ExpectAllTools must all be planned or run (a message that asks for several things).
	ExpectAllTools []string `json:"expect_all_tools"`
}

type scenarioCommand struct {
	Action string         `json:"action"`
	Input  map[string]any `json:"input"`
}

type scenarioOutcome struct {
	ID       string           `json:"id"`
	Category string           `json:"category"`
	Pass     bool             `json:"pass"`
	Problems []string         `json:"problems,omitempty"`
	Tools    []string         `json:"tools"`
	Plan     []map[string]any `json:"plan"`
	Answer   string           `json:"answer"`
	Intent   string           `json:"intent_source"`
	RagState string           `json:"rag_state"`
	Millis   int              `json:"ms"`
	Timing   map[string]int   `json:"timing_ms"`
	Parse    string           `json:"plan_parse"`
	CostUSD  float64          `json:"cost_usd"`
	Loop     *chat.LoopTrace  `json:"tool_loop,omitempty"`
	// Search is what each web search turned out as: the service that answered, the depth and topic asked,
	// and the services passed over (with why), so a run shows which one carried the answers.
	Search []map[string]any `json:"search,omitempty"`
}

// TestLiveScenarios runs the usage scenarios (testdata/scenarios_ko.json) through the whole product
// on real models and services: the planner picks tools, the tools call the real APIs, the answer model
// writes the reply, and memory questions are answered from the practice sample. Each scenario has its
// own channel, so follow-ups ("그럼 내일은?") see only their own conversation. It spends cents and
// needs the key folder TestLiveResellers uses (MASAMONG_LIVE_KEYS_DIR); MASAMONG_SCENARIO_REPORT names
// a JSON file for the full outcome:
//
//	MASAMONG_LIVE_KEYS_DIR=~/keys MASAMONG_SCENARIO_REPORT=/tmp/report.json \
//	  go test ./internal/application -run TestLiveScenarios -v -timeout 30m
//
// MASAMONG_LIVE_SEARCH picks the search services (key file names, in the order tried) and sets
// web_search.order to them, so a run goes through the services as production does:
// MASAMONG_LIVE_SEARCH=exa,tinyfish,linkup,kakao. A service without a key file is left out. Without it
// the run uses linkup and kakao, as before. Each scenario's report lists which service answered.
//
// MASAMONG_SCENARIO_FILE runs another file (a held-out set written after the fixes, to check they were
// not fitted to these sentences); MASAMONG_SCENARIO_DROP leaves services out ("finnhub,kakao") to see
// how the bot talks when its tools have no answer.
//
// The tool regression sets (testdata/tools_regression_ko.json, _en.json) run the same way, and
// MASAMONG_TOOL_MODE (planner | shadow | loop) sets chat.tool_mode for the run, so the two ways of
// picking tools can be compared on the same sentences:
//
//	MASAMONG_LIVE_KEYS_DIR=~/keys MASAMONG_TOOL_MODE=loop \
//	  MASAMONG_SCENARIO_FILE=testdata/tools_regression_ko.json MASAMONG_SCENARIO_REPORT=/tmp/loop-ko.json \
//	  go test ./internal/application -run TestLiveScenarios -v -timeout 30m
func TestLiveScenarios(t *testing.T) {
	dir := os.Getenv("MASAMONG_LIVE_KEYS_DIR")
	if dir == "" || os.Getenv("CI") != "" {
		t.Skip("set MASAMONG_LIVE_KEYS_DIR to a folder of key files")
	}
	t.Setenv("MASAMONG_OFFLINE", "")
	file := filepath.Join("testdata", "scenarios_ko.json")
	if f := os.Getenv("MASAMONG_SCENARIO_FILE"); f != "" {
		file = f
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	var suite struct {
		Forbid      []string       `json:"forbid_in_every_answer"`
		Scenarios   []liveScenario `json:"scenarios"`
		Locale      string         `json:"locale"`       // default ko
		Speaker     string         `json:"speaker"`      // default 영희
		Sample      string         `json:"sample"`       // practice chat used as memory, default sample-ko.csv
		EnableTools []string       `json:"enable_tools"` // opt-in tools switched on for the run
	}
	if err := json.Unmarshal(raw, &suite); err != nil {
		t.Fatal(err)
	}
	if suite.Locale == "" {
		suite.Locale = "ko"
	}
	if suite.Speaker == "" {
		suite.Speaker = "영희"
	}
	if suite.Sample == "" {
		suite.Sample = "sample-ko.csv"
	}
	if only := os.Getenv("MASAMONG_SCENARIO_ONLY"); only != "" {
		var keep []liveScenario
		for _, sc := range suite.Scenarios {
			if strings.Contains(","+only+",", ","+sc.ID+",") || strings.Contains(","+only+",", ","+sc.Category+",") {
				keep = append(keep, sc)
			}
		}
		suite.Scenarios = keep
	}
	svc := setupSvc(t)
	step := func(act string, in map[string]any) map[string]any {
		t.Helper()
		r := exec(svc, act, in)
		if !r.OK {
			t.Fatalf("%s: %+v", act, r.Error)
		}
		m, _ := jsonRoundTrip(r.Output)
		return m
	}
	// the LLM host: cometapi by default; MASAMONG_LIVE_HOST=openrouter runs the sandbox recipe (its
	// embedding and the Voyage rerank, which cometapi has no endpoint for)
	host := os.Getenv("MASAMONG_LIVE_HOST")
	if host == "" {
		host = "cometapi"
	}
	if _, ok := svc.Doc().Providers[host]; !ok { // the default config already names openrouter
		step("providers.add", map[string]any{"host": host})
	}
	drop := "," + os.Getenv("MASAMONG_SCENARIO_DROP") + ","
	searchIDs := []string{"linkup", "kakao"}
	if v := os.Getenv("MASAMONG_LIVE_SEARCH"); v != "" {
		searchIDs = nil
		for _, id := range strings.Split(v, ",") {
			if id = strings.TrimSpace(id); id == "" || strings.Contains(drop, ","+id+",") {
				continue
			}
			if _, err := os.Stat(filepath.Join(dir, id)); err != nil {
				t.Logf("search service %s has no key file, left out", id)
				continue
			}
			if _, ok := svc.Doc().Providers[id]; !ok {
				t.Fatalf("MASAMONG_LIVE_SEARCH: %q is not a provider id", id)
			}
			searchIDs = append(searchIDs, id)
		}
	}
	for _, id := range append(append([]string{host}, searchIDs...), "kma", "finnhub", "exchangerate") {
		if strings.Contains(drop, ","+id+",") {
			continue
		}
		step("providers.credentials", map[string]any{"id": id, "credential_ref": "file:" + filepath.Join(dir, id)})
	}
	step("models.recommend", map[string]any{"provider": host, "apply": true})
	// MASAMONG_LIVE_CHAT_HOST=anthropic moves the chat roles (planner, response, summary) to another
	// listed host, keeping the first host's embedding and rerank for memory
	if chat := os.Getenv("MASAMONG_LIVE_CHAT_HOST"); chat != "" && chat != host {
		if _, ok := svc.Doc().Providers[chat]; !ok {
			step("providers.add", map[string]any{"host": chat})
		}
		step("providers.credentials", map[string]any{"id": chat, "credential_ref": "file:" + filepath.Join(dir, chat)})
		step("models.recommend", map[string]any{"provider": chat, "apply": true})
	}
	applyLocale(t, svc, suite.Locale)
	ops := []any{}
	if os.Getenv("MASAMONG_LIVE_SEARCH") != "" && len(searchIDs) > 0 {
		order := make([]any, len(searchIDs))
		for i, id := range searchIDs {
			order[i] = id
		}
		ops = append(ops, map[string]any{"path": []any{"web_search", "order"}, "value": order})
		t.Logf("web_search.order = %v", searchIDs)
	}
	for _, id := range suite.EnableTools {
		ops = append(ops, map[string]any{"path": []any{"tools", id, "enabled"}, "value": true})
	}
	if mode := os.Getenv("MASAMONG_TOOL_MODE"); mode != "" {
		ops = append(ops, map[string]any{"path": []any{"chat", "tool_mode"}, "value": mode})
	}
	// MASAMONG_LOOP_FINAL (tools | search | off) sets chat.loop_final, to compare the rounds it saves
	// with what the answers lose
	if final := os.Getenv("MASAMONG_LOOP_FINAL"); final != "" {
		ops = append(ops, map[string]any{"path": []any{"chat", "loop_final"}, "value": final})
	}
	// MASAMONG_LIVE_RESPONSE_MODEL swaps the answer model on the same host, to compare models on the
	// same sentences
	if m := os.Getenv("MASAMONG_LIVE_RESPONSE_MODEL"); m != "" {
		ops = append(ops, map[string]any{"path": []any{"models", "response", "model"}, "value": m})
	}
	// MASAMONG_LIVE_PLANNER_MODEL does the same for the model that picks tools
	if m := os.Getenv("MASAMONG_LIVE_PLANNER_MODEL"); m != "" {
		ops = append(ops, map[string]any{"path": []any{"models", "planner", "model"}, "value": m})
	}
	// MASAMONG_LIVE_PLANNER_EFFORT sets models.planner.effort (none, low, ...) to compare thinking levels
	if e := os.Getenv("MASAMONG_LIVE_PLANNER_EFFORT"); e != "" {
		ops = append(ops, map[string]any{"path": []any{"models", "planner", "effort"}, "value": e})
	}
	// MASAMONG_LIVE_BYOK=anthropic=anthropic[,family=upstream] keeps those model families on OpenRouter
	// to your own provider key (providers.<host>.byok_only)
	for _, route := range strings.Split(os.Getenv("MASAMONG_LIVE_BYOK"), ",") {
		if family, upstream, ok := strings.Cut(strings.TrimSpace(route), "="); ok {
			ops = append(ops, map[string]any{"path": []any{"providers", host, "byok_only", family}, "value": upstream})
		}
	}
	if len(ops) > 0 {
		step("config.patch", map[string]any{"ops": ops})
	}
	t.Logf("chat.tool_mode = %s, response = %s, planner = %s (effort %q)", svc.Doc().Chat.Mode(), svc.Doc().Models["response"].Model, svc.Doc().Models["planner"].Model, svc.Doc().Models["planner"].Effort)
	// memory: the practice sample chat, indexed with the real embedding model
	sample, _ := filepath.Abs(filepath.Join("practice", suite.Sample))
	step("imports.start", map[string]any{"source": "kakao_csv", "path": sample, "room": "kakao:scenario"})
	step("memory.blocks.prepare", map[string]any{"source_room": "kakao:scenario", "budget_usd": "0.5"})
	blockID := ""
	for _, b := range step("memory.blocks.list", nil)["blocks"].([]any) {
		if m := b.(map[string]any); m["source_room"] == "kakao:scenario" {
			blockID, _ = m["block_id"].(string)
		}
	}
	if blockID == "" {
		t.Fatal("the sample's memory block is missing")
	}
	for i, sc := range suite.Scenarios {
		ch := fmt.Sprintf("s%02d", i)
		step("channels.configure", map[string]any{"guild_id": "g1", "id": ch, "respond": "always", "memory_use": "on"})
		if !sc.NoMemory {
			step("channels.view.attach", map[string]any{"guild_id": "g1", "channel_id": ch, "block_id": blockID})
		}
		// a little activity for the ranking question
		for j, who := range []string{"영희", "영희", "철수"} {
			svc.Observe(application.ObservedMessage{GuildID: "g1", ChannelID: ch, MessageID: fmt.Sprint(ch, j), AuthorID: fmt.Sprint(1000 + len(who)), AuthorName: who, At: time.Now()})
		}
	}
	outcomes := make([]scenarioOutcome, len(suite.Scenarios))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for i, sc := range suite.Scenarios {
		wg.Add(1)
		go func(i int, sc liveScenario) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			ch := fmt.Sprintf("s%02d", i)
			for _, tool := range sc.FailTools {
				defer application.FailChatToolForTest(svc, ch, tool)()
			}
			outcomes[i] = runScenario(svc, ch, sc, suite.Forbid, suite.Locale, suite.Speaker)
		}(i, sc)
	}
	wg.Wait()
	pass := 0
	byCat := map[string][2]int{}
	for _, o := range outcomes {
		c := byCat[o.Category]
		c[1]++
		if o.Pass {
			pass++
			c[0]++
		}
		byCat[o.Category] = c
		mark := "PASS"
		if !o.Pass {
			mark = "FAIL"
		}
		t.Logf("%s %-24s tools=%v %dms %v %s", mark, o.ID, o.Tools, o.Millis, o.Timing, strings.Join(o.Problems, "; "))
		for _, sr := range o.Search {
			t.Logf("   search.web -> %v (depth %v, topic %v, %v results, passed over %v)", sr["provider"], sr["depth"], sr["topic"], sr["results"], sr["passed_over"])
		}
	}
	cats := make([]string, 0, len(byCat))
	for c := range byCat {
		cats = append(cats, c)
	}
	sort.Strings(cats)
	for _, c := range cats {
		t.Logf("category %-12s %d/%d", c, byCat[c][0], byCat[c][1])
	}
	t.Logf("scenarios passed: %d/%d", pass, len(outcomes))
	if path := os.Getenv("MASAMONG_SCENARIO_REPORT"); path != "" {
		raw, _ := json.MarshalIndent(outcomes, "", " ")
		_ = os.WriteFile(path, raw, 0o600)
	}
}

func asSlice(v any) []any {
	s, _ := v.([]any)
	return s
}

func applyLocale(t *testing.T, svc *application.Service, loc string) {
	t.Helper()
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"i18n", "default_locale"}, "value": loc}}}); !r.OK {
		t.Fatalf("locale: %+v", r.Error)
	}
}

func runScenario(svc *application.Service, channel string, sc liveScenario, forbid []string, locale, speaker string) scenarioOutcome {
	o := scenarioOutcome{ID: sc.ID, Category: sc.Category, Tools: []string{}}
	started := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	var res application.ChatResultForTest
	if sc.Command != nil {
		raw, _ := json.Marshal(sc.Command.Input)
		cmd := svc.Execute(ctx, action.Request{Action: sc.Command.Action, Input: raw, Locale: locale, RequestID: "scenario",
			Invoker: &action.Invoker{Via: "discord", GuildID: "g1", ChannelID: channel, UserID: "1003"}})
		out, ok := svc.CommandToChat(ctx, cmd, application.ChatInput{GuildID: "g1", ChannelID: channel, MessageID: channel + "c",
			Message: sc.Turns[len(sc.Turns)-1], Speaker: speaker, SpeakerID: "1003", DryRun: true})
		o.Millis = int(time.Since(started).Milliseconds())
		if ok == sc.ExpectCard {
			st, _ := cmd.Output["status"].(string)
			o.Problems = append(o.Problems, fmt.Sprintf("carried on into chat = %v (command ok=%v status=%q)", ok, cmd.OK, st))
		}
		if !ok {
			o.Pass = len(o.Problems) == 0
			return o
		}
		res = out
		return checkScenario(o, res, sc, forbid)
	}
	for i, msg := range sc.Turns {
		// earlier turns of a follow-up are real (kept as history); the last one is a dry run, so a
		// poll, a reminder or a picture is planned but not made
		last := i == len(sc.Turns)-1
		r, err := svc.ChatTurn(ctx, application.ChatInput{GuildID: "g1", ChannelID: channel, MessageID: fmt.Sprint(channel, i), Message: msg,
			Speaker: speaker, SpeakerID: "1003", DryRun: last})
		if err != nil {
			o.Problems = append(o.Problems, "error: "+err.Error())
			o.Millis = int(time.Since(started).Milliseconds())
			return o
		}
		res = r
	}
	o.Millis = int(time.Since(started).Milliseconds())
	return checkScenario(o, res, sc, forbid)
}

func checkScenario(o scenarioOutcome, res application.ChatResultForTest, sc liveScenario, forbid []string) scenarioOutcome {
	o.Answer, o.RagState, o.Intent, o.CostUSD, o.Timing, o.Parse = res.Answer, res.RagState, res.Intent.Source, res.CostUSD, res.Timing, res.Plan.ParseState
	for _, tr := range res.Tools {
		o.Tools = append(o.Tools, tr.Tool)
		if tr.Tool == "search.web" {
			o.Search = append(o.Search, map[string]any{"ok": tr.OK, "provider": tr.Data["provider"], "depth": tr.Data["depth"], "topic": tr.Data["topic"],
				"results": len(asSlice(tr.Data["results"])), "passed_over": tr.Data["passed_over"], "error": tr.Error})
		}
	}
	for _, st := range res.Plan.ToolPlan {
		o.Plan = append(o.Plan, map[string]any{"tool": st.Tool, "parameters": st.Parameters})
	}
	planned := map[string]map[string]any{}
	for _, st := range res.Plan.ToolPlan {
		planned[st.Tool] = st.Parameters
	}
	// in loop mode the tools are the loop's calls; a shadow trace is only recorded (the planner's
	// choices are what ran), and a refused call did not happen
	o.Loop = res.ToolLoop
	if l := res.ToolLoop; l != nil && l.Mode == chat.ToolModeLoop {
		for _, st := range l.Steps {
			o.Plan = append(o.Plan, map[string]any{"tool": st.Tool, "parameters": st.Params, "source": "loop", "state": st.State})
			if st.State == "ran" || st.State == "failed" {
				planned[st.Tool] = st.Params
			}
		}
	}
	used := func(tool string) bool {
		_, ok := planned[tool]
		for _, t := range o.Tools {
			ok = ok || t == tool
		}
		return ok
	}
	if strings.TrimSpace(res.Answer) == "" {
		o.Problems = append(o.Problems, "empty answer")
	}
	if len(sc.ExpectTools) > 0 {
		hit := false
		for _, tool := range sc.ExpectTools {
			hit = hit || used(tool)
		}
		if !hit {
			o.Problems = append(o.Problems, fmt.Sprintf("expected one of %v", sc.ExpectTools))
		}
	}
	for _, tool := range sc.ExpectAllTools {
		if !used(tool) {
			o.Problems = append(o.Problems, "expected "+tool+" too")
		}
	}
	if sc.NoTool || sc.ExpectAsk {
		for _, tool := range o.Tools {
			if tool != "memory.search" {
				o.Problems = append(o.Problems, "unexpected tool "+tool)
			}
		}
	}
	if sc.ExpectAsk && !strings.ContainsAny(res.Answer, "?？") {
		o.Problems = append(o.Problems, "expected the reply to ask back")
	}
	for _, tool := range sc.ForbidTools {
		if used(tool) {
			o.Problems = append(o.Problems, "forbidden tool "+tool)
		}
	}
	for tool, want := range sc.Params {
		got := planned[tool]
		for k, v := range want {
			if !strings.Contains(strings.ToLower(fmt.Sprint(got[k])), strings.ToLower(v)) {
				o.Problems = append(o.Problems, fmt.Sprintf("%s.%s = %v, want %q", tool, k, got[k], v))
			}
		}
	}
	if len(sc.AnswerAny) > 0 {
		hit := false
		for _, s := range sc.AnswerAny {
			hit = hit || strings.Contains(res.Answer, s)
		}
		if !hit {
			o.Problems = append(o.Problems, fmt.Sprintf("answer has none of %v", sc.AnswerAny))
		}
	}
	for _, s := range append(append([]string{}, sc.AnswerNone...), forbid...) {
		if strings.Contains(res.Answer, s) {
			o.Problems = append(o.Problems, fmt.Sprintf("answer contains %q", s))
		}
	}
	o.Pass = len(o.Problems) == 0
	return o
}

// The scenario files are read by a test that only runs with real keys, so a typo in one (an unknown
// key, a tool that does not exist, a missing sample) is checked here, offline.
func TestScenarioFilesAreWellFormed(t *testing.T) {
	known := map[string]bool{}
	for _, b := range tools.Builtins() {
		known[b.ID] = true
	}
	files, _ := filepath.Glob(filepath.Join("testdata", "scenarios_*.json"))
	more, _ := filepath.Glob(filepath.Join("testdata", "tools_regression_*.json"))
	if len(more) != 2 {
		t.Fatalf("tool regression sets: %v", more)
	}
	for _, f := range append(files, more...) {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		var suite struct {
			About       string         `json:"_about"`
			Forbid      []string       `json:"forbid_in_every_answer"`
			Scenarios   []liveScenario `json:"scenarios"`
			Locale      string         `json:"locale"`
			Speaker     string         `json:"speaker"`
			Sample      string         `json:"sample"`
			EnableTools []string       `json:"enable_tools"`
		}
		dec := json.NewDecoder(strings.NewReader(string(raw)))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&suite); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		if suite.Sample != "" {
			if _, err := os.Stat(filepath.Join("practice", suite.Sample)); err != nil {
				t.Fatalf("%s: sample %v", f, err)
			}
		}
		if strings.Contains(f, "tools_regression") && len(suite.Scenarios) < 55 {
			t.Fatalf("%s: %d cases", f, len(suite.Scenarios))
		}
		ids := map[string]bool{}
		for _, sc := range suite.Scenarios {
			if sc.ID == "" || ids[sc.ID] || len(sc.Turns) == 0 {
				t.Fatalf("%s: scenario %q (duplicate or no turns)", f, sc.ID)
			}
			ids[sc.ID] = true
			named := append(append(append(append(append([]string{}, sc.ExpectTools...), sc.ExpectAllTools...), sc.ForbidTools...), sc.FailTools...), suite.EnableTools...)
			for tool := range sc.Params {
				named = append(named, tool)
			}
			for _, tool := range named {
				if !known[tool] {
					t.Fatalf("%s/%s: unknown tool %q", f, sc.ID, tool)
				}
			}
		}
	}
}
