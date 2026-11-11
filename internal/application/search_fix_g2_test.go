package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/chat"
)

// searchWith runs search.web as a chat turn would, with a live Guard in the context.
func searchWith(svc *application.Service, g *chat.Guard, in map[string]any) action.Result {
	raw, _ := json.Marshal(in)
	ctx := chat.WithGuard(context.Background(), g)
	return svc.Execute(ctx, action.Request{Action: "search.web", Input: raw, Locale: "en", Invoker: &action.Invoker{Via: "chat"}})
}

func monthSpend(t *testing.T, svc *application.Service) float64 {
	t.Helper()
	eng, err := svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	_, total, err := eng.Store.SpendSince("2000-01-01")
	if err != nil {
		t.Fatal(err)
	}
	return total
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// recorder serves a search answer for every request and records the bodies / queries it was sent.
type recorder struct {
	mu    sync.Mutex
	terms []string
	srv   *httptest.Server
}

func newRecorder(t *testing.T, reply func(term string) string) *recorder {
	t.Helper()
	rec := &recorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		term := r.URL.Query().Get("query")
		for _, k := range []string{"q", "query"} {
			if s, ok := b[k].(string); ok {
				term = s
			}
		}
		rec.mu.Lock()
		rec.terms = append(rec.terms, term)
		rec.mu.Unlock()
		_, _ = w.Write([]byte(reply(term)))
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := slices.Clone(r.terms)
	slices.Sort(out)
	return out
}

func tavilyReply(term string) string {
	return fmt.Sprintf(`{"results":[{"title":"t","url":"https://%s.example/1","content":"본문"}]}`, strings.ReplaceAll(term, " ", "-"))
}

const emptyLinkup = `{"results":[]}`

// A service that reports no cost (Linkup, Tavily, Serper) is charged at its list price: the month's
// spending, the turn's guard (cap and cost) all see it, empty answers included.
func TestSearchWithoutReportedCostIsRecordedAtListPrice(t *testing.T) {
	linkup := newRecorder(t, func(string) string {
		return `{"results":[{"type":"text","name":"L","url":"https://l.example/1","content":"본문"}]}`
	})
	svc := setupSvc(t)
	pointAt(t, svc, "linkup", linkup.srv.URL, "L_KEY", "k")
	g := &chat.Guard{MaxUSD: 1}
	res := searchWith(svc, g, map[string]any{"query": "캠핑", "depth": "deep"})
	if !res.OK || res.Output["provider"] != "linkup" {
		t.Fatalf("%+v", res)
	}
	if !near(g.PaidTotal(), 0.055) {
		t.Fatalf("paid %v, want Linkup deep's 0.055", g.PaidTotal())
	}
	if _, usd, _ := g.Totals(); !near(usd, 0.055) {
		t.Fatalf("the guard's spent %v", usd)
	}
	if got := monthSpend(t, svc); !near(got, 0.055) {
		t.Fatalf("month spend %v", got)
	}

	// charged and empty: the next service answers, both are paid for
	empty := newRecorder(t, func(string) string { return emptyLinkup })
	serper := newRecorder(t, func(string) string { return `{"organic":[{"title":"S","link":"https://s.example/1","snippet":"s"}]}` })
	svc = setupSvc(t)
	pointAt(t, svc, "linkup", empty.srv.URL, "L_KEY", "k")
	pointAt(t, svc, "serper", serper.srv.URL, "S_KEY", "k")
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"web_search", "order"}, "value": []any{"linkup", "serper"}}}}); !r.OK {
		t.Fatalf("order: %+v", r.Error)
	}
	g = &chat.Guard{MaxUSD: 1}
	res = searchWith(svc, g, map[string]any{"query": "캠핑"})
	if !res.OK || res.Output["provider"] != "serper" {
		t.Fatalf("%+v", res.Output)
	}
	if want := 0.0055 + 0.001; !near(g.PaidTotal(), want) || !near(monthSpend(t, svc), want) {
		t.Fatalf("paid %v, month %v, want %v (Linkup standard answered empty, then Serper)", g.PaidTotal(), monthSpend(t, svc), want)
	}

	// a failure is not charged; a free service (TinyFish) costs nothing
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer bad.Close()
	tf := newRecorder(t, func(string) string { return `{"results":[{"title":"T","url":"https://t.example/1","snippet":"s"}]}` })
	svc = setupSvc(t)
	pointAt(t, svc, "linkup", bad.URL, "L_KEY", "k")
	pointAt(t, svc, "tinyfish", tf.srv.URL, "TF_KEY", "k")
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"web_search", "order"}, "value": []any{"linkup", "tinyfish"}}}}); !r.OK {
		t.Fatalf("order: %+v", r.Error)
	}
	g = &chat.Guard{MaxUSD: 1}
	if res = searchWith(svc, g, map[string]any{"query": "캠핑", "depth": "deep"}); !res.OK || res.Output["provider"] != "tinyfish" {
		t.Fatalf("%+v", res.Output)
	}
	if g.PaidTotal() != 0 || monthSpend(t, svc) != 0 {
		t.Fatalf("a failed call and a free service cost nothing: paid %v, month %v", g.PaidTotal(), monthSpend(t, svc))
	}
}

// A deep search at a service that has no reworded queries of its own sends a request for each, and the
// fee and the daily quota count them all.
func TestRewordedQueriesAreRequestsInQuotaAndFee(t *testing.T) {
	tav := newRecorder(t, tavilyReply)
	svc := setupSvc(t)
	pointAt(t, svc, "tavily", tav.srv.URL, "T_KEY", "k")
	g := &chat.Guard{MaxUSD: 1}
	res := searchWith(svc, g, map[string]any{"query": "캠핑 장비", "depth": "deep", "queries": []string{"캠핑 텐트", "캠핑 의자", "캠핑 버너"}})
	if !res.OK || res.Output["provider"] != "tavily" {
		t.Fatalf("%+v", res.Output)
	}
	if got := tav.seen(); !slices.Equal(got, []string{"캠핑 의자", "캠핑 장비", "캠핑 텐트"}) {
		t.Fatalf("terms sent (MaxExtra of the three): %v", got)
	}
	if used := svc.Registry().Quota.Snapshot()["tavily"]; used != 3 {
		t.Fatalf("quota units %d, want the 3 requests sent", used)
	}
	if want := 3 * 0.016; !near(g.PaidTotal(), want) {
		t.Fatalf("paid %v, want %v (three advanced searches)", g.PaidTotal(), want)
	}
	items := res.Output["results"].([]any)
	if len(items) != 3 || items[0].(map[string]any)["url"] != "https://캠핑-장비.example/1" {
		t.Fatalf("the main query's result first: %+v", items)
	}

	// with only some units left the service sends fewer requests, the main query first
	tav = newRecorder(t, tavilyReply)
	svc = setupSvc(t)
	pointAt(t, svc, "tavily", tav.srv.URL, "T_KEY", "k")
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"providers", "tavily", "daily_limit"}, "value": 2}}}); !r.OK {
		t.Fatalf("limit: %+v", r.Error)
	}
	g = &chat.Guard{MaxUSD: 1}
	if res = searchWith(svc, g, map[string]any{"query": "캠핑 장비", "depth": "deep", "queries": []string{"캠핑 텐트", "캠핑 의자"}}); !res.OK {
		t.Fatalf("%+v", res)
	}
	if got := tav.seen(); !slices.Equal(got, []string{"캠핑 장비", "캠핑 텐트"}) {
		t.Fatalf("two units: the main query and the first reworded one: %v", got)
	}
	if want := 2 * 0.016; !near(g.PaidTotal(), want) || svc.Registry().Quota.Snapshot()["tavily"] != 2 {
		t.Fatalf("paid %v, quota %d", g.PaidTotal(), svc.Registry().Quota.Snapshot()["tavily"])
	}
	// the day's units are used up: the next search is the daily limit, not an unmetered request
	if res = searchWith(svc, &chat.Guard{MaxUSD: 1}, map[string]any{"query": "또 다른", "depth": "deep"}); res.OK && res.Output["status"] == "ok" {
		t.Fatalf("over the daily limit: %+v", res.Output)
	}

	// Kakao's deep search asks the web and the blogs for the main query: two units, three with a reworded one
	kak := newRecorder(t, func(string) string {
		return `{"documents":[{"title":"k","contents":"c","url":"https://k.example/1","datetime":""}],"meta":{}}`
	})
	svc = setupSvc(t)
	pointAt(t, svc, "kakao", kak.srv.URL, "K_KEY", "k")
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"providers", "kakao", "daily_limit"}, "value": 3}}}); !r.OK {
		t.Fatalf("limit: %+v", r.Error)
	}
	if res = searchWith(svc, &chat.Guard{MaxUSD: 1}, map[string]any{"query": "가평", "depth": "deep", "queries": []string{"가평 펜션", "가평 맛집"}}); !res.OK {
		t.Fatalf("%+v", res)
	}
	if got := kak.seen(); !slices.Equal(got, []string{"가평", "가평", "가평 펜션"}) || svc.Registry().Quota.Snapshot()["kakao"] != 3 {
		t.Fatalf("kakao terms %v, quota %d", got, svc.Registry().Quota.Snapshot()["kakao"])
	}
}

// hungService answers after delay or when the request ends, and counts its requests.
func hungService(t *testing.T, delay time.Duration, body string) *slowSearch {
	t.Helper()
	s := newSlowSearch(t, delay, 200, "x", "https://x.example/1")
	s.set(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(delay):
			_, _ = w.Write([]byte(body))
		case <-r.Context().Done():
		}
	})
	return s
}

// The whole search has one time limit: the next service gets what is left (at most its own limit) and is
// not asked when too little is left.
func TestSearchHasOneTotalTimeLimit(t *testing.T) {
	defer application.SetSearchLimitsForTest("deep", 400*time.Millisecond, 700*time.Millisecond, 150*time.Millisecond)()
	a := hungService(t, 30*time.Second, "")
	b := hungService(t, 30*time.Second, "")
	c := hungService(t, 30*time.Second, "")
	svc := setupSvc(t)
	pointAt(t, svc, "tavily", a.srv.URL, "T_KEY", "k")
	pointAt(t, svc, "serper", b.srv.URL, "S_KEY", "k")
	pointAt(t, svc, "tinyfish", c.srv.URL, "TF_KEY", "k")
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"web_search", "order"}, "value": []any{"tavily", "serper", "tinyfish"}}}}); !r.OK {
		t.Fatalf("order: %+v", r.Error)
	}
	started := time.Now()
	res := runVia(svc, "", map[string]any{"query": "캠핑", "depth": "deep"})
	took := time.Since(started)
	if res.OK && res.Output["status"] == "ok" {
		t.Fatalf("all hung: %+v", res.Output)
	}
	if took < 600*time.Millisecond || took > 1500*time.Millisecond {
		t.Fatalf("400 ms for the first and the 300 ms left for the second, none for the third: took %v", took)
	}
	if a.hits.Load() != 1 || b.hits.Load() != 1 || c.hits.Load() != 0 {
		t.Fatalf("hits %d %d %d: the third is not asked with under 150 ms left", a.hits.Load(), b.hits.Load(), c.hits.Load())
	}
}

// With no time left for the next service, an empty answer of the last one asked is the search's answer.
func TestSearchTimeLimitKeepsAnEmptyAnswer(t *testing.T) {
	defer application.SetSearchLimitsForTest("standard", 400*time.Millisecond, 500*time.Millisecond, 300*time.Millisecond)()
	first := hungService(t, 350*time.Millisecond, `{"organic":[]}`)
	second := hungService(t, 0, `{"organic":[{"title":"B","link":"https://b.example/1","snippet":"s"}]}`)
	svc := setupSvc(t)
	pointAt(t, svc, "serper", first.srv.URL, "S_KEY", "k")
	pointAt(t, svc, "tinyfish", second.srv.URL, "TF_KEY", "k")
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"web_search", "order"}, "value": []any{"serper", "tinyfish"}}}}); !r.OK {
		t.Fatalf("order: %+v", r.Error)
	}
	res := runVia(svc, "", map[string]any{"query": "캠핑"})
	if !res.OK || res.Output["status"] != "empty" || second.hits.Load() != 0 {
		t.Fatalf("empty answer kept: %+v (second %d)", res.Output, second.hits.Load())
	}
}

// The console's connection check gets its own time limit, whatever the caller's context allows.
func TestProbeHasItsOwnTimeLimit(t *testing.T) {
	defer application.SetProbeTimeoutForTest(300 * time.Millisecond)()
	hung := hungService(t, 30*time.Second, "")
	svc := setupSvc(t)
	pointAt(t, svc, "tavily", hung.srv.URL, "T_KEY", "k")
	started := time.Now()
	_, err := svc.Registry().Probe(context.Background(), "tavily")
	if err == nil || time.Since(started) > 3*time.Second {
		t.Fatalf("hung service: err %v after %v", err, time.Since(started))
	}
}

// ---- page reading ----

// A host is remembered only for what the fetch service said about it; our own cut-offs say nothing.
func TestHostCacheIgnoresOurCutOffs(t *testing.T) {
	defer application.SetFetchSoftForTest(120 * time.Millisecond)()
	svc, fake := withFetch(t, 3, hostOf, func(u string) (int, string, time.Duration) {
		if strings.Contains(u, "site1.") {
			return 200, okPage(campPage), 5 * time.Second // slow: the soft deadline cuts it, the host is not at fault
		}
		return 200, okPage(campPage), 0
	})
	for i := 0; i < 4; i++ {
		res := runVia(svc, "chat", map[string]any{"query": fmt.Sprintf("가평 캠핑장 요금 %d", i)})
		if !res.OK {
			t.Fatalf("round %d: %+v", i, res.Error)
		}
		if r := res.Output["results"].([]any)[1].(map[string]any); r["read"] != "timeout" {
			t.Fatalf("round %d: %v", i, r)
		}
	}
	if n := countOf(fake, "site1."); n != 4 {
		t.Fatalf("site1 was asked %d times, want every time: our cut-off is not the host's timeout", n)
	}
}

func TestHostCacheCountsServiceReportedTimeoutsAndResets(t *testing.T) {
	var slow atomic.Bool
	slow.Store(true)
	svc, fake := withFetch(t, 3, hostOf, func(u string) (int, string, time.Duration) {
		if strings.Contains(u, "site1.") && slow.Load() {
			return 200, `{"results":[],"errors":[{"url":"` + u + `","error":"timeout"}]}`, 0
		}
		return 200, okPage(campPage), 0
	})
	ask := func(q string) []any {
		return runVia(svc, "chat", map[string]any{"query": q}).Output["results"].([]any)
	}
	ask("가평 캠핑장 하나") // one timeout reported by the fetch service
	slow.Store(false)
	ask("가평 캠핑장 둘") // read: the strike is forgotten
	slow.Store(true)
	ask("가평 캠핑장 셋") // one timeout again, not two in a row
	if r := ask("가평 캠핑장 넷")[1].(map[string]any); r["read"] == "timeout" && countOf(fake, "site1.") == 3 {
		t.Fatalf("skipped after a success reset the strikes: %v", r)
	}
	// two reported timeouts in a row skip the host
	n := countOf(fake, "site1.")
	r := ask("가평 캠핑장 다섯")[1].(map[string]any)
	if r["read"] != "timeout" || countOf(fake, "site1.") != n {
		t.Fatalf("the host is skipped after two reported timeouts: %v (asked %d, was %d)", r, countOf(fake, "site1."), n)
	}
}

// A private blog post blocks that blog, not the platform; a rate limit of the host blocks nothing.
func TestHostCachePlatformHostsAndRateLimits(t *testing.T) {
	urls := []string{"https://blog.naver.com/alice/111", "https://blog.naver.com/bob/222", "https://limited.example.com/a", "https://shop.example.com/z"}
	svc, fake := withFetch(t, 4, func(i int) string { return urls[i] }, func(u string) (int, string, time.Duration) {
		switch {
		case strings.Contains(u, "alice"):
			return 200, `{"results":[],"errors":[{"url":"` + u + `","error":"login_required"}]}`, 0
		case strings.Contains(u, "limited."):
			return 200, `{"results":[],"errors":[{"url":"` + u + `","error":"target_http_error","status":429}]}`, 0
		}
		return 200, okPage(campPage), 0
	})
	in := map[string]any{"query": "가평 캠핑장 요금", "pages": 4}
	first := runVia(svc, "chat", in).Output["results"].([]any)
	if first[0].(map[string]any)["read"] != "blocked" || first[1].(map[string]any)["read"] != "ok" || first[2].(map[string]any)["read"] != "error" {
		t.Fatalf("first: %+v", first)
	}
	n := countOf(fake, "alice")
	second := runVia(svc, "chat", in).Output["results"].([]any)
	if countOf(fake, "alice") != n || countOf(fake, "bob") != 2 || countOf(fake, "limited.") != 2 {
		t.Fatalf("alice's blog is skipped, bob's and the rate-limited host are asked again: %v", fake.asked)
	}
	if second[0].(map[string]any)["read"] != "blocked" || second[1].(map[string]any)["read"] != "ok" {
		t.Fatalf("second: %+v", second)
	}
}

// A page the reader failed on is "unavailable", not "service" (which says the passage is the search
// service's own text).
func TestFailedPageReadIsUnavailable(t *testing.T) {
	svc, _ := withFetch(t, 2, hostOf, func(string) (int, string, time.Duration) { return 500, `{"error":"boom"}`, 0 })
	res := runVia(svc, "chat", map[string]any{"query": "캠핑"})
	if r := res.Output["results"].([]any)[0].(map[string]any); r["read"] != "unavailable" {
		t.Fatalf("%v", r)
	}
}

// Without a key Jina is asked for one page of a standard search and two of a deep one.
func TestKeylessJinaReadsFewPages(t *testing.T) {
	var jina atomic.Int32
	jsrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jina.Add(1)
		_, _ = w.Write([]byte(`{"code":200,"data":{"title":"젠","url":"https://x.example.com/a","content":"` + strings.Repeat("가평 캠핑장 요금은 1박에 4만 원입니다. ", 20) + `","usage":{"tokens":3}}}`))
	}))
	defer jsrv.Close()
	svc := setupSvc(t)
	pointAt(t, svc, "serper", serperServer(t, 6, hostOf).URL, "S_KEY", "k")
	if r := exec(svc, "providers.add", map[string]any{"kind": "jina_reader"}); !r.OK {
		t.Fatalf("add reader: %+v", r)
	}
	pointAt(t, svc, "jina", jsrv.URL, "J_KEY", "")
	res := runVia(svc, "chat", map[string]any{"query": "가평 캠핑장 요금"})
	reads := []string{}
	for _, it := range res.Output["results"].([]any)[:3] {
		reads = append(reads, it.(map[string]any)["read"].(string))
	}
	if jina.Load() != 1 || reads[0] != "ok" || reads[1] != "skipped" || reads[2] != "skipped" {
		t.Fatalf("standard: %d requests, reads %v", jina.Load(), reads)
	}
	jina.Store(0)
	res = runVia(svc, "chat", map[string]any{"query": "가평 캠핑장 요금 심층", "depth": "deep"})
	if jina.Load() != 2 || res.Output["pages_read"] != 2 {
		t.Fatalf("deep: %d requests, %+v", jina.Load(), res.Output["pages_read"])
	}
}

// A scheduled task reads no pages, not even when the call asks for them.
func TestScheduledTaskSearchReadsNoPages(t *testing.T) {
	svc, fake := withFetch(t, 3, hostOf, func(string) (int, string, time.Duration) { return 200, okPage(campPage), 0 })
	for _, in := range []map[string]any{{"query": "가평 캠핑장"}, {"query": "가평 캠핑장 요금", "pages": 3}} {
		res := runVia(svc, "task", in)
		if !res.OK || res.Output["pages_read"] != nil || fake.count() != 0 {
			t.Fatalf("task search %v: %+v (fetched %d)", in, res.Output, fake.count())
		}
	}
	if res := runVia(svc, "chat", map[string]any{"query": "가평 캠핑장 요금 챗", "pages": 3}); res.Output["pages_read"] != 3 {
		t.Fatalf("a chat turn still reads: %+v", res.Output)
	}
}

// A search request is sent once: the client does not quietly retry it (a paid service could charge each
// attempt beyond the turn's hold). One that timed out after it was sent may have been charged, so it is
// counted at list price; one the service refused with an HTTP error is not.
func TestSearchRequestIsSentOnceAndALostAnswerIsCharged(t *testing.T) {
	defer application.SetSearchLimitsForTest("standard", 300*time.Millisecond, 2*time.Second, 50*time.Millisecond)()
	hung := hungService(t, 30*time.Second, "")
	tf := newRecorder(t, func(string) string { return `{"results":[{"title":"T","url":"https://t.example/1","snippet":"s"}]}` })
	svc := setupSvc(t)
	pointAt(t, svc, "linkup", hung.srv.URL, "L_KEY", "k")
	pointAt(t, svc, "tinyfish", tf.srv.URL, "TF_KEY", "k")
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"web_search", "order"}, "value": []any{"linkup", "tinyfish"}}}}); !r.OK {
		t.Fatalf("order: %+v", r.Error)
	}
	g := &chat.Guard{MaxUSD: 1}
	if res := searchWith(svc, g, map[string]any{"query": "캠핑"}); !res.OK || res.Output["provider"] != "tinyfish" {
		t.Fatalf("%+v", res.Output)
	}
	if hung.hits.Load() != 1 {
		t.Fatalf("the timed-out request was sent %d times", hung.hits.Load())
	}
	if !near(g.PaidTotal(), 0.0055) || !near(monthSpend(t, svc), 0.0055) {
		t.Fatalf("paid %v, month %v: a request whose answer was lost counts at Linkup's list price", g.PaidTotal(), monthSpend(t, svc))
	}

	var hits atomic.Int32
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hits.Add(1); w.WriteHeader(502) }))
	defer bad.Close()
	svc = setupSvc(t)
	pointAt(t, svc, "linkup", bad.URL, "L_KEY", "k")
	pointAt(t, svc, "tinyfish", tf.srv.URL, "TF_KEY", "k")
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"web_search", "order"}, "value": []any{"linkup", "tinyfish"}}}}); !r.OK {
		t.Fatalf("order: %+v", r.Error)
	}
	g = &chat.Guard{MaxUSD: 1}
	if res := searchWith(svc, g, map[string]any{"query": "캠핑"}); !res.OK || res.Output["provider"] != "tinyfish" {
		t.Fatalf("%+v", res.Output)
	}
	if hits.Load() != 1 || g.PaidTotal() != 0 {
		t.Fatalf("a 502 is sent %d times and costs %v; want once and nothing", hits.Load(), g.PaidTotal())
	}

	// an answer the service gave but that could not be read was still billed
	odd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"unexpected":true}`))
	}))
	defer odd.Close()
	svc = setupSvc(t)
	pointAt(t, svc, "linkup", odd.URL, "L_KEY", "k")
	pointAt(t, svc, "tinyfish", tf.srv.URL, "TF_KEY", "k")
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"web_search", "order"}, "value": []any{"linkup", "tinyfish"}}}}); !r.OK {
		t.Fatalf("order: %+v", r.Error)
	}
	g = &chat.Guard{MaxUSD: 1}
	if res := searchWith(svc, g, map[string]any{"query": "캠핑"}); !res.OK || res.Output["provider"] != "tinyfish" {
		t.Fatalf("%+v", res.Output)
	}
	if !near(g.PaidTotal(), 0.0055) || !near(monthSpend(t, svc), 0.0055) {
		t.Fatalf("paid %v, month %v: an unreadable answer counts at Linkup's list price", g.PaidTotal(), monthSpend(t, svc))
	}
}

// The service is asked without the bot's name, a research search reaches it as one (TinyFish's search of
// papers), and the result keeps the query as it was asked.
func TestSearchAsksWithoutTheBotsNameAndForPapers(t *testing.T) {
	var qs []string
	var mu sync.Mutex
	tf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		qs = append(qs, r.URL.RawQuery)
		mu.Unlock()
		_, _ = w.Write([]byte(`{"results":[{"title":"Three-dimensional time","url":"https://w.example/doi/1","snippet":"This paper introduces","authors":["G Kletetschka"],"year":2025}]}`))
	}))
	defer tf.Close()
	svc := setupSvc(t)
	pointAt(t, svc, "tinyfish", tf.URL, "TF_KEY", "k")
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"web_search", "order"}, "value": []any{"tinyfish"}}}}); !r.OK {
		t.Fatalf("order: %+v", r.Error)
	}
	res := searchWith(svc, &chat.Guard{MaxUSD: 1}, map[string]any{"query": "마사몽 시간이 3차원이라는 논문", "topic": "research"})
	if !res.OK || res.Output["query"] != "마사몽 시간이 3차원이라는 논문" || res.Output["topic"] != "research" {
		t.Fatalf("%+v", res.Output)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(qs) != 1 {
		t.Fatalf("requests: %v", qs)
	}
	v, _ := url.ParseQuery(qs[0])
	if v.Get("query") != "시간이 3차원이라는 논문" || v.Get("domain_type") != "research_paper" {
		t.Fatalf("asked %v", v)
	}
}
