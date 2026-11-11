package application_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/application"
)

// fetchFake is a TinyFish Fetch that answers by the address asked for and records every one.
type fetchFake struct {
	srv   *httptest.Server
	mu    sync.Mutex
	asked []string
	reply func(u string) (status int, body string, delay time.Duration)
}

func newFetchFake(t *testing.T, reply func(u string) (int, string, time.Duration)) *fetchFake {
	t.Helper()
	f := &fetchFake{reply: reply}
	f.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b struct {
			URLs []string `json:"urls"`
		}
		_ = json.Unmarshal(raw, &b)
		u := b.URLs[0]
		f.mu.Lock()
		f.asked = append(f.asked, u)
		f.mu.Unlock()
		status, body, delay := f.reply(u)
		if delay > 0 {
			select {
			case <-time.After(delay):
			case <-r.Context().Done():
				return
			}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(f.srv.Close)
	t.Cleanup(application.SetFetchBaseForTest(f.srv.URL))
	return f
}

func (f *fetchFake) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.asked)
}

func (f *fetchFake) saw(sub string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.asked {
		if strings.Contains(u, sub) {
			return true
		}
	}
	return false
}

func okPage(text string) string {
	b, _ := json.Marshal(map[string]any{"results": []any{map[string]any{"url": "u", "title": "페이지", "text": text}}})
	return string(b)
}

const campPage = "# 가평 캠핑장 안내\n\n가평에는 강 옆에 있는 캠핑장이 많습니다. 주말 예약은 한 달 전에 마감됩니다.\n\n프로야구 소식은 다른 페이지를 보세요.\n\n캠핑장 이용 요금은 1박에 4만 원이고, 강 보이는 자리는 5만 원입니다."

// serperResults serves n results from a Serper-shaped server (no page text: pages are read by fetch).
func serperServer(t *testing.T, n int, urlOf func(i int) string) *httptest.Server {
	t.Helper()
	var rows []string
	for i := 0; i < n; i++ {
		rows = append(rows, fmt.Sprintf(`{"title":"R%d","link":%q,"snippet":"스니펫 %d","position":%d}`, i, urlOf(i), i, i+1))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"organic":[` + strings.Join(rows, ",") + `]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func runVia(svc *application.Service, via string, in map[string]any) action.Result {
	raw, _ := json.Marshal(in)
	req := action.Request{Action: "search.web", Input: raw, Locale: "en"}
	if via != "" {
		req.Invoker = &action.Invoker{Via: via}
	}
	return svc.Execute(context.Background(), req)
}

func hostOf(i int) string { return fmt.Sprintf("https://site%d.example.com/page", i) }

// withFetch sets up a Serper search service, a TinyFish key (the page reader's) and a fake TinyFish Fetch.
func withFetch(t *testing.T, n int, urlOf func(int) string, reply func(string) (int, string, time.Duration)) (*application.Service, *fetchFake) {
	t.Helper()
	svc := setupSvc(t)
	pointAt(t, svc, "serper", serperServer(t, n, urlOf).URL, "S_KEY", "k")
	pointAt(t, svc, "tinyfish", "http://127.0.0.1:1", "TF_KEY", "tf")
	return svc, newFetchFake(t, reply)
}

// Pages are read for a chat turn by the depth's default and nowhere else; a planner's number is clamped.
func TestSearchPagesByCaller(t *testing.T) {
	svc, fake := withFetch(t, 8, hostOf, func(string) (int, string, time.Duration) { return 200, okPage(campPage), 0 })

	chat := runVia(svc, "chat", map[string]any{"query": "가평 캠핑장 요금"})
	if !chat.OK || chat.Output["pages_read"] != 3 || fake.count() != 3 {
		t.Fatalf("chat default is 3 pages: %+v (fetched %d)", chat.Output, fake.count())
	}
	items := chat.Output["results"].([]any)
	first := items[0].(map[string]any)
	if first["read"] != "ok" || !strings.Contains(first["passage"].(string), "1박에 4만 원") || strings.Contains(first["passage"].(string), "프로야구") || first["snippet"] != "스니펫 0" {
		t.Fatalf("first result: %v", first)
	}
	if _, has := items[3].(map[string]any)["read"]; has {
		t.Fatalf("results beyond the pages read carry no read key: %v", items[3])
	}

	before := fake.count()
	for _, via := range []string{"command", "", "reply", "cli"} {
		res := runVia(svc, via, map[string]any{"query": "가평 캠핑장 요금"})
		if _, has := res.Output["pages_read"]; has || fake.count() != before {
			t.Fatalf("via %q reads no pages: %+v", via, res.Output)
		}
	}
	// the quote fallback passes pages 0 itself
	if r := runVia(svc, "chat", map[string]any{"query": "삼성 주가", "pages": 0}); r.Output["pages_read"] != nil || fake.count() != before {
		t.Fatalf("explicit pages 0: %+v", r.Output)
	}
	// a planner asks for 99: the deep setting (6) is the most
	res := runVia(svc, "chat", map[string]any{"query": "가평 캠핑장 요금", "size": 8, "pages": 99})
	if res.Output["pages_read"] != 6 || fake.count() != before+6 {
		t.Fatalf("clamped to 6: %+v (fetched %d)", res.Output, fake.count()-before)
	}
	// quick reads none whatever is asked
	before = fake.count()
	if res = runVia(svc, "chat", map[string]any{"query": "x", "depth": "quick", "pages": 5}); res.Output["pages_read"] != nil || fake.count() != before {
		t.Fatalf("quick: %+v", res.Output)
	}
	// the operator's setting: 0 = snippets only for chat too
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"web_search", "read_pages"}, "value": 0}}}); !r.OK {
		t.Fatalf("read_pages: %+v", r.Error)
	}
	if res = runVia(svc, "chat", map[string]any{"query": "가평 캠핑장 요금"}); res.Output["pages_read"] != nil || fake.count() != before {
		t.Fatalf("read_pages 0: %+v", res.Output)
	}
}

// Without pages the answer is exactly what it was before: no new keys anywhere.
func TestSearchWithoutPagesIsByteIdentical(t *testing.T) {
	svc := setupSvc(t)
	pointAt(t, svc, "serper", serperServer(t, 1, func(int) string { return "https://camp.example/1" }).URL, "S_KEY", "k")
	want := `{"live":true,"paid_provider_call":true,"provider":"serper","query":"캠핑","results":[{"datetime":"","snippet":"스니펫 0","title":"R0","url":"https://camp.example/1"}],"source":"serper","status":"ok"}`
	for _, via := range []string{"command", ""} {
		got, _ := json.Marshal(runVia(svc, via, map[string]any{"query": "캠핑"}).Output)
		if string(got) != want {
			t.Fatalf("via %q:\n got %s\nwant %s", via, got, want)
		}
	}
	if got, _ := json.Marshal(runVia(svc, "chat", map[string]any{"query": "캠핑", "pages": 0}).Output); string(got) != want {
		t.Fatalf("chat with pages 0: %s", got)
	}
}

// A failing fetch changes nothing about the search: ok, snippets, and the search service's health. A
// page's own error never reaches a health record; the service's failure does, under its own key.
func TestSearchFetchFailureKeepsSnippetsAndHealth(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   func(u string) string
		read   string
		health bool // the fetch service's health records an error
	}{
		{"page error", 200, func(u string) string { return `{"results":[],"errors":[{"url":"` + u + `","error":"bot_blocked"}]}` }, "blocked", false},
		{"service error", 500, func(string) string { return `{"error":"boom"}` }, "service", true},
		{"allowance used", 402, func(string) string { return `{"error":"daily allowance used"}` }, "service", true},
		{"key refused", 401, func(string) string { return `{"error":"no"}` }, "service", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _ := withFetch(t, 3, hostOf, func(u string) (int, string, time.Duration) { return tc.status, tc.body(u), 0 })
			res := runVia(svc, "chat", map[string]any{"query": "캠핑"})
			if !res.OK || res.Output["status"] != "ok" || res.Output["pages_read"] != 0 || len(res.Output["results"].([]any)) != 3 {
				t.Fatalf("%+v", res)
			}
			if r := res.Output["results"].([]any)[0].(map[string]any); r["read"] != tc.read || r["snippet"] != "스니펫 0" {
				t.Fatalf("result: %v", r)
			}
			health, _ := out(t, svc, "providers.list", nil)["health"].(map[string]any)
			fetchH, _ := health["fetch"].(map[string]any)
			if got := fetchH != nil && fetchH["last_error"] != nil; got != tc.health {
				t.Fatalf("the reader's health records an error: %v, want %v (%v)", got, tc.health, fetchH)
			}
			if h, _ := health["tinyfish"].(map[string]any); h != nil && h["last_error"] != nil {
				t.Fatalf("the search service's health: %v", h)
			}
		})
	}
}

// Pages are counted one by one against the daily allowance; when it is used up the rest is snippets.
func TestSearchFetchQuotaIsPerPage(t *testing.T) {
	svc, fake := withFetch(t, 5, hostOf, func(string) (int, string, time.Duration) { return 200, okPage(campPage), 0 })
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{
		map[string]any{"path": []any{"web_search", "fetch_daily_limit"}, "value": 4},
		map[string]any{"path": []any{"web_search", "read_pages"}, "value": 3},
	}}); !r.OK {
		t.Fatalf("patch: %+v", r.Error)
	}
	res := runVia(svc, "chat", map[string]any{"query": "가평 캠핑장"})
	if res.Output["pages_read"] != 3 || fake.count() != 3 {
		t.Fatalf("first search: %+v", res.Output)
	}
	res = runVia(svc, "chat", map[string]any{"query": "가평 캠핑장 요금"})
	reads := []any{}
	for _, it := range res.Output["results"].([]any)[:3] {
		reads = append(reads, it.(map[string]any)["read"])
	}
	if fake.count() != 4 || res.Output["pages_read"] != 1 || reads[0] != "ok" || reads[1] != "skipped" || reads[2] != "skipped" {
		t.Fatalf("one unit left: %v, fetched %d, %+v", reads, fake.count(), res.Output["pages_read"])
	}
	res = runVia(svc, "chat", map[string]any{"query": "가평 캠핑장 후기"})
	if !res.OK || res.Output["pages_read"] != 0 || fake.count() != 4 || res.Output["status"] != "ok" {
		t.Fatalf("allowance used: snippets only: %+v", res.Output)
	}
}

// A host that blocked is not asked again for an hour; login-walled and non-public pages are never asked;
// Naver blog posts are read at their mobile address.
func TestSearchFetchHostsAndTargets(t *testing.T) {
	urls := []string{"https://shop.example.com/a", "https://cafe.naver.com/club/1", "https://blog.naver.com/someone/22333", "https://news.example.com/b", "http://10.0.0.5/x"}
	svc, fake := withFetch(t, 5, func(i int) string { return urls[i] }, func(u string) (int, string, time.Duration) {
		if strings.Contains(u, "shop.") {
			return 200, `{"results":[],"errors":[{"url":"` + u + `","error":"bot_blocked"}]}`, 0
		}
		return 200, okPage(campPage), 0
	})
	in := map[string]any{"query": "가평 캠핑장 요금", "pages": 5}
	res := runVia(svc, "chat", in)
	reads := []string{}
	for _, it := range res.Output["results"].([]any) {
		reads = append(reads, it.(map[string]any)["read"].(string))
	}
	if strings.Join(reads, ",") != "blocked,skipped,ok,ok,skipped" {
		t.Fatalf("reads: %v", reads)
	}
	if fake.saw("cafe.naver.com") || fake.saw("10.0.0.5") || !fake.saw("https://m.blog.naver.com/someone/22333") || fake.saw("https://blog.naver.com") {
		t.Fatalf("addresses asked: %v", fake.asked)
	}
	n := fake.count()
	res = runVia(svc, "chat", in)
	if fake.count() != n+2 || fake.saw("shop.example.com/a") && countOf(fake, "shop.") != 1 {
		t.Fatalf("the blocked host is not asked again: %v", fake.asked)
	}
	if r := res.Output["results"].([]any)[0].(map[string]any); r["read"] != "blocked" {
		t.Fatalf("remembered: %v", r)
	}
}

func countOf(f *fetchFake, sub string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, u := range f.asked {
		if strings.Contains(u, sub) {
			n++
		}
	}
	return n
}

// A slow page does not hold the answer: the soft deadline (once two pages are in) cuts it off.
func TestSearchFetchSoftDeadline(t *testing.T) {
	defer application.SetFetchSoftForTest(150 * time.Millisecond)()
	defer application.SetFetchDeadlineForTest("", 5*time.Second)()
	svc, _ := withFetch(t, 3, hostOf, func(u string) (int, string, time.Duration) {
		if strings.Contains(u, "site1.") {
			return 200, okPage(campPage), 8 * time.Second
		}
		return 200, okPage(campPage), 0
	})
	started := time.Now()
	res := runVia(svc, "chat", map[string]any{"query": "가평 캠핑장 요금"})
	if took := time.Since(started); took > 3*time.Second {
		t.Fatalf("held by the slow page for %v", took)
	}
	items := res.Output["results"].([]any)
	if items[0].(map[string]any)["read"] != "ok" || items[1].(map[string]any)["read"] != "timeout" || items[2].(map[string]any)["read"] != "ok" || res.Output["pages_read"] != 2 {
		t.Fatalf("%+v", res.Output)
	}
}

// Results that carry the service's own text of 300+ runes are cut from it: no page is opened.
func TestSearchUsesServiceTextBeforeFetching(t *testing.T) {
	var reqBody atomic.Value
	exa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		reqBody.Store(string(raw))
		text := strings.Repeat("관련 없는 이야기가 길게 이어집니다. ", 12) + "\n\n가평 캠핑장은 1박에 4만 원입니다.\n\n" + strings.Repeat("또 다른 이야기입니다. ", 20)
		b, _ := json.Marshal(map[string]any{"results": []any{
			map[string]any{"title": "E1", "url": "https://c.example/1", "highlights": []string{"하이라이트"}, "text": text},
			map[string]any{"title": "E2", "url": "https://c.example/2", "highlights": []string{"짧음"}},
		}, "costDollars": map[string]any{"total": 0.007}})
		_, _ = w.Write(b)
	}))
	defer exa.Close()
	svc := setupSvc(t)
	pointAt(t, svc, "exa", exa.URL, "E_KEY", "k")
	pointAt(t, svc, "tinyfish", "http://127.0.0.1:1", "TF_KEY", "tf")
	fake := newFetchFake(t, func(string) (int, string, time.Duration) { return 200, okPage(campPage), 0 })
	res := runVia(svc, "chat", map[string]any{"query": "가평 캠핑장 요금"})
	items := res.Output["results"].([]any)
	first, second := items[0].(map[string]any), items[1].(map[string]any)
	if first["read"] != "service" || !strings.Contains(first["passage"].(string), "1박에 4만 원") || first["snippet"] != "하이라이트" {
		t.Fatalf("first: %v", first)
	}
	if second["read"] != "skipped" || second["passage"] != nil || fake.count() != 0 {
		t.Fatalf("second (no text: an Exa result is not opened): %v fetched %d", second, fake.count())
	}
	if !strings.Contains(reqBody.Load().(string), `"text":{"maxCharacters":3000}`) {
		t.Fatalf("exa was not asked for page text: %v", reqBody.Load())
	}
	// no pages: exa is asked what it always was
	runVia(svc, "command", map[string]any{"query": "가평 캠핑장 요금"})
	if strings.Contains(reqBody.Load().(string), `"text"`) {
		t.Fatalf("the /search command asks for no page text: %v", reqBody.Load())
	}
}

type slowSearch struct {
	hits atomic.Int32
	srv  *httptest.Server
	h    atomic.Pointer[http.HandlerFunc]
}

// set replaces how the server answers (a race-free swap while it runs).
func (s *slowSearch) set(h http.HandlerFunc) { s.h.Store(&h) }

// newSlowSearch serves a Serper-shaped answer after delay (status 200) or the status alone; it counts hits.
func newSlowSearch(t *testing.T, delay time.Duration, status int, title, link string) *slowSearch {
	s := &slowSearch{}
	s.set(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		if status != 200 {
			w.WriteHeader(status)
			return
		}
		_, _ = w.Write([]byte(fmt.Sprintf(`{"organic":[{"title":%q,"link":%q,"snippet":"s"}]}`, title, link)))
	})
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.hits.Add(1)
		(*s.h.Load())(w, r)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// Every depth is served by the first service of the order in its own mode; the next one is asked only
// when that one fails or finds nothing. Deep waits up to 45 s for a service.
func TestDeepSearchUsesOneServiceAtATime(t *testing.T) {
	if got := application.SearchDeadlineForTest("deep"); got != 45*time.Second {
		t.Fatalf("deep deadline %v", got)
	}
	first := newSlowSearch(t, 0, 200, "A", "https://a.example/1")
	second := newSlowSearch(t, 0, 200, "B", "https://b.example/1")
	svc := setupSvc(t)
	pointAt(t, svc, "tavily", first.srv.URL, "T_KEY", "k")
	pointAt(t, svc, "serper", second.srv.URL, "S_KEY", "k")
	var body atomic.Value
	first.set(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		body.Store(string(raw))
		_, _ = w.Write([]byte(`{"results":[{"title":"A","url":"https://a.example/1","content":"본문"}]}`))
	})
	for _, depth := range []string{"quick", "", "deep"} {
		res := runVia(svc, "", map[string]any{"query": "캠핑 " + depth, "depth": depth})
		if !res.OK || res.Output["provider"] != "tavily" || second.hits.Load() != 0 {
			t.Fatalf("depth %q: only the first service is asked: %+v (second %d)", depth, res.Output, second.hits.Load())
		}
	}
	if !strings.Contains(body.Load().(string), `"search_depth":"advanced"`) {
		t.Fatalf("deep is the service's own deep mode: %v", body.Load())
	}
	// it fails: the next is asked, and the answer says so
	first.set(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })
	res := runVia(svc, "", map[string]any{"query": "캠핑 실패", "depth": "deep"})
	passed, _ := json.Marshal(res.Output["passed_over"])
	if res.Output["provider"] != "serper" || second.hits.Load() != 1 || !strings.Contains(string(passed), `"provider":"tavily"`) {
		t.Fatalf("hand over: %+v", res.Output)
	}
}

// Pages are opened by the provider that found them when it has a reader (TinyFish), by the configured
// reader when it has none (Serper, Kakao), and not at all for Exa, Linkup, Tavily or Brave results, which
// come with the service's text.
func TestSearchPageReaderFollowsTheProvider(t *testing.T) {
	// TinyFish search → TinyFish Fetch
	tf := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"title":"T0","url":"https://t0.example.com/p","snippet":"s"},{"title":"T1","url":"https://t1.example.com/p","snippet":"s"}]}`))
	}))
	defer tf.Close()
	svc := setupSvc(t)
	pointAt(t, svc, "tinyfish", tf.URL, "TF_KEY", "tf")
	fake := newFetchFake(t, func(string) (int, string, time.Duration) { return 200, okPage(campPage), 0 })
	res := runVia(svc, "chat", map[string]any{"query": "가평 캠핑장 요금"})
	if res.Output["provider"] != "tinyfish" || res.Output["pages_read"] != 2 || fake.count() != 2 {
		t.Fatalf("tinyfish: %+v (fetched %d)", res.Output, fake.count())
	}

	// Exa results without text are not opened, even with a TinyFish key configured
	exa := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"results":[{"title":"E","url":"https://e.example.com/1","highlights":["짧음"]}]}`))
	}))
	defer exa.Close()
	svc = setupSvc(t)
	pointAt(t, svc, "exa", exa.URL, "E_KEY", "k")
	pointAt(t, svc, "tinyfish", "http://127.0.0.1:1", "TF_KEY", "tf")
	before := fake.count()
	res = runVia(svc, "chat", map[string]any{"query": "가평 캠핑장 요금"})
	if r := res.Output["results"].([]any)[0].(map[string]any); res.Output["provider"] != "exa" || r["read"] != "skipped" || fake.count() != before {
		t.Fatalf("exa: %+v (fetched %d)", res.Output, fake.count()-before)
	}
	// "off" stops page reading for the providers that borrow a reader, too
	svc, fake = withFetch(t, 2, hostOf, func(string) (int, string, time.Duration) { return 200, okPage(campPage), 0 })
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"web_search", "fetch"}, "value": "off"}}}); !r.OK {
		t.Fatalf("fetch off: %+v", r.Error)
	}
	if res = runVia(svc, "chat", map[string]any{"query": "가평 캠핑장 요금"}); res.Output["pages_read"] != 0 || fake.count() != 0 {
		t.Fatalf("serper with fetch off: %+v", res.Output)
	}
}

// web.read goes through TinyFish Fetch when that is the reader, and falls back to Jina Reader.
func TestWebReadThroughTinyFishFetchThenJina(t *testing.T) {
	var jinaPath atomic.Value
	jina := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		jinaPath.Store(r.URL.Path)
		_, _ = w.Write([]byte(`{"code":200,"data":{"title":"젠","url":"https://news.example.com/a","content":"젠 본문","usage":{"tokens":3}}}`))
	}))
	defer jina.Close()
	failing := atomic.Bool{}
	svc := setupSvc(t)
	if r := exec(svc, "providers.add", map[string]any{"kind": "jina_reader"}); !r.OK {
		t.Fatalf("add reader: %+v", r)
	}
	pointAt(t, svc, "jina", jina.URL, "J_KEY", "")
	pointAt(t, svc, "tinyfish", "http://127.0.0.1:1", "TF_KEY", "tf")
	fake := newFetchFake(t, func(u string) (int, string, time.Duration) {
		if failing.Load() {
			return 200, `{"results":[],"errors":[{"url":"` + u + `","error":"timeout"}]}`, 0
		}
		return 200, okPage("# 제목\n\n본문 내용입니다."), 0
	})
	out(t, svc, "tools.configure", map[string]any{"id": "web.read", "enabled": "on"})
	page := out(t, svc, "web.read", map[string]any{"url": "https://news.example.com/a#x"})
	if page["source"] != "tinyfish_fetch" || page["provider"] != "tinyfish" || !strings.Contains(page["content"].(string), "본문 내용") || page["title"] != "페이지" || jinaPath.Load() != nil {
		t.Fatalf("tinyfish fetch: %v", page)
	}
	failing.Store(true)
	page = out(t, svc, "web.read", map[string]any{"url": "https://news.example.com/a"})
	if page["source"] != "jina_reader" || page["content"] != "젠 본문" || fake.count() != 2 {
		t.Fatalf("fallback: %v", page)
	}
	// "jina" as the chosen reader skips TinyFish
	if r := exec(svc, "config.patch", map[string]any{"ops": []any{map[string]any{"path": []any{"web_search", "fetch"}, "value": "jina"}}}); !r.OK {
		t.Fatalf("fetch: %+v", r.Error)
	}
	failing.Store(false)
	page = out(t, svc, "web.read", map[string]any{"url": "https://news.example.com/a"})
	if page["source"] != "jina_reader" || fake.count() != 2 {
		t.Fatalf("jina chosen: %v", page)
	}
}

// The price fallback (finance.quote → a web search) reads no pages even when a chat turn asked for it.
func TestQuoteFallbackSearchReadsNoPages(t *testing.T) {
	finn := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
		_, _ = w.Write([]byte(`{"error":"You don't have access to this resource."}`))
	}))
	defer finn.Close()
	svc, fake := withFetch(t, 3, hostOf, func(string) (int, string, time.Duration) { return 200, okPage(campPage), 0 })
	pointAt(t, svc, "finnhub", finn.URL, "F_KEY", "f")
	raw, _ := json.Marshal(map[string]any{"symbol": "005930.KS"})
	res := svc.Execute(context.Background(), action.Request{Action: "finance.quote", Input: raw, Locale: "en", Invoker: &action.Invoker{Via: "chat"}})
	if !res.OK || res.Output["source"] != "search" || fake.count() != 0 {
		t.Fatalf("quote from search: %+v (fetched %d)", res, fake.count())
	}
	for _, it := range res.Output["results"].([]any) {
		if _, has := it.(map[string]any)["read"]; has {
			t.Fatalf("a price line carries no page reads: %v", it)
		}
	}
}
