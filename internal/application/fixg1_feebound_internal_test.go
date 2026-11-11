package application

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/lang"
	"masamong/internal/tools"
)

// fixg1Svc is a set-up service whose search order is order (provider ids of the default document, or
// extra ones given by kind), each with a key and pointed at url (unreachable unless a test serves it).
func fixg1Svc(t *testing.T, order []string, extra map[string]string, url map[string]string) *Service {
	t.Helper()
	s, err := Open(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if r := s.Execute(context.Background(), action.Request{Action: "setup.resume", Input: []byte(`{"instance":"fixg1","timezone":"Asia/Seoul"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	raw, _ := config.Marshal(s.Doc())
	d, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("FIXG1_KEY", "k")
	for id, kind := range extra {
		d.Providers[id] = config.Provider{Kind: kind}
	}
	for _, id := range order {
		p := d.Providers[id]
		p.CredentialRef, p.Disabled = "env:FIXG1_KEY", false
		p.BaseURL = "http://127.0.0.1:1"
		if u := url[id]; u != "" {
			p.BaseURL = u
		}
		d.Providers[id] = p
	}
	d.WebSearch.Order = order
	out, _ := config.Marshal(d)
	in, _ := json.Marshal(map[string]any{"yaml": string(out)})
	if r := s.Execute(context.Background(), action.Request{Action: "config.apply", Input: in, BaseRevision: s.Revision()}); !r.OK {
		t.Fatalf("apply: %+v", r.Error)
	}
	return s
}

// opsOrder is an order with two paid services and two free ones, as an operator may run it.
var opsOrder = []string{"exa", "tinyfish", "linkup", "kakao"}

func toolsOf(t *testing.T, s *Service) map[string]chat.ToolInfo {
	t.Helper()
	p, _ := lang.Get("ko")
	out := map[string]chat.ToolInfo{}
	for _, info := range s.chatToolset(chat.Voice{Pack: p}, "", "").Enabled {
		out[info.ID] = info
	}
	return out
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-12 }

// A web search holds what it may pay at every service of the configured order (a service that finds
// nothing may still charge, and the next one is asked), by the call's depth and reworded queries; a
// provider the call names is the only one counted. The price fallback of finance.quote holds the
// standard search it makes. With only free services (or none) neither pays: nothing is held or waited for.
func TestFeeBoundFollowsTheSearchOrder(t *testing.T) {
	infos := toolsOf(t, fixg1Svc(t, opsOrder, nil, nil))
	search, quote := infos["search.web"], infos["finance.quote"]
	if !search.Pays || search.FeeBound == nil || !quote.Pays || quote.FeeBound == nil {
		t.Fatalf("search %+v quote %+v", search, quote)
	}
	for _, c := range []struct {
		params map[string]any
		want   float64
	}{
		{map[string]any{"depth": "deep"}, 0.015 + 0.055},                           // Exa deep + Linkup deep; TinyFish and Kakao are free
		{map[string]any{"depth": " Deep ", "queries": []any{"a", "b", "c"}}, 0.07}, // Exa and Linkup take reworded queries in one request
		{map[string]any{"depth": "quick"}, 0.005 + 0.0055},
		{map[string]any{}, 0.008 + 0.0055},
		{map[string]any{"depth": "bogus"}, 0.008 + 0.0055},
		{nil, 0.008 + 0.0055},
		{map[string]any{"depth": "deep", "provider": "linkup"}, 0.055},
		{map[string]any{"depth": "deep", "provider": "nowhere"}, 0.07},
	} {
		if got := search.FeeBound(c.params); !near(got, c.want) {
			t.Errorf("search %v: bound %v, want %v", c.params, got, c.want)
		}
	}
	if !near(search.MaxFeeUSD, 0.07) {
		t.Errorf("search MaxFeeUSD %v is not its dearest call's bound", search.MaxFeeUSD)
	}
	if got := quote.FeeBound(map[string]any{"symbol": "AAPL"}); !near(got, 0.008+0.0055) || !near(quote.MaxFeeUSD, got) {
		t.Errorf("quote bound %v (max %v), want one standard search", got, quote.MaxFeeUSD)
	}
	// services that run reworded queries as requests of their own count each of them
	brave := toolsOf(t, fixg1Svc(t, []string{"brave2"}, map[string]string{"brave2": config.KindBrave}, nil))["search.web"]
	if got := brave.FeeBound(map[string]any{"depth": "deep", "queries": []any{"a", "", "b", "c"}}); !near(got, 0.005*3) {
		t.Errorf("brave deep with queries: %v", got)
	}
	for name, order := range map[string][]string{"free": {"tinyfish", "kakao"}, "none": nil} {
		infos := toolsOf(t, fixg1Svc(t, order, nil, nil))
		for _, id := range []string{"search.web", "finance.quote"} {
			if info, ok := infos[id]; ok && (info.Pays || info.MaxFeeUSD != 0 || info.FeeBound != nil) {
				t.Errorf("%s order: %s pays %+v", name, id, info)
			}
		}
	}
}

// The tool loop runs a round's calls of read-only lookups at once; tools that write keep their order, and
// a tool that pays nothing holds nothing.
func TestToolInfoMarksParallelLookups(t *testing.T) {
	p, _ := lang.Get("ko")
	parallel := map[string]bool{}
	for _, r := range tools.Resolve(nil, nil) {
		info := toolInfo(r, p, nil)
		parallel[r.ID] = info.Parallel
		if info.Parallel && (r.Writes || r.Once) {
			t.Errorf("%s writes or is once, yet runs beside itself", r.ID)
		}
		if !info.Pays && (info.MaxFeeUSD != 0 || info.FeeBound != nil) {
			t.Errorf("%s pays nothing, yet holds a fee", r.ID)
		}
	}
	for _, id := range []string{"search.web", "web.read", "places.search", "weather.query", "finance.quote", "finance.fx", "disasters.query"} {
		if !parallel[id] {
			t.Errorf("%s does not run beside itself", id)
		}
	}
	for _, id := range []string{"reminders.propose", "polls.create", "images.generate", "summaries.channel"} {
		if parallel[id] {
			t.Errorf("%s runs beside itself", id)
		}
	}
}

// fixg1LLM is a tool loop model that asks for one round of calls, then answers.
type fixg1LLM struct {
	mu    sync.Mutex
	calls []chat.LoopCall
	asked bool
}

func (f *fixg1LLM) Chat(context.Context, string, []chat.Msg, int, float64) (string, float64, error) {
	return "-", 0, nil
}
func (f *fixg1LLM) ToolsReady(string) bool { return true }
func (f *fixg1LLM) ChatTools(context.Context, string, []chat.LoopMsg, []chat.ToolSpec, int) (chat.LoopReply, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.asked {
		return chat.LoopReply{Text: "-"}, nil
	}
	f.asked = true
	return chat.LoopReply{Calls: f.calls}, nil
}

// timedRun runs each call for the time its query (or region) names after its first letter ("a2s") and charges fee for a web search.
type timedRun struct {
	mu     sync.Mutex
	fee    float64
	t0     time.Time
	starts map[string]time.Duration
}

func (r *timedRun) run(ctx context.Context, tool string, p map[string]any) chat.ToolResult {
	name, _ := p["query"].(string)
	if name == "" {
		name, _ = p["region"].(string)
	}
	d, err := time.ParseDuration(name[1:]) // "a2s": two seconds
	if err != nil {
		panic(name)
	}
	r.mu.Lock()
	r.starts[name] = time.Since(r.t0)
	r.mu.Unlock()
	time.Sleep(d)
	if tool == "search.web" {
		chat.GuardFrom(ctx).Spend(r.fee)
		chat.GuardFrom(ctx).Paid(r.fee)
	}
	return chat.ToolResult{Tool: tool, OK: true, Data: map[string]any{"status": "ok"}}
}

// With the default per-reply cap (0.15) and an order of Exa, TinyFish, Linkup and Kakao (deep bound
// 0.07), two deep searches and a weather lookup start together through the real Guard, and a third deep
// search is let in beside them while what is spent and held (0.14) is under the cap. Under a lower cap
// (0.10) the third waits until one has ended and is then let in on what that one really paid.
func TestDeepSearchesRunSideBySideUnderTheDefaultCap(t *testing.T) {
	s := fixg1Svc(t, opsOrder, nil, nil)
	if got := parseUSD(s.Doc().Limits.PerRequestBudgetUSD, 0); got != 0.15 {
		t.Fatalf("default per-reply cap %v", got)
	}
	infos := toolsOf(t, s)
	weather, ok := infos["weather.query"]
	if !ok || weather.Pays {
		t.Fatalf("weather %+v", weather)
	}
	ts := chat.Toolset{Enabled: []chat.ToolInfo{infos["search.web"], weather}, Known: map[string]bool{"search.web": true, "weather.query": true}}
	p, _ := lang.Get("ko")
	deep := func(id, name string) chat.LoopCall {
		return chat.LoopCall{ID: id, Name: "search__web", Arguments: `{"query":"` + name + `","depth":"deep"}`}
	}
	round := func(cap float64, calls ...chat.LoopCall) (map[string]time.Duration, []chat.ToolResult) {
		var starts map[string]time.Duration
		var results []chat.ToolResult
		synctest.Test(t, func(t *testing.T) {
			r := &timedRun{fee: 0.012, t0: time.Now(), starts: map[string]time.Duration{}}
			g := &chat.Guard{MaxUSD: cap, MaxCalls: 10}
			loop := chat.ToolLoop{LLM: &fixg1LLM{calls: calls}, Run: r.run, Role: "planner", Voice: chat.Voice{Pack: p}, Guard: g}
			var err error
			results, _, err = loop.Do(context.Background(), ts, nil, "q", "")
			if err != nil {
				t.Fatal(err)
			}
			if g.HeldTotal() != 0 {
				t.Fatalf("still held %v", g.HeldTotal())
			}
			starts = r.starts
		})
		return starts, results
	}
	starts, results := round(0.15, deep("a", "a2s"), deep("b", "b2s"), chat.LoopCall{ID: "w", Name: "weather__query", Arguments: `{"region":"w1s"}`})
	if starts["a2s"] != 0 || starts["b2s"] != 0 || starts["w1s"] != 0 || len(results) != 3 {
		t.Fatalf("two deep searches and the weather: starts %v", starts)
	}
	starts, _ = round(0.15, deep("a", "a2s"), deep("b", "b2s"), deep("c", "c1s"))
	if starts["a2s"] != 0 || starts["b2s"] != 0 || starts["c1s"] != 0 {
		t.Fatalf("three deep searches under 0.15 (0.14 held): starts %v", starts)
	}
	starts, results = round(0.10, deep("a", "a1s"), deep("b", "b2s"), deep("c", "c1s"))
	if starts["a1s"] != 0 || starts["b2s"] != 0 || starts["c1s"] != time.Second {
		t.Fatalf("three deep searches under 0.10: starts %v, want the third after the first ended", starts)
	}
	for i, r := range results {
		if !r.OK {
			t.Fatalf("call %d not run: %+v", i, r)
		}
	}
}

// A search whose first service charges and finds nothing asks the next one, which charges too: the turn
// is charged both fees, and both together stay within what the call held.
func TestFallbackSearchSpendsWithinItsHold(t *testing.T) {
	exa := func(results string) *httptest.Server {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = w.Write([]byte(`{"results":` + results + `,"costDollars":{"total":0.007}}`))
		}))
		t.Cleanup(srv.Close)
		return srv
	}
	empty, found := exa(`[]`), exa(`[{"title":"E","url":"https://e.example/1","highlights":["h"]}]`)
	s := fixg1Svc(t, []string{"exa", "exa2"}, map[string]string{"exa2": config.KindExa}, map[string]string{"exa": empty.URL, "exa2": found.URL})
	info := toolsOf(t, s)["search.web"]
	params := map[string]any{"query": "camping"}
	bound := info.FeeBound(params)
	g := &chat.Guard{MaxUSD: 0.15}
	raw, _ := json.Marshal(params)
	res := s.Execute(chat.WithGuard(context.Background(), g), action.Request{Action: "search.web", Input: raw, Locale: "en", Invoker: &action.Invoker{Via: "command"}})
	if !res.OK || res.Output["provider"] != "exa2" {
		t.Fatalf("search: %+v", res)
	}
	_, spent, _ := g.Totals()
	if !near(spent, 0.014) || spent > bound {
		t.Fatalf("spent %v against a hold of %v", spent, bound)
	}
}
