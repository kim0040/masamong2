package application_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/fsutil"
	"masamong/internal/lang"
	"masamong/internal/tools"
)

var badTimezoneTestID atomic.Uint64

func applyDoc(t *testing.T, svc *application.Service, fn func(d *config.Document)) {
	t.Helper()
	raw, _ := config.Marshal(svc.Doc())
	d, err := config.Parse(raw) // deep copy
	if err != nil {
		t.Fatal(err)
	}
	fn(&d)
	out, _ := config.Marshal(d)
	res := exec(svc, "config.apply", map[string]any{"yaml": string(out), "base_revision": svc.Revision()})
	if !res.OK {
		t.Fatalf("apply: %+v", res.Error)
	}
}

func as(svc *application.Service, act string, in map[string]any, inv *action.Invoker) action.Result {
	raw, _ := json.Marshal(in)
	return svc.Execute(context.Background(), action.Request{Action: act, Input: raw, Locale: "en", Invoker: inv})
}

func TestToolSwitchesLimitsAndDefaults(t *testing.T) {
	svc := setupSvc(t)
	serveAnyServer(t, svc)
	off := exec(svc, "tools.configure", map[string]any{"id": "help.version", "enabled": "off"})
	if !off.OK {
		t.Fatalf("%+v", off.Error)
	}
	if r := exec(svc, "help.version", nil); r.OK || r.Error.Code != errcode.FeatureDisabled || r.Error.MessageKey != "tool.disabled" {
		t.Fatalf("disabled tool ran: %+v", r)
	}
	exec(svc, "tools.configure", map[string]any{"id": "help.version", "enabled": "default"})
	if r := exec(svc, "help.version", nil); !r.OK {
		t.Fatalf("re-enabled: %+v", r.Error)
	}
	if _, set := svc.Doc().Tools["help.version"]; set {
		t.Fatal("clearing every field must drop the tools entry")
	}

	// features.<name>=false also switches the tool off (it used to be doctor-only).
	applyDoc(t, svc, func(d *config.Document) { d.Features["weather"] = false })
	if r := exec(svc, "weather.query", nil); r.OK || r.Error.Args["reason"] != "feature_flag" {
		t.Fatalf("feature flag ignored: %+v", r)
	}
	applyDoc(t, svc, func(d *config.Document) { d.Features["weather"] = true })

	// defaults fill missing input for implemented tools.
	if r := exec(svc, "tools.configure", map[string]any{"id": "weather.query", "defaults": `{"fixture":"ok","region":"seoul"}`}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	if r := exec(svc, "weather.query", nil); !r.OK || r.Output["source"] != "fixture" {
		t.Fatalf("defaults not applied: %+v", r)
	}

	// per-user daily limit counts Discord users separately; operator calls have no user
	lim := exec(svc, "tools.configure", map[string]any{"id": "help.version", "per_user_per_day": 2})
	if !lim.OK {
		t.Fatalf("%+v", lim.Error)
	}
	u1 := &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "u1"}
	u2 := &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "u2"}
	for i, want := range []bool{true, true, false} {
		r := as(svc, "help.version", nil, u1)
		if r.OK != want {
			t.Fatalf("u1 call %d: %+v", i, r)
		}
		if !want && (r.Error.Code != errcode.ToolLimited || r.Error.Args["scope"] != "per_user_per_day") {
			t.Fatalf("%+v", r.Error)
		}
	}
	if r := as(svc, "help.version", nil, u2); !r.OK {
		t.Fatal("another user keeps their quota")
	}
	if r := exec(svc, "help.version", nil); !r.OK {
		t.Fatal("operator calls are not per-user limited")
	}

	// channel switch: off in one channel only
	if r := exec(svc, "tools.configure", map[string]any{"id": "help.version", "guild_id": "g", "channel_id": "c2", "enabled": "off"}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	if r := as(svc, "help.version", nil, &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c2", UserID: "u3"}); r.OK || r.Error.MessageKey != "tool.channel_off" {
		t.Fatalf("channel switch ignored: %+v", r)
	}
	// surface: a tool whose chat flag is off is refused to the planner, still fine as a command
	exec(svc, "tools.configure", map[string]any{"id": "help.version", "chat": "off", "per_user_per_day": 0})
	if r := as(svc, "help.version", nil, &action.Invoker{Via: "chat", GuildID: "g", ChannelID: "c", UserID: "u4"}); r.OK || r.Error.MessageKey != "tool.not_for_surface" {
		t.Fatalf("chat surface: %+v", r)
	}
	if r := as(svc, "help.version", nil, &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "u4"}); !r.OK {
		t.Fatalf("command surface: %+v", r.Error)
	}

	list := exec(svc, "tools.list", nil)
	found := false
	for _, it := range list.Output["tools"].([]any) {
		m := it.(map[string]any)
		if m["id"] == "help.version" {
			found = true
			if m["chat"] != false || m["used_today"].(int) < 5 {
				t.Fatalf("list: %+v", m)
			}
		}
	}
	if !found || len(list.Output["channel_overrides"].([]any)) != 1 {
		t.Fatalf("%+v", list.Output)
	}
}

func TestToolCommandReadinessAndDoctorSurface(t *testing.T) {
	svc := setupSvc(t)
	if !svc.ToolCommandEnabled("weather.query") || !svc.ToolCommandReady("weather.query") {
		t.Fatal("keyless Open-Meteo should make weather available")
	}
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["openmeteo"] = config.Provider{Kind: config.KindOpenMeteo, Disabled: true}
	})
	if !svc.ToolCommandEnabled("weather.query") || svc.ToolCommandReady("weather.query") {
		t.Fatal("weather with no configured provider should stay registered as enabled but not be offered as ready")
	}

	applyDoc(t, svc, func(d *config.Document) {
		d.I18n.Timezone = "Mars/Olympus"
		d.Memory.NaiveTimezone = "Mars/Base"
		d.Discord.Enabled = true
	})
	svc.Discord = &startupDoctorGateway{status: application.DiscordStartupStatus{
		Attempts: 4, LastError: "discord open: temporary network outage", NextRetry: "2026-10-05T00:30:00Z",
	}}
	res := exec(svc, "runtime.doctor", nil)
	if !res.OK {
		t.Fatalf("doctor: %+v", res.Error)
	}
	features := res.Output["features"].(map[string]any)
	discord := features["discord"].(map[string]any)
	if discord["status"] != "pending" || discord["reason"] != "gateway_retrying" {
		t.Fatalf("doctor Discord retry status = %+v", discord)
	}
	timezone := features["timezone"].(map[string]any)
	if timezone["status"] != "error" || timezone["reason"] != "timezone_invalid:Mars/Olympus" {
		t.Fatalf("doctor timezone status = %+v", timezone)
	}
	memoryTimezone := features["memory_timezone"].(map[string]any)
	if memoryTimezone["status"] != "error" || memoryTimezone["reason"] != "timezone_invalid:Mars/Base" {
		t.Fatalf("doctor memory timezone status = %+v", memoryTimezone)
	}
	startup, ok := res.Output["discord_startup"].(application.DiscordStartupStatus)
	if !ok || startup.Attempts != 4 || startup.LastError == "" || startup.NextRetry == "" {
		t.Fatalf("doctor startup details = %#v", res.Output["discord_startup"])
	}
}

func TestMemberCommandFailuresUseLocalizedMessages(t *testing.T) {
	svc := setupSvc(t)
	inv := &action.Invoker{Via: "command", ChannelID: "dm", UserID: "u1", Private: true}
	for _, tc := range []struct {
		name   string
		action string
		input  map[string]any
	}{
		{name: "fortune", action: "fortune.query", input: map[string]any{"what": "unknown"}},
		{name: "reminder", action: "reminders.update", input: map[string]any{"op": "unknown"}},
		{name: "privacy", action: "privacy.consent", input: map[string]any{"scope": "memory", "decision": "unknown"}},
	} {
		raw, _ := json.Marshal(tc.input)
		caller := inv
		if tc.name == "reminder" {
			caller = nil // exercise the reminders console's unknown-operation failure path
		}
		res := svc.Execute(context.Background(), action.Request{Action: tc.action, Input: raw, Locale: "ko", Invoker: caller})
		if res.OK || res.Error == nil || res.Error.MessageKey != "generic.error" || res.Error.Message != svc.I18n.T("ko", "generic.error") {
			t.Errorf("%s failure was not localized and generic: %+v", tc.name, res.Error)
		}
	}
}

type startupDoctorGateway struct {
	status application.DiscordStartupStatus
}

func (g *startupDoctorGateway) Connected() bool                                 { return false }
func (g *startupDoctorGateway) BotUser() (string, string)                       { return "", "" }
func (g *startupDoctorGateway) Guilds() []application.GuildInfo                 { return nil }
func (g *startupDoctorGateway) Channels(string) []application.ChannelInfo       { return nil }
func (g *startupDoctorGateway) Permissions(string, string) (int64, bool)        { return 0, false }
func (g *startupDoctorGateway) StartupStatus() application.DiscordStartupStatus { return g.status }

func TestInvokerCannotComeFromTransportJSON(t *testing.T) {
	var req action.Request
	_ = json.Unmarshal([]byte(`{"action":"help.version","Invoker":{"Via":"chat","UserID":"x"},"invoker":{"via":"chat"}}`), &req)
	if req.Invoker != nil {
		t.Fatal("Invoker must never be decoded from a transport")
	}
}

func TestOperatorHTTPTool(t *testing.T) {
	svc := setupSvc(t)
	var mu sync.Mutex
	var seen []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		seen = append(seen, r.URL.EscapedPath()+"?"+r.URL.RawQuery+" auth="+r.Header.Get("X-Key"))
		mu.Unlock()
		if strings.HasSuffix(r.URL.Path, "/500") {
			w.WriteHeader(500)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"draw": 1150, "data": map[string]any{"numbers": []int{1, 2, 3}, "noise": "x"}, "q": r.URL.Query().Get("q")})
	}))
	defer srv.Close()
	t.Setenv("LOTTO_KEY", "s3cr3t-key")
	applyDoc(t, svc, func(d *config.Document) {
		d.Tools = map[string]tools.Settings{"lotto.latest": {
			Description: "lottery numbers",
			Aliases:     []string{"lotto"},
			Params:      []tools.Param{{Name: "round", Required: true}, {Name: "q"}},
			HTTP: &tools.HTTP{URL: srv.URL + "/lotto/{round}?q={q}&key={credential}", CredentialRef: "env:LOTTO_KEY",
				Headers: map[string]string{"X-Key": "{credential}"}, Pick: []string{"draw", "data.numbers"}},
			Limits: tools.Limits{PerDay: 3},
		}}
	})
	r := exec(svc, "lotto.latest", map[string]any{"round": "11 50", "q": "a&b"})
	if !r.OK || r.Output["status"] != "ok" {
		t.Fatalf("%+v", r)
	}
	data := r.Output["data"].(map[string]any)
	if data["draw"] != float64(1150) || len(data["data.numbers"].([]any)) != 3 || data["q"] != nil {
		t.Fatalf("pick: %+v", data)
	}
	if !strings.Contains(seen[0], "/lotto/11%2050?q=a%26b&key=s3cr3t-key auth=s3cr3t-key") {
		t.Fatalf("escaping/credential: %s", seen[0])
	}
	if m := exec(svc, "lotto.latest", map[string]any{"q": "x"}); m.OK || m.Error.MessageKey != "tool.missing_params" {
		t.Fatalf("required param: %+v", m)
	}
	fail := exec(svc, "lotto.latest", map[string]any{"round": "500"})
	blob, _ := json.Marshal(fail)
	if fail.OK || strings.Contains(string(blob), "s3cr3t") {
		t.Fatalf("error must not leak the credential: %s", blob)
	}
	if again := exec(svc, "lotto.latest", map[string]any{"round": "1"}); again.OK || again.Error.Code != errcode.ToolLimited {
		t.Fatalf("per_day limit: %+v", again)
	}
	var cmd *application.ToolCommand
	for _, c := range svc.ToolCommands() {
		if c.Tool == "lotto.latest" {
			c := c
			cmd = &c
		}
	}
	if cmd == nil || !cmd.Custom || cmd.FirstParam != "round" {
		t.Fatalf("custom command: %+v", cmd)
	}
	// non-loopback plain http is refused at config time
	bad := svc.Doc()
	bad.Tools = map[string]tools.Settings{"x.y": {Description: "d", HTTP: &tools.HTTP{URL: "http://example.com/"}}}
	raw, _ := config.Marshal(bad)
	if res := exec(svc, "config.apply", map[string]any{"yaml": string(raw), "base_revision": svc.Revision()}); res.OK {
		t.Fatal("plain http tool accepted")
	}
}

// A path parameter of "." or ".." would move the call to another path of the operator's host; it is
// refused before anything is sent.
func TestOperatorHTTPToolRefusesDotSegments(t *testing.T) {
	svc := setupSvc(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"path": r.URL.Path})
	}))
	defer srv.Close()
	applyDoc(t, svc, func(d *config.Document) {
		d.Tools = map[string]tools.Settings{"city.data": {Description: "city data", Params: []tools.Param{{Name: "city", Required: true}},
			HTTP: &tools.HTTP{URL: srv.URL + "/x/{city}/data"}}}
	})
	for _, v := range []string{"..", ".", " .. "} {
		if r := exec(svc, "city.data", map[string]any{"city": v}); r.OK {
			t.Fatalf("%q accepted: %+v", v, r)
		}
	}
	if hits.Load() != 0 {
		t.Fatalf("%d requests sent", hits.Load())
	}
	if r := exec(svc, "city.data", map[string]any{"city": "..seoul"}); !r.OK || r.Output["data"].(map[string]any)["path"] != "/x/..seoul/data" {
		t.Fatalf("an ordinary value with dots: %+v", r)
	}
}

// fakeLLM serves /chat/completions: the planner model gets plan, everything else an answer. It keeps
// every request so the test can read the prompts.
type fakeLLM struct {
	mu   sync.Mutex
	plan string
	reqs []map[string]any
}

func (f *fakeLLM) server(t *testing.T) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		f.mu.Lock()
		f.reqs = append(f.reqs, body)
		f.mu.Unlock()
		content := "그때 캠핑 가자고 했잖아 ㅋㅋ"
		if body["model"] == "fake/planner" {
			content = f.plan
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": content}}},
			"usage": map[string]any{"cost": 0.001}})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func (f *fakeLLM) prompt(model string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i := len(f.reqs) - 1; i >= 0; i-- {
		if f.reqs[i]["model"] == model {
			var parts []string
			for _, m := range f.reqs[i]["messages"].([]any) {
				parts = append(parts, m.(map[string]any)["content"].(string))
			}
			return strings.Join(parts, "\n---\n")
		}
	}
	return ""
}

func chatSvc(t *testing.T, plan string) (*application.Service, *fakeLLM) {
	svc := setupSvc(t)
	f := &fakeLLM{plan: plan}
	srv := f.server(t)
	t.Setenv("FAKE_OR_KEY", "k")
	applyDoc(t, svc, func(d *config.Document) {
		d.Providers["openrouter"] = config.Provider{Kind: config.KindOpenAI, BaseURL: srv.URL, CredentialRef: "env:FAKE_OR_KEY"}
		d.Providers["openmeteo"] = config.Provider{Kind: config.KindOpenMeteo, Disabled: true} // no weather source: "unconfigured"
		d.Models["planner"] = config.Model{Provider: "openrouter", Model: "fake/planner"}
		d.Models["response"] = config.Model{Provider: "openrouter", Model: "fake/response"}
		d.Memory.Retrieve = "off"
		d.Chat.ToolMode = "planner" // the planner pipeline; the tool loop has its own tests
		d.Tools = map[string]tools.Settings{"search.web": {Chat: boolp(false)}}
	})
	return svc, f
}

func boolp(b bool) *bool { return &b }

func TestChatTurnChainsPlannerToolsAndAnswer(t *testing.T) {
	plan := `{"memory_requirement":"none","question_kind":"other","tool_plan":[
		{"tool_name":"weather","parameters":{"region":"부산","channel_id":"999"}},
		{"tool_name":"finance.quote","parameters":{"symbol":"사용자 종목"}},
		{"tool_name":"search.web","parameters":{"query":"뉴스"}},
		{"tool_name":"polls.create","parameters":{"question":"점심 뭐 먹지?","options":["김밥","라면"]}}]}`
	svc, f := chatSvc(t, plan)
	// the plan and its placeholder words are Korean: pin the room to the Korean persona
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "c", "persona_ref": "friendly_ko"}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	res, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "c", Message: "부산 날씨 어때? 그리고 점심 투표 만들어줘",
		Speaker: "영희", SpeakerID: "u1", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	byTool := map[string]string{}
	for _, tr := range res.Tools {
		st := tr.Status
		if st == "" {
			st, _ = tr.Data["status"].(string)
		}
		byTool[tr.Tool] = st
	}
	// an unconfigured provider stays distinguishable from a transient failure in the conversation.
	if byTool["weather.query"] != "unconfigured" || byTool["finance.quote"] != "needs_input" || byTool["polls.create"] != "dry_run" {
		t.Fatalf("tool chain: %+v", byTool)
	}
	if _, ran := byTool["search.web"]; ran {
		t.Fatal("a tool switched off for chat must not run")
	}
	states := map[string]string{}
	for _, st := range res.Assess.Steps {
		states[st.Tool] = st.State
	}
	if states["search.web"] != "disabled" || len(res.Assess.Stripped) != 1 {
		t.Fatalf("assessment: %+v", res.Assess)
	}
	planner := f.prompt("fake/planner")
	if strings.Contains(planner, "- search.web:") || !strings.Contains(planner, "- polls.create:") || !strings.Contains(planner, "마사몽 의도 분석기") {
		t.Fatalf("planner menu must follow the tool registry: %s", planner)
	}
	answer := f.prompt("fake/response")
	if !strings.Contains(answer, `<도구결과>`) && !strings.Contains(answer, "<도구결과>") {
		t.Fatalf("answer prompt lacks the tool block: %s", answer)
	}
	if !strings.Contains(answer, "unconfigured") || !strings.Contains(answer, "dry_run") {
		t.Fatalf("tool results did not reach the answer model: %s", answer)
	}
	if res.Answer == "" || res.CostUSD <= 0 {
		t.Fatalf("%+v", res)
	}
}

func TestChatTurnSpeaksTheChannelPersonaLanguage(t *testing.T) {
	svc, f := chatSvc(t, `{"memory_requirement":"none","tool_plan":[]}`)
	if r := exec(svc, "channels.configure", map[string]any{"guild_id": "g", "id": "en", "persona_ref": "friendly_en"}); !r.OK {
		t.Fatalf("%+v", r.Error)
	}
	if _, err := svc.ChatTurn(context.Background(), application.ChatInput{GuildID: "g", ChannelID: "en", Message: "hey what's up", Speaker: "Ann", DryRun: true}); err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"fake/planner", "fake/response"} {
		p := f.prompt(model)
		for _, name := range svc.Doc().BotNames() { // the bot's name is config data, not code
			p = strings.ReplaceAll(p, name, "")
		}
		for _, r := range p {
			if r >= 0xAC00 && r <= 0xD7A3 {
				t.Fatalf("%s prompt has Korean text for an English channel: %s", model, p)
			}
		}
	}
	if !strings.Contains(f.prompt("fake/response"), "You are "+svc.Doc().BotNames()[0]+", a Discord bot") {
		t.Fatalf("English persona with the configured name expected: %s", f.prompt("fake/response"))
	}
}

func TestChatTurnUsesCurrentMessageLanguageBeforeLocaleAndPersona(t *testing.T) {
	tests := []struct {
		name, message, locale, want string
	}{
		{name: "message overrides interaction locale and Korean default", message: "what should I have for lunch?", locale: "ja", want: "en"},
		{name: "Japanese message overrides Korean default", message: "おすすめのランチは？", want: "ja"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			svc, f := chatSvc(t, `{"memory_requirement":"none","tool_plan":[]}`)
			if _, err := svc.ChatTurn(context.Background(), application.ChatInput{
				GuildID: "g", ChannelID: "c", Message: tt.message, Locale: tt.locale, Speaker: "Ann", DryRun: true,
			}); err != nil {
				t.Fatal(err)
			}
			want, _ := lang.Get(tt.want)
			if got := f.prompt("fake/response"); !strings.Contains(got, want.Chat.ReplyLanguageRule) {
				t.Fatalf("response prompt lacks %s language rule: %s", tt.want, got)
			}
		})
	}
}

func TestChatTurnLimitsAndMonthlyBudget(t *testing.T) {
	svc, _ := chatSvc(t, `{"memory_requirement":"none","tool_plan":[]}`)
	exec(svc, "tools.configure", map[string]any{"id": "chat.respond", "per_user_per_day": 1})
	in := application.ChatInput{GuildID: "g", ChannelID: "c", Message: "안녕", Speaker: "철수", SpeakerID: "u9", DryRun: true}
	if _, err := svc.ChatTurn(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ChatTurn(context.Background(), in); !errors.Is(err, application.ErrChatLimited) {
		t.Fatalf("reply limit: %v", err)
	}
	exec(svc, "tools.configure", map[string]any{"id": "chat.respond", "per_user_per_day": 0})
	applyDoc(t, svc, func(d *config.Document) { d.Limits.MonthlyBudgetUSD = "0.001" })
	if _, err := svc.ChatTurn(context.Background(), in); !errors.Is(err, application.ErrChatBudget) {
		t.Fatalf("monthly budget: %v", err)
	}
	exec(svc, "tools.configure", map[string]any{"id": "chat.respond", "enabled": "off"})
	if _, err := svc.ChatTurn(context.Background(), in); !errors.Is(err, application.ErrChatDisabled) {
		t.Fatalf("replies off: %v", err)
	}
}

// A built-in runs only where it was made to: fortune (consent first) is not offered to the model and a
// poll vote has no command, whatever config says; the console gets can_chat/can_command to show that.
func TestToolCapabilitiesAreEnforced(t *testing.T) {
	svc := setupSvc(t)
	if r := exec(svc, "tools.configure", map[string]any{"id": "fortune.query", "chat": "true"}); r.OK {
		t.Fatalf("fortune in conversation accepted: %+v", r.Output)
	}
	if r := exec(svc, "tools.configure", map[string]any{"id": "polls.vote", "commands": "true"}); r.OK {
		t.Fatalf("a command for polls.vote accepted: %+v", r.Output)
	}
	if r := exec(svc, "tools.configure", map[string]any{"id": "weather.query", "chat": "false"}); !r.OK {
		t.Fatalf("turning chat off: %+v", r.Error)
	}
	list := exec(svc, "tools.list", nil)
	for _, it := range list.Output["tools"].([]any) {
		m := it.(map[string]any)
		switch m["id"] {
		case "fortune.query":
			if m["can_chat"] != false || m["chat"] != false || m["can_command"] != true {
				t.Errorf("fortune %v", m)
			}
		case "images.generate":
			if m["can_chat"] != true || m["chat"] != true {
				t.Errorf("images are drawn in conversation: %v", m)
			}
		case "weather.query":
			if m["can_chat"] != true || m["chat"] != false {
				t.Errorf("weather %v", m)
			}
		}
	}
}

// When the intent model is gone the answer model plans the turn (tools still run), and when the answer
// model is gone the intent model answers: the previous bot's most common failure left turns without
// tools or replies.
func TestChatFallsBackToTheOtherChatModel(t *testing.T) {
	var gone, planned, answered int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		model, _ := body["model"].(string) // model listings have no body
		if strings.HasPrefix(model, "slow/") {
			select { // a host that stops answering
			case <-r.Context().Done():
			case <-time.After(time.Second):
			}
			return
		}
		if strings.HasPrefix(model, "mute/") { // answers, with nothing in it
			_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": ""}, "finish_reason": "stop"}}})
			return
		}
		if strings.HasPrefix(model, "gone/") {
			gone++
			w.WriteHeader(404)
			_, _ = w.Write([]byte(`{"error":{"message":"model not found","type":"invalid_request_error","code":"model_not_found"}}`))
			return
		}
		raw, _ := json.Marshal(body["messages"])
		content := "부산은 지금 알 수가 없네"
		if strings.Contains(string(raw), "tool_plan") {
			planned++
			content = `{"memory_requirement":"none","question_kind":"other","tool_plan":[{"tool_name":"weather.query","parameters":{"region":"부산"}}]}`
		} else {
			answered++
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []map[string]any{{"message": map[string]any{"content": content}}}, "usage": map[string]any{"cost": 0.001}})
	}))
	defer srv.Close()
	svc := setupSvc(t)
	t.Setenv("FAKE_OR_KEY", "k")
	models := func(planner, response string) {
		applyDoc(t, svc, func(d *config.Document) {
			d.Chat.ToolMode = "planner" // the planner pipeline; the tool loop has its own tests
			d.Providers["openrouter"] = config.Provider{Kind: config.KindOpenAI, BaseURL: srv.URL, CredentialRef: "env:FAKE_OR_KEY"}
			d.Providers["openmeteo"] = config.Provider{Kind: config.KindOpenMeteo, Disabled: true}
			d.Models["planner"] = config.Model{Provider: "openrouter", Model: planner}
			d.Models["response"] = config.Model{Provider: "openrouter", Model: response}
			d.Memory.Retrieve = "off"
		})
	}
	in := application.ChatInput{GuildID: "g", ChannelID: "c", Message: "부산 날씨 어때?", Speaker: "영희", SpeakerID: "u1", DryRun: true}
	models("gone/planner", "fake/response")
	res, err := svc.ChatTurn(context.Background(), in)
	if err != nil || gone != 1 || planned != 1 || len(res.Tools) != 1 || res.Tools[0].Tool != "weather.query" || res.Intent.Source != "planner" {
		t.Fatalf("the answer model plans when the intent model is gone: err=%v gone=%d planned=%d tools=%+v intent=%+v", err, gone, planned, res.Tools, res.Intent)
	}
	models("fake/planner", "gone/response")
	res, err = svc.ChatTurn(context.Background(), in)
	if err != nil || strings.TrimSpace(res.Answer) == "" || gone != 2 {
		t.Fatalf("the intent model answers when the answer model is gone: err=%v answer=%q gone=%d", err, res.Answer, gone)
	}
	// an empty reply is a failed call too: the other model plans
	models("mute/planner", "fake/response")
	res, err = svc.ChatTurn(context.Background(), in)
	if err != nil || len(res.Tools) != 1 || res.Tools[0].Tool != "weather.query" {
		t.Fatalf("an empty intent reply hands over: err=%v tools=%+v", err, res.Tools)
	}
	// an intent model that hangs is given up after its deadline, not the HTTP client's two minutes
	defer application.SetChatDeadlineForTest("planner", 300*time.Millisecond)()
	models("slow/planner", "fake/response")
	started := time.Now()
	res, err = svc.ChatTurn(context.Background(), in)
	if err != nil || len(res.Tools) != 1 || res.Tools[0].Tool != "weather.query" || time.Since(started) > 3*time.Second {
		t.Fatalf("a hanging intent model hands over within its deadline: err=%v tools=%+v took=%v", err, res.Tools, time.Since(started))
	}
}

// "What can you do?" lists only what can answer now: a tool without its service key is not offered.
func TestHelpListsOnlyReadyTools(t *testing.T) {
	svc := setupSvc(t)
	ready := map[string]bool{}
	for _, it := range exec(svc, "tools.list", nil).Output["tools"].([]any) {
		m := it.(map[string]any)
		ready[m["id"].(string)] = m["ready"] == true
	}
	if ready["finance.quote"] || ready["search.web"] {
		t.Fatal("test setup: quotes and web search have no key here")
	}
	help := exec(svc, "help.version", nil)
	listed := 0
	tools, _ := help.Output["tools"].([]any)
	for _, it := range tools {
		id := it.(map[string]any)["id"].(string)
		if !ready[id] {
			t.Fatalf("help offers %s, which cannot answer", id)
		}
		listed++
	}
	cmds, _ := help.Output["command_aliases"].([]any)
	for _, it := range cmds {
		if id := it.(map[string]any)["tool"].(string); !ready[id] {
			t.Fatalf("help lists the %s command, which cannot answer", id)
		}
	}
	if listed == 0 {
		t.Fatal("ready tools must still be listed")
	}
}

func TestDoctorShowsConnectingWhileTheFirstDiscordAttemptRuns(t *testing.T) {
	svc := setupSvc(t)
	applyDoc(t, svc, func(d *config.Document) { d.Discord.Enabled = true })
	svc.Discord = &startupDoctorGateway{status: application.DiscordStartupStatus{Connecting: true}}
	res := exec(svc, "runtime.doctor", nil)
	if !res.OK {
		t.Fatalf("doctor: %+v", res.Error)
	}
	discord := res.Output["features"].(map[string]any)["discord"].(map[string]any)
	if discord["status"] != "pending" || discord["reason"] != "gateway_connecting" {
		t.Fatalf("a first attempt in flight is connecting, not an error: %+v", discord)
	}
	for _, a := range res.Output["attention"].([]map[string]any) {
		if a["item"] == "discord" {
			t.Fatalf("connecting must not raise an attention item: %+v", a)
		}
	}
	// after an attempt succeeded and the gateway later dropped, it is a real disconnect again
	svc.Discord = &startupDoctorGateway{status: application.DiscordStartupStatus{Attempts: 1}}
	res = exec(svc, "runtime.doctor", nil)
	discord = res.Output["features"].(map[string]any)["discord"].(map[string]any)
	if discord["status"] != "error" || discord["reason"] != "gateway_disconnected" {
		t.Fatalf("a dropped gateway is a disconnect: %+v", discord)
	}
}

func TestDoctorLogsABadTimezoneOncePerValue(t *testing.T) {
	var logs strings.Builder
	svc := setupSvcLog(t, func(*application.Service) *slog.Logger { return slog.New(slog.NewTextHandler(&logs, nil)) })
	zone := fmt.Sprintf("Mars/Doctor-once-%d", badTimezoneTestID.Add(1))
	applyDoc(t, svc, func(d *config.Document) { d.I18n.Timezone = zone })
	for i := 0; i < 5; i++ {
		if res := exec(svc, "runtime.doctor", nil); !res.OK {
			t.Fatalf("doctor: %+v", res.Error)
		}
	}
	if n := strings.Count(logs.String(), "unknown configured timezone"); n != 1 {
		t.Fatalf("a bad zone was logged %d times for 5 doctor calls, want once:\n%s", n, logs.String())
	}
	applyDoc(t, svc, func(d *config.Document) { d.I18n.Timezone = zone + "-twice" })
	exec(svc, "runtime.doctor", nil)
	if n := strings.Count(logs.String(), "unknown configured timezone"); n != 2 {
		t.Fatalf("a new bad value is logged again: %d", n)
	}
}

func TestDoctorSurfacesDataFolderPermissionWarnings(t *testing.T) {
	svc := setupSvc(t)
	fsutil.ResetWarnings()
	t.Cleanup(fsutil.ResetWarnings)
	fsutil.Warn("test", "the data folder could not be made private")
	res := exec(svc, "runtime.doctor", nil)
	if !res.OK {
		t.Fatalf("doctor: %+v", res.Error)
	}
	found := false
	for _, a := range res.Output["attention"].([]map[string]any) {
		found = found || a["item"] == "data_folder" && a["detail"] == "the data folder could not be made private"
	}
	if !found || len(res.Output["data_folder_warnings"].([]string)) != 1 {
		t.Fatalf("doctor must show the warning: %+v", res.Output["attention"])
	}
}
