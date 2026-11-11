package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"masamong/internal/providers"
)

// Each service is served the response shape its API reference shows (read 2026-09-30); the test
// checks the request it receives (address, key header, fields) and what search.web gets back.
func TestEachServiceRequestAndResponse(t *testing.T) {
	cases := []struct {
		kind, path, keyHeader, keyValue, reply string
		wantField                              map[string]any
		wantTitle, wantURL, wantAnswer         string
	}{
		{"tavily", "/search", "Authorization", "Bearer k", `{"query":"q","answer":"요약 답","results":[{"title":"T1","url":"https://a.example/1","content":"본문 1","score":0.9,"published_date":"2026-09-29"}],"response_time":0.8,"usage":{"credits":1}}`,
			map[string]any{"query": "캠핑 장비", "max_results": float64(3), "time_range": "week"}, "T1", "https://a.example/1", "요약 답"},
		{"linkup", "/v1/search", "Authorization", "Bearer k", `{"results":[{"type":"text","name":"L1","url":"https://b.example/1","content":"내용","favicon":"x"},{"type":"image","name":"img","url":"https://b.example/i.png"}]}`,
			map[string]any{"q": "캠핑 장비", "depth": "standard", "outputType": "searchResults", "maxResults": float64(3)}, "L1", "https://b.example/1", ""},
		{"exa", "/search", "x-api-key", "k", `{"requestId":"r","results":[{"title":"E1","url":"https://c.example/1","publishedDate":"2026-09-01T00:00:00.000Z","author":null,"highlights":["첫 부분","둘째 부분"]}],"costDollars":{"total":0.007}}`,
			map[string]any{"query": "캠핑 장비", "numResults": float64(3), "type": "auto"}, "E1", "https://c.example/1", ""},
		{"serper", "/search", "X-API-KEY", "k", `{"searchParameters":{"q":"q"},"answerBox":{"snippet":"상자 답"},"organic":[{"title":"S1","link":"https://d.example/1","snippet":"스니펫","date":"2 days ago","position":1}]}`,
			map[string]any{"q": "캠핑 장비", "num": float64(3), "gl": "kr", "hl": "ko", "tbs": "qdr:w"}, "S1", "https://d.example/1", "상자 답"},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			var got map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != tc.path || r.Header.Get(tc.keyHeader) != tc.keyValue {
					w.WriteHeader(401)
					_, _ = w.Write([]byte(`{"detail":{"error":"Unauthorized: missing or invalid API key."}}`))
					return
				}
				raw, _ := io.ReadAll(r.Body)
				_ = json.Unmarshal(raw, &got)
				_, _ = w.Write([]byte(tc.reply))
			}))
			defer srv.Close()
			c := providers.NewClient(5*time.Second, nil)
			c.MaxRetries = 0
			t.Setenv("K", "k")
			cred, _ := providers.Resolve("env:K")
			s, err := New(tc.kind, c, srv.URL, cred)
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.Search(context.Background(), Query{Text: "캠핑 장비", Max: 3, Fresh: "week"})
			if err != nil {
				t.Fatal(err)
			}
			for k, v := range tc.wantField {
				if got[k] != v {
					t.Errorf("request %s = %v, want %v (%v)", k, got[k], v, got)
				}
			}
			if len(res.Results) != 1 || res.Results[0].Title != tc.wantTitle || res.Results[0].URL != tc.wantURL || res.Results[0].Snippet == "" || res.Answer != tc.wantAnswer {
				t.Fatalf("response: %+v", res)
			}
			if tc.kind == "exa" && (res.CostUSD == nil || *res.CostUSD != 0.007 || res.Results[0].Snippet != "첫 부분 … 둘째 부분") {
				t.Fatalf("exa cost and highlights: %+v", res)
			}
			// a wrong key is ErrAuth with the service's message
			t.Setenv("K", "wrong")
			bad, _ := providers.Resolve("env:K")
			s2, _ := New(tc.kind, c, srv.URL, bad)
			if _, err := s2.Search(context.Background(), Query{Text: "x"}); !errors.Is(err, ErrAuth) || !strings.Contains(err.Error(), "invalid API key") {
				t.Fatalf("bad key: %v", err)
			}
		})
	}
}

func TestLinkupFreshnessIsADate(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	if freshFrom("week", now) != "2026-09-23" || freshFrom("", now) != "" {
		t.Fatal(freshFrom("week", now))
	}
}

func TestPublicURL(t *testing.T) {
	for raw, ok := range map[string]bool{
		"https://news.example.com/a?b=1#top": true, "http://example.org": true,
		"http://localhost:8787/": false, "http://127.0.0.1/": false, "http://10.0.0.5/x": false, "http://192.168.1.1": false,
		"http://[::1]/": false, "http://169.254.169.254/latest/meta-data": false, "https://intranet/": false,
		"https://printer.local/": false, "https://user:pw@example.com/": false, "ftp://example.com/": false,
		"https://example.com:8443/": false, "file:///etc/passwd": false, "https://db.internal/": false,
	} {
		got, err := PublicURL(raw)
		if (err == nil) != ok {
			t.Errorf("%s: %v", raw, err)
		}
		if ok && strings.Contains(got, "#") {
			t.Errorf("%s: fragment kept", got)
		}
	}
}

func TestReaderJSON(t *testing.T) {
	var path, accept, auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, accept, auth = r.URL.Path, r.Header.Get("Accept"), r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"code":200,"status":20000,"data":{"title":"기사","url":"https://news.example.com/a","content":"# 기사\n\n본문입니다. 긴 내용","usage":{"tokens":42}}}`))
	}))
	defer srv.Close()
	c := providers.NewClient(5*time.Second, nil)
	r := NewReader(c, srv.URL, providers.Credential{})
	p, err := r.Read(context.Background(), "https://news.example.com/a", 8)
	if err != nil {
		t.Fatal(err)
	}
	if path != "/https://news.example.com/a" || accept != "application/json" || auth != "" {
		t.Fatalf("request: %s %s %q", path, accept, auth)
	}
	if p.Title != "기사" || !p.Truncated || p.Content != "# 기사\n\n본문…" || p.Tokens != 42 {
		t.Fatalf("page: %+v", p)
	}
	if _, err := r.Read(context.Background(), "http://127.0.0.1:8787/api", 100); !errors.Is(err, ErrNotPublic) {
		t.Fatalf("private address: %v", err)
	}
}

// Depth and topic reach each service in its own words (read from the API references 2026-10-15), and
// Serper's news search is read from news[], with the outlet ahead of the snippet.
func TestDepthAndNewsPerService(t *testing.T) {
	type seen struct {
		path string
		body map[string]any
		qs   string
	}
	cases := []struct {
		kind  string
		q     Query
		reply string
		check func(t *testing.T, s seen, res Response)
	}{
		{"tavily", Query{Text: "환율", Depth: DepthQuick}, `{"results":[]}`, func(t *testing.T, s seen, _ Response) {
			if s.body["search_depth"] != "fast" || s.body["country"] != "south korea" || s.body["topic"] != nil {
				t.Errorf("quick: %v", s.body)
			}
		}},
		{"tavily", Query{Text: "world news", Depth: DepthDeep, Topic: TopicNews, Fresh: "day"}, `{"results":[]}`, func(t *testing.T, s seen, _ Response) {
			if s.body["search_depth"] != "advanced" || s.body["topic"] != "news" || s.body["chunks_per_source"] != float64(3) ||
				s.body["include_answer"] != "advanced" || s.body["time_range"] != "day" || s.body["country"] != nil {
				t.Errorf("deep news: %v", s.body)
			}
		}},
		{"linkup", Query{Text: "x", Depth: DepthQuick}, `{"results":[]}`, func(t *testing.T, s seen, _ Response) {
			if s.body["depth"] != "flash" {
				t.Errorf("quick: %v", s.body)
			}
		}},
		{"linkup", Query{Text: "x", Depth: DepthDeep}, `{"results":[]}`, func(t *testing.T, s seen, _ Response) {
			if s.body["depth"] != "deep" {
				t.Errorf("deep: %v", s.body)
			}
		}},
		{"exa", Query{Text: "x", Depth: DepthDeep, Topic: TopicNews, Fresh: "week"}, `{"results":[]}`, func(t *testing.T, s seen, _ Response) {
			if s.body["type"] != "deep-lite" || s.body["category"] != "news" || s.body["startPublishedDate"] == nil {
				t.Errorf("deep news: %v", s.body)
			}
		}},
		{"exa", Query{Text: "x", Depth: DepthQuick, Fresh: "week"}, `{"results":[]}`, func(t *testing.T, s seen, _ Response) {
			if s.body["type"] != "instant" || s.body["category"] != nil {
				t.Errorf("a recent page is not only news: %v", s.body)
			}
		}},
		{"serper", Query{Text: "국제 뉴스", Topic: TopicNews, Fresh: "day"}, `{"news":[{"title":"N1","link":"https://n.example/1","snippet":"요약","date":"3 hours ago","source":"연합뉴스","position":1}]}`,
			func(t *testing.T, s seen, res Response) {
				if s.path != "/news" || s.body["tbs"] != "qdr:d" || s.body["gl"] != "kr" {
					t.Errorf("news request: %s %v", s.path, s.body)
				}
				if len(res.Results) != 1 || res.Results[0].Snippet != "연합뉴스 — 요약" || res.Results[0].Published != "3 hours ago" {
					t.Errorf("news reply: %+v", res)
				}
			}},
		{"brave", Query{Text: "환율", Max: 3, Fresh: "week"}, `{"type":"search","query":{"original":"환율"},"web":{"type":"search","results":[{"title":"<strong>환율</strong> 오늘","url":"https://e.example/1","description":"원/달러 <strong>환율</strong>","page_age":"2026-10-14T09:00:00","extra_snippets":["더 보기"]}]}}`,
			func(t *testing.T, s seen, res Response) {
				if s.path != "/res/v1/web/search" || !strings.Contains(s.qs, "freshness=pw") || !strings.Contains(s.qs, "country=KR") || !strings.Contains(s.qs, "extra_snippets=true") || !strings.Contains(s.qs, "text_decorations=false") {
					t.Errorf("web request: %s %s", s.path, s.qs)
				}
				if len(res.Results) != 1 || res.Results[0].Title != "환율 오늘" || res.Results[0].Snippet != "원/달러 환율 … 더 보기" || res.Results[0].Published != "2026-10-14T09:00:00" {
					t.Errorf("web reply: %+v", res)
				}
			}},
		{"brave", Query{Text: "x", Topic: TopicNews}, `{"type":"news","results":[{"type":"news_result","title":"B1","url":"https://f.example/1","description":"d","age":"2 hours ago","profile":{"name":"Outlet"}}]}`,
			func(t *testing.T, s seen, res Response) {
				if s.path != "/res/v1/news/search" || len(res.Results) != 1 || res.Results[0].Snippet != "Outlet — d" || res.Results[0].Published != "2 hours ago" {
					t.Errorf("news: %s %+v", s.path, res)
				}
			}},
		{"brave", Query{Text: "x", Depth: DepthDeep, Max: 4}, `{"grounding":{"generic":[{"url":"https://g.example/1","title":"G1","snippets":["chunk one","chunk two"]}],"map":[]},"sources":{"https://g.example/1":{"title":"G1","hostname":"g.example","age":["Wednesday, January 15, 2025","2025-01-15","392 days ago","2025-01-15T13:45:02Z"]}}}`,
			func(t *testing.T, s seen, res Response) {
				if s.path != "/res/v1/llm/context" || !strings.Contains(s.qs, "maximum_number_of_urls=4") {
					t.Errorf("deep request: %s %s", s.path, s.qs)
				}
				if len(res.Results) != 1 || res.Results[0].Snippet != "chunk one … chunk two" || res.Results[0].Published != "2025-01-15T13:45:02Z" {
					t.Errorf("deep reply: %+v", res)
				}
			}},
		{"tinyfish", Query{Text: "국제 뉴스", Topic: TopicNews, Fresh: "day", Max: 1}, `{"query":"국제 뉴스","results":[{"position":1,"site_name":"a.example","title":"T1","snippet":"s1","url":"https://a.example/1","date":"2026-10-15","publisher":"Pub"},{"position":2,"site_name":"b.example","title":"T2","snippet":"s2","url":"https://b.example/2"}],"total_results":2,"page":0}`,
			func(t *testing.T, s seen, res Response) {
				if s.path != "/" || !strings.Contains(s.qs, "domain_type=news") || !strings.Contains(s.qs, "recency_minutes=1440") || !strings.Contains(s.qs, "language=ko") || !strings.Contains(s.qs, "location=KR") {
					t.Errorf("request: %s %s", s.path, s.qs)
				}
				if len(res.Results) != 1 || res.Results[0].Snippet != "Pub — s1" || res.Results[0].Published != "2026-10-15" {
					t.Errorf("reply (cut to size): %+v", res)
				}
			}},
	}
	for _, tc := range cases {
		t.Run(tc.kind, func(t *testing.T) {
			var got seen
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				got = seen{path: r.URL.Path, qs: r.URL.RawQuery}
				_ = json.Unmarshal(raw, &got.body)
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.reply))
			}))
			defer srv.Close()
			c := providers.NewClient(5*time.Second, nil)
			c.MaxRetries = 0
			t.Setenv("K", "k")
			cred, _ := providers.Resolve("env:K")
			s, err := New(tc.kind, c, srv.URL, cred)
			if err != nil {
				t.Fatal(err)
			}
			res, err := s.Search(context.Background(), tc.q)
			if err != nil {
				t.Fatal(err)
			}
			tc.check(t, got, res)
		})
	}
}

// Brave and TinyFish take their keys in their own headers; a refused key is ErrAuth.
func TestBraveAndTinyFishKeys(t *testing.T) {
	for kind, header := range map[string]string{"brave": "X-Subscription-Token", "tinyfish": "X-API-Key"} {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get(header) != "k" {
				w.WriteHeader(401)
				_, _ = w.Write([]byte(`{"error":{"message":"invalid API key"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"query":{"original":"x"},"results":[]}`))
		}))
		c := providers.NewClient(5*time.Second, nil)
		c.MaxRetries = 0
		t.Setenv("K", "k")
		good, _ := providers.Resolve("env:K")
		s, _ := New(kind, c, srv.URL, good)
		if _, err := s.Search(context.Background(), Query{Text: "x"}); err != nil {
			t.Errorf("%s: %v", kind, err)
		}
		t.Setenv("K", "wrong")
		bad, _ := providers.Resolve("env:K")
		s, _ = New(kind, c, srv.URL, bad)
		if _, err := s.Search(context.Background(), Query{Text: "x"}); !errors.Is(err, ErrAuth) {
			t.Errorf("%s bad key: %v", kind, err)
		}
		srv.Close()
	}
}

// Answers shaped like the ones the services returned on 2026-11-07, with the text replaced by made-up
// values: extra fields, nulls and empty snippets are read without error and give the pages.
func TestSampleResponsesPerService(t *testing.T) {
	obj := func(raw string) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	t.Run("exa", func(t *testing.T) {
		res, err := ParseExa(obj(`{"requestId":"r1","resolvedSearchType":"","results":[
			{"id":"https://a.example/1","title":"가짜 제목","url":"https://a.example/1","publishedDate":"2026-11-06T09:00:00.000Z","author":null,"image":"https://a.example/i.png","favicon":"https://a.example/f.ico","highlights":["첫째 조각","둘째 조각"],"highlightScores":[0.5,0.4]},
			{"id":"https://b.example/2","title":"","url":"https://b.example/2","publishedDate":null,"author":"x","highlights":[]}],
			"costDollars":{"total":0.012}}`))
		if err != nil || len(res.Results) != 2 || res.Results[0].Snippet != "첫째 조각 … 둘째 조각" || res.Results[0].Published != "2026-11-06T09:00:00.000Z" || res.Results[1].Published != "" {
			t.Fatalf("%+v %v", res, err)
		}
		if res.CostUSD == nil || *res.CostUSD != 0.012 {
			t.Fatalf("cost: %v", res.CostUSD)
		}
		if res, _ := ParseExa(obj(`{"results":[]}`)); res.CostUSD != nil || len(res.Results) != 0 {
			t.Fatalf("no cost field: %+v", res)
		}
	})
	t.Run("linkup", func(t *testing.T) {
		res, err := ParseLinkup(obj(`{"results":[{"type":"text","name":"가짜 이름","url":"https://l.example/1","content":"가짜 본문","favicon":"https://l.example/f.ico"},{"type":"image","name":"사진","url":"https://l.example/p.png"}]}`))
		if err != nil || len(res.Results) != 1 || res.Results[0].Title != "가짜 이름" || res.Results[0].Published != "" {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("tinyfish news without a snippet", func(t *testing.T) {
		res, err := ParseTinyFish(obj(`{"query":"q","results":[{"position":1,"site_name":"n.example","title":"가짜 기사","snippet":"","url":"https://n.example/1","date":"Fri, 06 Nov 2026 18:48:00 GMT","publisher":"가짜일보"},{"position":2,"site_name":"w.example","title":"가짜 문서","snippet":"가짜 요약","url":"https://w.example/2"}],"total_results":2,"page":0}`))
		if err != nil || len(res.Results) != 2 || res.Results[0].Snippet != "가짜일보" || res.Results[0].Published != "Fri, 06 Nov 2026 18:48:00 GMT" || res.Results[1].Snippet != "가짜 요약" {
			t.Fatalf("%+v %v", res, err)
		}
	})
	t.Run("tavily", func(t *testing.T) {
		res, err := ParseTavily(obj(`{"query":"q","answer":null,"images":[],"results":[{"title":"가짜","url":"https://t.example/1","content":"가짜 본문","score":0.8,"raw_content":null,"published_date":null,"favicon":"x","id":"a1"}],"response_time":"1.2","usage":{"credits":1},"request_id":"r"}`))
		if err != nil || len(res.Results) != 1 || res.Results[0].Published != "" || res.Answer != "" {
			t.Fatalf("%+v %v", res, err)
		}
	})
}

// Tavily is asked for the dates of general results too (news always has them).
func TestTavilyAsksForPublishedDates(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(raw, &got)
		_, _ = w.Write([]byte(`{"results":[]}`))
	}))
	defer srv.Close()
	c := providers.NewClient(5*time.Second, nil)
	c.MaxRetries = 0
	t.Setenv("K", "k")
	cred, _ := providers.Resolve("env:K")
	s, _ := New("tavily", c, srv.URL, cred)
	if _, err := s.Search(context.Background(), Query{Text: "환율"}); err != nil || got["include_published_date"] != true {
		t.Fatalf("%v %v", got, err)
	}
}

// How many results are asked for: 1–10, five when none is asked, and more than ten is ten.
func TestSizeBounds(t *testing.T) {
	for in, want := range map[int]int{-3: 5, 0: 5, 1: 1, 5: 5, 10: 10, 11: 10, 50: 10} {
		if got := size(in); got != want {
			t.Errorf("size(%d) = %d, want %d", in, got, want)
		}
	}
}

// Brave's context endpoint lists a source's age as [full date, YYYY-MM-DD, relative age, ISO timestamp]
// (the timestamp is new since 2026-07-31); the machine-readable one is taken, and a page with no known
// date has none.
func TestBraveDate(t *testing.T) {
	for _, tc := range []struct {
		age  []string
		want string
	}{
		{[]string{"Monday, January 15, 2024", "2024-01-15", "380 days ago", "2024-01-15T13:45:02Z"}, "2024-01-15T13:45:02Z"},
		{[]string{"Monday, January 15, 2024", "2024-01-15", "380 days ago"}, "2024-01-15"},
		{[]string{}, ""},
		{nil, ""},
		{[]string{"", "", "", ""}, ""},
	} {
		if got := braveDate(tc.age); got != tc.want {
			t.Errorf("braveDate(%q) = %q, want %q", tc.age, got, tc.want)
		}
	}
}
