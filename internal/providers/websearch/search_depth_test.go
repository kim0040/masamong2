package websearch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/providers"
)

func fetchClient(t *testing.T) (*providers.Client, providers.Credential) {
	t.Helper()
	c := providers.NewClient(5*time.Second, nil)
	t.Setenv("FK", "fk")
	cred, _ := providers.Resolve("env:FK")
	return c, cred
}

// Fetch: one request per page, all at once; results (and errors[]) are read into the page that asked.
func TestTinyFishFetchParsesPagesAndErrors(t *testing.T) {
	var mu sync.Mutex
	var bodies []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		mu.Lock()
		bodies = append(bodies, b)
		mu.Unlock()
		if r.Header.Get("X-API-Key") != "fk" || r.Method != http.MethodPost {
			w.WriteHeader(401)
			return
		}
		u := b["urls"].([]any)[0].(string)
		switch {
		case strings.Contains(u, "ok."):
			_, _ = w.Write([]byte(`{"results":[{"url":"` + u + `","final_url":"https://ok.example.com/final","title":" 제목 ","text":"본문입니다. 두 번째 문장."}],"errors":[]}`))
		case strings.Contains(u, "blocked."):
			_, _ = w.Write([]byte(`{"results":[],"errors":[{"url":"` + u + `","error":"bot_blocked"}]}`))
		case strings.Contains(u, "login."):
			_, _ = w.Write([]byte(`{"results":[],"errors":[{"url":"` + u + `","error":"login_required"}]}`))
		case strings.Contains(u, "empty."):
			_, _ = w.Write([]byte(`{"results":[{"url":"` + u + `","text":"  "}],"errors":[]}`))
		case strings.Contains(u, "gone."):
			_, _ = w.Write([]byte(`{"results":[],"errors":[{"url":"` + u + `","error":"target_http_error","status":403}]}`))
		default:
			_, _ = w.Write([]byte(`{"results":[],"errors":[{"url":"` + u + `","error":"page_not_found"}]}`))
		}
	}))
	defer srv.Close()
	c, cred := fetchClient(t)
	f := NewTinyFishFetcher(c, srv.URL, cred)
	urls := []string{"https://ok.example.com/a#frag", "https://blocked.example.com/", "https://login.example.com/", "https://empty.example.com/", "https://gone.example.com/", "https://nowhere.example.com/", "http://127.0.0.1/admin"}
	got, err := f.Fetch(context.Background(), urls, 10)
	if err != nil {
		t.Fatalf("page errors are not the call's: %v", err)
	}
	want := []string{"", "blocked", "login", "empty", "blocked", "error", "not_public"}
	for i, w := range want {
		if got[i].Err != w {
			t.Errorf("url %d (%s): err %q, want %q", i, urls[i], got[i].Err, w)
		}
	}
	if got[0].Title != "제목" || got[0].URL != "https://ok.example.com/final" || got[0].Content != "본문입니다. 두 번…" || !got[0].Truncated {
		t.Errorf("page: %+v", got[0])
	}
	if len(bodies) != 6 { // the loopback address is never sent
		t.Fatalf("requests: %d", len(bodies))
	}
	for _, b := range bodies {
		if len(b["urls"].([]any)) != 1 || b["format"] != "markdown" || b["links"] != false || b["image_links"] != false || b["per_url_timeout_ms"] == nil {
			t.Fatalf("request body: %v", b)
		}
		if strings.Contains(b["urls"].([]any)[0].(string), "#") {
			t.Fatalf("fragment sent: %v", b["urls"])
		}
	}
}

// The service's own failures are the call's error (and a page has none): 402 and 429 and 5xx, and a
// refused key. No retry: one request each.
func TestTinyFishFetchServiceFailures(t *testing.T) {
	for _, status := range []int{401, 402, 429, 500} {
		var hits atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits.Add(1)
			w.WriteHeader(status)
			_, _ = w.Write([]byte(`{"error":"x"}`))
		}))
		c, cred := fetchClient(t)
		got, err := NewTinyFishFetcher(c, srv.URL, cred).Fetch(context.Background(), []string{"https://a.example.com/"}, 100)
		srv.Close()
		if err == nil || got[0].Err != "service" || hits.Load() != 1 {
			t.Errorf("status %d: err %v, page %+v, hits %d", status, err, got[0], hits.Load())
		}
		if status == 401 && !isAuth(err) {
			t.Errorf("401 must be ErrAuth: %v", err)
		}
	}
}

func isAuth(err error) bool { return err != nil && strings.Contains(err.Error(), ErrAuth.Error()) }

// A slow page does not hold the others: at the deadline the fast ones are returned and the slow one is
// "timeout". The soft deadline cuts it earlier once enough pages are in.
func TestTinyFishFetchPartialResultsAtTheDeadline(t *testing.T) {
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "slow.") {
			select {
			case <-release:
			case <-r.Context().Done():
			}
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"url":"u","title":"t","text":"본문 텍스트"}],"errors":[]}`))
	}))
	defer srv.Close()
	defer close(release)
	c, cred := fetchClient(t)
	f := NewTinyFishFetcher(c, srv.URL, cred)
	urls := []string{"https://fast1.example.com/", "https://slow.example.com/", "https://fast2.example.com/"}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	started := time.Now()
	got, err := f.Fetch(ctx, urls, 100)
	cancel()
	if err != nil || time.Since(started) > 2*time.Second {
		t.Fatalf("hard deadline: err %v, took %v", err, time.Since(started))
	}
	if got[0].Err != "" || got[2].Err != "" || got[1].Err != "timeout" || got[1].URL != urls[1] {
		t.Fatalf("partial: %+v", got)
	}

	ctx = WithSoftDeadline(context.Background(), 2, 100*time.Millisecond)
	ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	started = time.Now()
	got, _ = f.Fetch(ctx, urls, 100)
	if took := time.Since(started); took > 2*time.Second || got[0].Err != "" || got[2].Err != "" || got[1].Err != "timeout" {
		t.Fatalf("soft deadline: took %v, %+v", took, got)
	}
}

// Jina Reader as the fetcher: a page, a page that is not public, and a failing host.
func TestJinaFetcher(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "bad.") {
			w.WriteHeader(422)
			return
		}
		_, _ = w.Write([]byte(`{"code":200,"data":{"title":"공지","url":"https://news.example.com/a","content":"# 공지\n\n내일 휴강입니다.","usage":{"tokens":12}}}`))
	}))
	defer srv.Close()
	c := providers.NewClient(5*time.Second, nil)
	f := NewJinaFetcher(NewReader(c, srv.URL, providers.Credential{}))
	got, err := f.Fetch(context.Background(), []string{"https://news.example.com/a", "https://bad.example.com/", "http://10.0.0.1/"}, 1000)
	if err != nil || got[0].Err != "" || got[0].Title != "공지" || got[1].Err != "error" || got[2].Err != "not_public" {
		t.Fatalf("jina: %v %+v", err, got)
	}
	if f.Kind() != "jina_reader" {
		t.Fatal(f.Kind())
	}
}

func TestHostCache(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	h := NewHostCache()
	h.Now = func() time.Time { return now }
	if _, skip := h.Skip("https://namu.example.com/x"); skip {
		t.Fatal("unknown host")
	}
	h.Note("https://www.shop.example.com/a", "blocked")
	if why, skip := h.Skip("https://shop.example.com/b"); !skip || why != "blocked" {
		t.Fatalf("blocked is remembered at once, www or not: %q %v", why, skip)
	}
	h.Note("https://wiki.example.com/a", "timeout")
	if _, skip := h.Skip("https://wiki.example.com/b"); skip {
		t.Fatal("one timeout is not the host")
	}
	h.Note("https://wiki.example.com/c", "timeout")
	if why, skip := h.Skip("https://wiki.example.com/b"); !skip || why != "timeout" {
		t.Fatalf("second timeout: %q %v", why, skip)
	}
	h.Note("https://login.example.com/", "login")
	if why, _ := h.Skip("https://login.example.com/"); why != "blocked" {
		t.Fatalf("login reads as blocked: %q", why)
	}
	h.Note("https://x.example.com/", "error") // other errors are not remembered
	if _, skip := h.Skip("https://x.example.com/"); skip {
		t.Fatal("plain errors are not remembered")
	}
	now = now.Add(61 * time.Minute)
	if _, skip := h.Skip("https://shop.example.com/b"); skip {
		t.Fatal("an hour later the host is asked again")
	}
	var nilCache *HostCache
	nilCache.Note("https://a.example.com/", "blocked")
	if _, skip := nilCache.Skip("https://a.example.com/"); skip {
		t.Fatal("nil cache")
	}
}

func TestCanonicalURLAndMergeResults(t *testing.T) {
	same := []string{
		"https://www.example.com/a/b/?utm_source=x&utm_medium=y#top",
		"http://example.com/a/b",
		"https://m.example.com/a/b/?fbclid=1",
		"HTTPS://Example.com:443/a/b?gclid=2",
	}
	for _, u := range same {
		if got := CanonicalURL(u); got != "https://example.com/a/b" {
			t.Errorf("%s -> %s", u, got)
		}
	}
	if CanonicalURL("https://example.com/a?id=2&x=1") != CanonicalURL("https://example.com/a?x=1&id=2&utm_campaign=z") {
		t.Error("parameters that matter are kept, in any order")
	}
	if CanonicalURL("https://example.com/a?id=1") == CanonicalURL("https://example.com/a?id=2") {
		t.Error("different pages")
	}
	if CanonicalURL("https://example.com/") != "https://example.com" || CanonicalURL("not a url") != "not a url" {
		t.Error("root / unparsable")
	}

	a := []Result{{Title: "A1", URL: "https://a.example/1"}, {Title: "A2", URL: "https://www.b.example/2?utm_source=a", Snippet: "s"}, {Title: "A3", URL: "https://c.example/3"}}
	b := []Result{{Title: "B1", URL: "https://b.example/2", Text: "text from b", Published: "2026-11-01"}, {Title: "B2", URL: "https://d.example/4"}}
	got := MergeResults(a, b)
	var titles []string
	for _, r := range got {
		titles = append(titles, r.Title)
	}
	// rank 0: A1, B1 (= A2's page? no: B1 is b.example/2, A2 is rank 1); rank 1: A2 duplicates B1
	if strings.Join(titles, ",") != "A1,B1,B2,A3" {
		t.Fatalf("order: %v", titles)
	}
	if got[1].Snippet != "s" || got[1].Text != "text from b" {
		t.Fatalf("the copy kept is filled from the dropped one: %+v", got[1])
	}
	if again := MergeResults(a, b); len(again) != len(got) || again[0] != got[0] {
		t.Fatal("not stable")
	}
	if len(MergeResults()) != 0 || len(MergeResults(nil, a)) != 3 {
		t.Fatal("empty lists")
	}
}

const koPage = `# 서울 날씨 안내

서울 지역의 오늘 날씨는 대체로 맑겠으며 낮 최고기온은 18도까지 오르겠습니다.

[홈](https://x.example/) [뉴스](https://x.example/news) [스포츠](https://x.example/sport)

- [로그인](https://x.example/login)
- [회원가입](https://x.example/join)

![광고 배너](https://x.example/ad.png)

프로야구 한국시리즈가 어제 막을 내렸습니다. 우승팀은 마지막 경기에서 역전승을 거뒀습니다.

서울에서는 내일 오후부터 비가 내릴 것으로 예상되며, 강수량은 5~10mm 정도입니다. 자세한 내용은 [기상청 예보](https://kma.example/forecast)를 참고하세요. https://kma.example/raw

연예계 소식: 새 드라마가 다음 주에 첫 방송을 시작합니다.

서울의 날씨는 주말에 다시 맑아지고 아침 기온은 영하권으로 떨어질 수 있습니다.

쿠키 정책 및 개인정보 처리방침.`

func TestPassagesKorean(t *testing.T) {
	got := Passages("서울의 날씨가 어때", koPage, 400)
	if r := []rune(got); len(r) > 400 {
		t.Fatalf("cap: %d runes", len(r))
	}
	// the page's first paragraph, then the weather paragraphs, in page order; the baseball and TV ones are left out
	for _, want := range []string{"서울 날씨 안내", "낮 최고기온은 18도", "내일 오후부터 비가"} {
		if !strings.Contains(got, want) {
			t.Errorf("missing %q in:\n%s", want, got)
		}
	}
	for _, bad := range []string{"프로야구", "드라마", "로그인", "http", "](", "광고 배너", "kma.example"} {
		if strings.Contains(got, bad) {
			t.Errorf("%q must not be in:\n%s", bad, got)
		}
	}
	if !strings.Contains(got, "기상청 예보를 참고하세요") {
		t.Errorf("a link keeps its text:\n%s", got)
	}
	if strings.Index(got, "낮 최고기온") > strings.Index(got, "내일 오후부터") {
		t.Errorf("page order kept:\n%s", got)
	}

	// room for everything that matches: the later weather paragraph comes too, and still in page order
	long := Passages("서울의 날씨가 어때", koPage, 1500)
	if !strings.Contains(long, "주말에 다시 맑아지고") || strings.Index(long, "내일 오후부터") > strings.Index(long, "주말에") {
		t.Errorf("long:\n%s", long)
	}
	// nothing matches: the beginning of the page
	if none := Passages("zzz qqq", koPage, 200); !strings.HasPrefix(none, "서울 날씨 안내") || len([]rune(none)) > 200 {
		t.Errorf("no match: %q", none)
	}
	if Passages("x", "", 100) != "" || Passages("x", koPage, 0) != "" {
		t.Error("empty")
	}
}

func TestPassagesCutsLongParagraphAndEnglish(t *testing.T) {
	var b strings.Builder
	for i := 0; i < 40; i++ {
		b.WriteString("Filler sentence number about nothing in particular. ")
	}
	b.WriteString("The Ryzen 9 9950X3D launched at 699 dollars with 16 cores. ")
	for i := 0; i < 40; i++ {
		b.WriteString("More filler sentence about something unrelated here. ")
	}
	got := Passages("ryzen 9950x3d price", b.String(), 500)
	if !strings.Contains(got, "699 dollars") || len([]rune(got)) > 500 {
		t.Fatalf("a long single paragraph is cut at sentences and the match found (%d runes): %s", len([]rune(got)), got)
	}
	if strings.Count(got, "\n\n") > 0 {
		t.Errorf("parts of one paragraph are joined with a space: %q", got)
	}
}

func TestPassagesDropsMenusAndImages(t *testing.T) {
	page := "Skip\n\n* [Home](/) \n* [About](/about)\n* [Contact](/contact)\n\n![logo](https://x.example/l.png)\n\nThe museum opens at nine and closes at six on weekdays. Tickets cost twelve dollars.\n\n[Read more](https://x.example/more)"
	got := Passages("museum opening hours", page, 300)
	if got != "The museum opens at nine and closes at six on weekdays. Tickets cost twelve dollars." {
		t.Fatalf("menus, labels, images and link-only lines dropped: %q", got)
	}
}

// ---------------------------------------------------------------------------
// what each service is asked at each depth, and the page text it gives back (PROVIDERS.md)
// ---------------------------------------------------------------------------

type seenReq struct {
	path, qs string
	body     map[string]any
}

func serve(t *testing.T, kind string, reply string, q Query) (seenReq, Response) {
	t.Helper()
	var got seenReq
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		got = seenReq{path: r.URL.Path, qs: r.URL.RawQuery}
		_ = json.Unmarshal(raw, &got.body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(reply))
	}))
	defer srv.Close()
	c := providers.NewClient(5*time.Second, nil)
	c.MaxRetries = 0
	t.Setenv("K", "k")
	cred, _ := providers.Resolve("env:K")
	s, err := New(kind, c, srv.URL, cred)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Search(context.Background(), q)
	if err != nil {
		t.Fatal(err)
	}
	return got, res
}

func TestExaPageTextRidesAlongOnlyWhenAsked(t *testing.T) {
	reply := `{"results":[{"title":"E1","url":"https://c.example/1","highlights":["첫 부분","둘째 부분"],"text":"전체 본문 첫 문단.\n\n둘째 문단입니다."}],"costDollars":{"total":0.007}}`
	contents := func(m map[string]any) map[string]any { return m["contents"].(map[string]any) }

	got, res := serve(t, "exa", reply, Query{Text: "x"})
	if c := contents(got.body); c["text"] != nil || c["maxAgeHours"] != nil || c["highlights"].(map[string]any)["maxCharacters"] != float64(700) || got.body["type"] != "auto" {
		t.Errorf("without Full the request is the old one: %v", got.body)
	}
	if res.Results[0].Snippet != "첫 부분 … 둘째 부분" || res.Results[0].Text != "전체 본문 첫 문단.\n\n둘째 문단입니다." {
		t.Errorf("snippet from the highlights, text from the page: %+v", res.Results[0])
	}

	got, _ = serve(t, "exa", reply, Query{Text: "x", Full: true, Fresh: "day"})
	if c := contents(got.body); c["text"].(map[string]any)["maxCharacters"] != float64(3000) || c["maxAgeHours"] != float64(24) || c["highlights"].(map[string]any)["maxCharacters"] != float64(700) {
		t.Errorf("standard with pages: %v", got.body)
	}
	got, _ = serve(t, "exa", reply, Query{Text: "x", Full: true, Depth: DepthDeep, Extra: []string{" 둘째 질의 ", "", "a", "b", "c"}})
	if c := contents(got.body); c["text"].(map[string]any)["maxCharacters"] != float64(4000) || got.body["type"] != "deep-lite" || c["maxAgeHours"] != nil {
		t.Errorf("deep with pages: %v", got.body)
	}
	if aq, _ := got.body["additionalQueries"].([]any); len(aq) != 3 || aq[0] != "둘째 질의" || aq[2] != "b" {
		t.Errorf("up to three extra queries on a deep search: %v", got.body["additionalQueries"])
	}
	if got, _ = serve(t, "exa", reply, Query{Text: "x", Extra: []string{"y"}}); got.body["additionalQueries"] != nil {
		t.Errorf("only the deep types take extra queries: %v", got.body)
	}

	// no highlights: the snippet is the text, clipped; the Text is not clipped to the snippet's length
	long := strings.Repeat("가나다라마바사 ", 200)
	_, res = serve(t, "exa", `{"results":[{"title":"E","url":"https://c.example/2","text":"`+long+`"}]}`, Query{Text: "x"})
	if r := []rune(res.Results[0].Snippet); len(r) != 701 {
		t.Errorf("snippet is clipped to 700: %d", len(r))
	}
	if r := []rune(res.Results[0].Text); len(r) <= 701 || len(r) > 4001 {
		t.Errorf("text is not clipped to the snippet: %d", len(r))
	}
}

func TestServiceTextIsKeptPastTheSnippet(t *testing.T) {
	body := strings.Repeat("문장이 이어집니다. ", 120) // ~1,100 runes
	for kind, reply := range map[string]string{
		"tavily": `{"results":[{"title":"T","url":"https://a.example/1","content":"` + body + `"}]}`,
		"linkup": `{"results":[{"type":"text","name":"L","url":"https://a.example/1","content":"` + body + `"}]}`,
		"brave":  `{"web":{"results":[{"title":"B","url":"https://a.example/1","description":"` + body + `","extra_snippets":["추가 조각"]}]}}`,
	} {
		_, res := serve(t, kind, reply, Query{Text: "x"})
		r := res.Results[0]
		if len([]rune(r.Snippet)) != 701 || len([]rune(r.Text)) < 1000 {
			t.Errorf("%s: snippet %d runes, text %d runes", kind, len([]rune(r.Snippet)), len([]rune(r.Text)))
		}
	}
	_, res := serve(t, "brave", `{"web":{"results":[{"title":"B","url":"https://a.example/1","description":"설명","extra_snippets":["추가 조각"]}]}}`, Query{Text: "x"})
	if res.Results[0].Text != "설명\n\n추가 조각" {
		t.Errorf("brave text: %q", res.Results[0].Text)
	}
	_, res = serve(t, "brave", `{"grounding":{"generic":[{"url":"https://g.example/1","title":"G","snippets":["첫 조각","둘째 조각"]}]}}`, Query{Text: "x", Depth: DepthDeep})
	if res.Results[0].Text != "첫 조각\n\n둘째 조각" || res.Results[0].Snippet != "첫 조각 … 둘째 조각" {
		t.Errorf("brave context: %+v", res.Results[0])
	}
}

func TestTavilyChunksAndBraveDeepParameters(t *testing.T) {
	for depth, want := range map[string]any{DepthQuick: nil, "": float64(3), DepthDeep: float64(3)} {
		got, _ := serve(t, "tavily", `{"results":[]}`, Query{Text: "x", Depth: depth})
		if got.body["chunks_per_source"] != want {
			t.Errorf("tavily %q: chunks_per_source %v, want %v", depth, got.body["chunks_per_source"], want)
		}
	}
	got, _ := serve(t, "brave", `{"grounding":{"generic":[]}}`, Query{Text: "x", Depth: DepthDeep, Max: 4})
	for _, p := range []string{"count=20", "maximum_number_of_urls=4", "maximum_number_of_tokens=8192", "maximum_number_of_tokens_per_url=2048", "context_threshold_mode=balanced"} {
		if !strings.Contains(got.qs, p) {
			t.Errorf("brave deep lacks %s: %s", p, got.qs)
		}
	}
}

func TestKakaoDeepAsksWebAndBlogAtOnce(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	fail := false
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		switch {
		case r.URL.Path == "/v2/search/blog" && fail:
			w.WriteHeader(500)
		case r.URL.Path == "/v2/search/blog":
			_, _ = w.Write([]byte(`{"documents":[{"title":"<b>블로그</b> 글","contents":"후기","url":"https://blog.example/1","datetime":"2026-11-01T00:00:00.000+09:00"}],"meta":{}}`))
		default:
			_, _ = w.Write([]byte(`{"documents":[{"title":"웹 문서","contents":"내용","url":"https://web.example/1","datetime":""}],"meta":{}}`))
		}
	}))
	defer srv.Close()
	c := providers.NewClient(5*time.Second, nil)
	c.MaxRetries = 0
	t.Setenv("K", "k")
	cred, _ := providers.Resolve("env:K")
	s, _ := New("kakao_search", c, srv.URL, cred)

	res, err := s.Search(context.Background(), Query{Text: "x"})
	if err != nil || len(res.Results) != 1 || len(paths) != 1 || paths[0] != "/v2/search/web" {
		t.Fatalf("standard: web only: %v %v %+v", err, paths, res)
	}
	paths = nil
	res, err = s.Search(context.Background(), Query{Text: "x", Depth: DepthDeep})
	if err != nil || len(paths) != 2 || len(res.Results) != 2 || res.Results[0].Title != "웹 문서" || res.Results[1].Title != "블로그 글" {
		t.Fatalf("deep: web and blog: %v %v %+v", err, paths, res)
	}
	fail = true
	res, err = s.Search(context.Background(), Query{Text: "x", Depth: DepthDeep})
	if err != nil || len(res.Results) != 1 {
		t.Fatalf("the blog search failing leaves the web results: %v %+v", err, res)
	}
}

func TestFetchTarget(t *testing.T) {
	for in, want := range map[string]string{
		"https://blog.naver.com/someone/223456789?x=1#top": "https://m.blog.naver.com/someone/223456789",
		"https://blog.naver.com/someone":                   "https://blog.naver.com/someone",
		"https://news.example.com/a#frag":                  "https://news.example.com/a",
		"https://cafe.naver.com/club/1":                    "",
		"https://cafe.daum.net/club/2":                     "",
		"http://192.168.0.2/x":                             "",
	} {
		got, ok := FetchTarget(in)
		if got != want || ok != (want != "") {
			t.Errorf("%s -> %q %v, want %q", in, got, ok, want)
		}
	}
}
