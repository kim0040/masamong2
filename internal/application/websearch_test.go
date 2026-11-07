package application_test

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/application"
)

// pointAt moves a provider to a test server and gives it a key from the environment.
func pointAt(t *testing.T, svc *application.Service, id, url, env, key string) {
	t.Helper()
	t.Setenv(env, key)
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{
		map[string]any{"path": []any{"providers", id, "base_url"}, "value": url},
		map[string]any{"path": []any{"providers", id, "credential_ref"}, "value": "env:" + env},
	}}); !r.OK {
		t.Fatalf("point %s: %+v", id, r.Error)
	}
}

// search.web tries the services in order: a refused key hands over to the next one, the answer says
// which service found the pages, and the refused one shows as rejected on the Connections page.
func TestSearchFallsBackAndNamesTheService(t *testing.T) {
	var tavilyHits, serperHits atomic.Int32
	tavily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tavilyHits.Add(1)
		w.WriteHeader(401)
		_, _ = w.Write([]byte(`{"detail":{"error":"Unauthorized: missing or invalid API key."}}`))
	}))
	defer tavily.Close()
	serper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		serperHits.Add(1)
		_, _ = w.Write([]byte(`{"organic":[{"title":"가평 캠핑장","link":"https://camp.example/1","snippet":"강 보이는 자리","position":1}]}`))
	}))
	defer serper.Close()
	svc := setupSvc(t)
	if r := exec(svc, "search.web", map[string]any{"query": "캠핑"}); !r.OK || r.Output["status"] != "unconfigured" || !strings.Contains(r.Output["message"].(string), "Tavily") {
		t.Fatalf("no key anywhere: %+v", r)
	}
	pointAt(t, svc, "tavily", tavily.URL, "T_KEY", "bad")
	pointAt(t, svc, "serper", serper.URL, "S_KEY", "good")
	feats := out(t, svc, "runtime.doctor", nil)["features"].(map[string]any)
	if feats["search"].(map[string]any)["status"] != "ready" {
		t.Fatalf("doctor: any search service with a key: %v", feats["search"])
	}
	res := out(t, svc, "search.web", map[string]any{"query": "캠핑"})
	if res["provider"] != "serper" || res["source"] != "serper" || len(res["results"].([]any)) != 1 || tavilyHits.Load() != 1 {
		t.Fatalf("fallback: %v", res)
	}
	passed, _ := json.Marshal(res["passed_over"])
	if !strings.Contains(string(passed), `"provider":"tavily"`) {
		t.Fatalf("passed over: %s", passed)
	}
	health := out(t, svc, "providers.list", nil)["health"].(map[string]any)
	if h, _ := health["tavily"].(map[string]any); h == nil || h["rejected"] != true {
		t.Fatalf("the refused key shows: %v", health["tavily"])
	}
	// an explicit order, and one service by name
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"web_search", "order"}, "value": []any{"serper", "tavily"}}}}); !r.OK {
		t.Fatalf("order: %+v", r.Error)
	}
	before := tavilyHits.Load()
	if res := out(t, svc, "search.web", map[string]any{"query": "캠핑"}); res["provider"] != "serper" || tavilyHits.Load() != before {
		t.Fatalf("serper first: %v", res)
	}
	if r := exec(svc, "search.web", map[string]any{"query": "캠핑", "provider": "kakao"}); r.OK {
		t.Fatal("a service without a key cannot be picked")
	}
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"web_search", "order"}, "value": []any{"kma"}}}}); r.OK {
		t.Fatal("only search providers can be ordered")
	}
	list := out(t, svc, "tools.list", nil)["tools"].([]any)
	for _, raw := range list {
		tl := raw.(map[string]any)
		if tl["id"] != "search.web" {
			continue
		}
		order, _ := json.Marshal(tl["provider_order"])
		inputs, _ := json.Marshal(tl["inputs"])
		if string(order) != `["serper","tavily"]` || !strings.Contains(string(inputs), `"name":"fresh"`) || tl["provider_ready"] != true {
			t.Fatalf("tools.list search: %v", tl)
		}
	}
}

func TestWebReadPublicPagesOnly(t *testing.T) {
	var path string
	jina := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path = r.URL.Path
		_, _ = w.Write([]byte(`{"code":200,"status":20000,"data":{"title":"공지","url":"https://news.example.com/a","content":"# 공지\n\n내일 휴강입니다.","usage":{"tokens":12}}}`))
	}))
	defer jina.Close()
	svc := setupSvc(t)
	if r := exec(svc, "providers.add", map[string]any{"kind": "jina_reader"}); !r.OK || r.Output["id"] != "jina" {
		t.Fatalf("add reader: %+v", r)
	}
	pointAt(t, svc, "jina", jina.URL, "J_KEY", "")
	// off until the operator turns it on: it hands links people post to an outside service
	if r := exec(svc, "web.read", map[string]any{"url": "https://news.example.com/a"}); r.OK || path != "" {
		t.Fatalf("web.read must wait for the operator: %+v", r)
	}
	for _, raw := range out(t, svc, "tools.list", nil)["tools"].([]any) {
		if tl := raw.(map[string]any); tl["id"] == "web.read" && (tl["ready"] != false || tl["ready_reason"] != "opt_in" || tl["opt_in"] != true) {
			t.Fatalf("tools.list web.read: %v", tl)
		}
	}
	out(t, svc, "tools.configure", map[string]any{"id": "web.read", "enabled": "on"})
	page := out(t, svc, "web.read", map[string]any{"url": "https://news.example.com/a#top"})
	if page["title"] != "공지" || !strings.Contains(page["content"].(string), "휴강") || path != "/https://news.example.com/a" {
		t.Fatalf("read: %v (path %s)", page, path)
	}
	for _, bad := range []string{"http://127.0.0.1:8787/api/action", "http://192.168.0.1/", "file:///etc/hosts", "https://intranet/"} {
		if r := exec(svc, "web.read", map[string]any{"url": bad}); r.OK || r.Error.Code != "INVALID_INPUT" {
			t.Fatalf("%s must be refused: %+v", bad, r)
		}
	}
}

// Instances made before the search services existed can add them from the console.
func TestAddSearchServiceToAnOldInstance(t *testing.T) {
	svc := setupSvc(t)
	if r := exec(svc, "providers.remove", map[string]any{"id": "linkup"}); !r.OK {
		t.Fatalf("remove: %+v", r)
	}
	r := exec(svc, "providers.add", map[string]any{"kind": "linkup"})
	if !r.OK || r.Output["id"] != "linkup" {
		t.Fatalf("add: %+v", r)
	}
	p := out(t, svc, "config.document", nil)["document"].(map[string]any)["providers"].(map[string]any)["linkup"].(map[string]any)
	if p["base_url"] != "https://api.linkup.so" || p["credential_ref"] != "env:MASAMONG_LINKUP_KEY" || p["daily_limit"].(float64) != 100 {
		t.Fatalf("linkup: %v", p)
	}
	if r := exec(svc, "providers.remove", map[string]any{"id": "kma"}); r.OK {
		t.Fatal("weather providers are switched off, not removed")
	}
}

// A price Finnhub's plan does not carry (Korean listings, recorded 2026-10-01 as 403 "You don't have
// access to this resource.") is answered from a web search, marked as one; a US symbol still gets the
// live quote, and without a search service the plan refusal stays an error.
func TestQuoteFallsBackToSearchOutsideTheQuotePlan(t *testing.T) {
	finn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Query().Get("symbol"), ".KS") {
			w.WriteHeader(403)
			_, _ = w.Write([]byte(`{"error":"You don't have access to this resource."}`))
			return
		}
		_, _ = w.Write([]byte(`{"c":337.35,"d":7.95,"dp":2.41,"h":339.5,"l":330.14,"o":331,"pc":329.4,"t":1790700000}`))
	}))
	defer finn.Close()
	var query string
	serper := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		query, _ = body["q"].(string)
		_, _ = w.Write([]byte(`{"organic":[{"title":"삼성전자 주가","link":"https://finance.example/005930","snippet":"현재가 81,200원","position":1}]}`))
	}))
	defer serper.Close()
	svc := setupSvc(t)
	pointAt(t, svc, "finnhub", finn.URL, "F_KEY", "f")
	if r := exec(svc, "finance.quote", map[string]any{"symbol": "005930.KS"}); r.OK {
		t.Fatalf("no search service: the plan refusal stays an error: %+v", r)
	}
	pointAt(t, svc, "serper", serper.URL, "S_KEY", "s")
	res := out(t, svc, "finance.quote", map[string]any{"symbol": "005930.KS"})
	if res["source"] != "search" || res["live"] != false || res["reason"] != "symbol_not_on_plan" || query != "005930 주가" || len(res["results"].([]any)) != 1 {
		t.Fatalf("Korean listing from search: %v (query %q)", res, query)
	}
	if us := out(t, svc, "finance.quote", map[string]any{"symbol": "AAPL"}); us["source"] != "finnhub" || us["price"] != "337.35" {
		t.Fatalf("a US symbol keeps the live quote: %v", us)
	}
}

// A service that is slow does not hold the turn: after its limit for the depth the search goes on to
// the next one, and the answer says the first timed out (before, the 15 s HTTP limit cut each attempt
// and the client retried it twice).
func TestSlowSearchServiceIsPassedOver(t *testing.T) {
	var slowHits atomic.Int32
	slow := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		slowHits.Add(1)
		// read the body so the server notices the caller hanging up; then hang until it does
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-r.Context().Done():
		case <-time.After(20 * time.Second):
		}
	}))
	defer slow.Close()
	fast := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"organic":[{"title":"가평 캠핑장","link":"https://camp.example/1","snippet":"강 보이는 자리","position":1}]}`))
	}))
	defer fast.Close()
	svc := setupSvc(t)
	pointAt(t, svc, "tavily", slow.URL, "T_KEY", "k")
	pointAt(t, svc, "serper", fast.URL, "S_KEY", "k")
	for _, c := range []struct{ asked, limit string }{{"quick", "quick"}, {"", "standard"}, {"deep", "deep"}} {
		restore := application.SetSearchDeadlineForTest(c.limit, 150*time.Millisecond)
		before := slowHits.Load()
		started := time.Now()
		res := out(t, svc, "search.web", map[string]any{"query": "캠핑", "depth": c.asked})
		restore()
		if took := time.Since(started); took > 3*time.Second {
			t.Fatalf("depth %q: the slow service held the search for %v", c.asked, took)
		}
		passed, _ := json.Marshal(res["passed_over"])
		if res["provider"] != "serper" || slowHits.Load() != before+1 || !strings.Contains(string(passed), `"provider":"tavily"`) || !strings.Contains(string(passed), `"reason":"timeout"`) {
			t.Fatalf("depth %q: the slow service must be tried once and passed over: %v (hits %d)", c.asked, res, slowHits.Load()-before)
		}
	}
}

// A freshness word the schema does not offer is dropped, not sent on (Tavily answers 400 to "hour").
func TestSearchDropsUnknownFreshness(t *testing.T) {
	var got map[string]any
	tavily := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = nil
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(`{"results":[{"title":"T","url":"https://t.example/1","content":"본문"}]}`))
	}))
	defer tavily.Close()
	svc := setupSvc(t)
	pointAt(t, svc, "tavily", tavily.URL, "T_KEY", "k")
	for fresh, want := range map[string]any{"hour": nil, "": nil, "Week": "week", "day": "day"} {
		out(t, svc, "search.web", map[string]any{"query": "캠핑", "fresh": fresh})
		if got["time_range"] != want {
			t.Errorf("fresh %q: time_range = %v, want %v", fresh, got["time_range"], want)
		}
	}
}
