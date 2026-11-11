package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

// termServer answers every request of a service kind with results named after the query it was sent
// (two a query, one for Kakao) and records the terms and paths, in arrival order.
type termServer struct {
	mu    sync.Mutex
	terms []string
	paths []string
	srv   *httptest.Server
}

func newTermServer(t *testing.T, kind string) *termServer {
	t.Helper()
	ts := &termServer{}
	ts.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		var term string
		switch kind {
		case "tavily":
			term, _ = body["query"].(string)
		case "serper":
			term, _ = body["q"].(string)
		case "brave":
			term = r.URL.Query().Get("q")
		default: // tinyfish, kakao_search
			term = r.URL.Query().Get("query")
		}
		ts.mu.Lock()
		ts.terms = append(ts.terms, term)
		ts.paths = append(ts.paths, r.URL.Path)
		ts.mu.Unlock()
		slug := strings.ReplaceAll(term, " ", "-")
		u := func(n int) string {
			return fmt.Sprintf("https://%s.example/%s/%d", map[bool]string{true: "blog", false: "web"}[strings.HasSuffix(r.URL.Path, "/blog")], slug, n)
		}
		w.Header().Set("Content-Type", "application/json")
		switch kind {
		case "tavily":
			fmt.Fprintf(w, `{"results":[{"title":"t1","url":%q,"content":"c"},{"title":"t2","url":%q,"content":"c"}]}`, u(1), u(2))
		case "serper":
			fmt.Fprintf(w, `{"organic":[{"title":"t1","link":%q,"snippet":"s"},{"title":"t2","link":%q,"snippet":"s"}]}`, u(1), u(2))
		case "brave":
			if r.URL.Path == "/res/v1/web/search" {
				fmt.Fprintf(w, `{"web":{"results":[{"title":"t1","url":%q,"description":"d"},{"title":"t2","url":%q,"description":"d"}]}}`, u(1), u(2))
				return
			}
			fmt.Fprintf(w, `{"grounding":{"generic":[{"url":%q,"title":"t1","snippets":["s"]},{"url":%q,"title":"t2","snippets":["s"]}]}}`, u(1), u(2))
		case "tinyfish":
			fmt.Fprintf(w, `{"results":[{"title":"t1","url":%q,"snippet":"s"},{"title":"t2","url":%q,"snippet":"s"}]}`, u(1), u(2))
		default:
			fmt.Fprintf(w, `{"documents":[{"title":"t1","contents":"c","url":%q,"datetime":""}],"meta":{}}`, u(1))
		}
	}))
	t.Cleanup(ts.srv.Close)
	return ts
}

func (ts *termServer) seen() []string {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	out := slices.Clone(ts.terms)
	slices.Sort(out)
	return out
}

func newSearcher(t *testing.T, kind, base string) Searcher {
	t.Helper()
	c := providers.NewClient(5*time.Second, nil)
	c.MaxRetries = 0
	t.Setenv("K", "k")
	cred, _ := providers.Resolve("env:K")
	s, err := New(kind, c, base, cred)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func urlsOf(res Response) []string {
	var out []string
	for _, r := range res.Results {
		out = append(out, strings.TrimPrefix(r.URL, "https://"))
	}
	return out
}

// Every service that takes no reworded queries of its own runs up to MaxExtra of them as requests of
// its own beside the main one, merged with the main query's results first and cut to the size asked.
func TestDeepSearchRunsRewordedQueriesAtEveryOtherService(t *testing.T) {
	for _, kind := range []string{"tavily", "serper", "brave", "tinyfish", "kakao_search"} {
		t.Run(kind, func(t *testing.T) {
			ts := newTermServer(t, kind)
			s := newSearcher(t, kind, ts.srv.URL)
			q := Query{Text: "main", Depth: DepthDeep, Max: 4, Extra: []string{" a ", "b", "c", "main", "A"}}
			res, err := s.Search(context.Background(), q)
			if err != nil {
				t.Fatal(err)
			}
			// the third reworded query is over MaxExtra; the main query and a repeat are not asked again
			want := []string{"a", "b", "main"}
			if kind == "kakao_search" {
				want = []string{"a", "b", "main", "main"} // the main query asks web and blog
			}
			if got := ts.seen(); !slices.Equal(got, want) {
				t.Fatalf("terms sent: %v, want %v", got, want)
			}
			wantReqs := 3
			if kind == "kakao_search" {
				wantReqs = 4
			}
			if res.Requests != wantReqs || Requests(kind, DepthDeep, 3) != wantReqs {
				t.Fatalf("requests %d, Requests() %d, want %d", res.Requests, Requests(kind, DepthDeep, 3), wantReqs)
			}
			if len(res.Results) != 4 {
				t.Fatalf("cut to the size asked: %v", urlsOf(res))
			}
			got := urlsOf(res)
			if kind == "kakao_search" {
				// main web, a, b (web only), then the main query's blog result
				if got[0] != "web.example/main/1" || got[1] != "web.example/a/1" || got[2] != "web.example/b/1" || got[3] != "blog.example/main/1" {
					t.Fatalf("order: %v", got)
				}
				return
			}
			host := "web.example/"
			if got[0] != host+"main/1" || got[1] != host+"a/1" || got[2] != host+"b/1" || got[3] != host+"main/2" {
				t.Fatalf("the main query first, then the reworded ones by rank: %v", got)
			}
		})
	}
}

// Reworded queries are for deep searches only; fewer requests reserved means fewer sent, the main query first.
func TestRewordedQueriesFollowDepthAndReservedRequests(t *testing.T) {
	for _, kind := range []string{"tavily", "serper", "brave", "tinyfish", "kakao_search"} {
		t.Run(kind, func(t *testing.T) {
			extra := []string{"a", "b"}
			cases := []struct {
				name string
				q    Query
				want []string
			}{
				{"standard", Query{Text: "main", Extra: extra}, []string{"main"}},
				{"quick", Query{Text: "main", Depth: DepthQuick, Extra: extra}, []string{"main"}},
				{"one request", Query{Text: "main", Depth: DepthDeep, Extra: extra, Requests: 1}, []string{"main"}},
				{"two requests", Query{Text: "main", Depth: DepthDeep, Extra: extra, Requests: 2}, []string{"a", "main"}},
				{"no reworded", Query{Text: "main", Depth: DepthDeep}, []string{"main"}},
			}
			if kind == "kakao_search" {
				// the main query's blog search is a request of its own: 1 reserved is the web alone,
				// 2 the web and the blog, 3 one reworded query as well
				cases[2].want = []string{"main"}
				cases[3].want = []string{"main", "main"}
				cases[4].want = []string{"main", "main"}
				cases = append(cases, struct {
					name string
					q    Query
					want []string
				}{"three requests", Query{Text: "main", Depth: DepthDeep, Extra: extra, Requests: 3}, []string{"a", "main", "main"}})
			}
			for _, c := range cases {
				ts := newTermServer(t, kind)
				s := newSearcher(t, kind, ts.srv.URL)
				if _, err := s.Search(context.Background(), c.q); err != nil {
					t.Fatalf("%s: %v", c.name, err)
				}
				if got := ts.seen(); !slices.Equal(got, c.want) {
					t.Errorf("%s: terms %v, want %v", c.name, got, c.want)
				}
			}
		})
	}
}

// Exa takes the reworded queries in its own request and Linkup plans its own: one request each.
func TestExaAndLinkupSendOneRequestForReworded(t *testing.T) {
	got, res := serve(t, "exa", `{"results":[]}`, Query{Text: "main", Depth: DepthDeep, Extra: []string{"a", "b", "c", "main"}})
	extra, _ := got.body["additionalQueries"].([]any)
	if len(extra) != 3 || res.Requests > 1 {
		t.Fatalf("exa: additionalQueries %v, requests %d", got.body["additionalQueries"], res.Requests)
	}
	got, res = serve(t, "linkup", `{"results":[]}`, Query{Text: "main", Depth: DepthDeep, Extra: []string{"a"}})
	if got.body["q"] != "main" || got.body["depth"] != "deep" || res.Requests > 1 {
		t.Fatalf("linkup: %v", got.body)
	}
	if Requests("exa", DepthDeep, 3) != 1 || Requests("linkup", DepthDeep, 3) != 1 || Requests("tinyfish", DepthStandard, 3) != 1 {
		t.Fatal("request counts")
	}
}

// A reworded query that fails is dropped; the main query failing fails the search but still says how many
// requests were answered (they may have been charged).
func TestRewordedQueryFailureAndPanic(t *testing.T) {
	calls := 0
	var mu sync.Mutex
	one := func(failOn string, panicOn string) searchFn {
		return func(ctx context.Context, q Query) (Response, error) {
			mu.Lock()
			calls++
			mu.Unlock()
			switch q.Text {
			case failOn:
				return Response{}, fmt.Errorf("boom")
			case panicOn:
				panic("parser met something new")
			}
			return Response{Results: []Result{{URL: "https://" + q.Text + ".example/1"}}}, nil
		}
	}
	q := Query{Text: "main", Depth: DepthDeep, Extra: []string{"a", "b"}, Max: 5}

	res, err := fanOut(context.Background(), q, one("a", "b"))
	if err != nil || len(res.Results) != 1 || res.Requests != 1 {
		t.Fatalf("reworded failing or panicking: only the main answer remains: %v %+v", err, res)
	}
	// the main query failing: the reworded queries' results (paid for) answer the search
	res, err = fanOut(context.Background(), q, one("main", ""))
	if err != nil || res.Requests != 2 || len(res.Results) != 2 {
		t.Fatalf("main failing, reworded found pages: %v %+v", err, res)
	}
	res, err = fanOut(context.Background(), q, one("", "main"))
	if err != nil || res.Requests != 2 || len(res.Results) != 2 {
		t.Fatalf("a panic in the main query, reworded found pages: %v %+v", err, res)
	}
	// nothing found anywhere: the main query's error, with the requests answered for the fee
	none := func(ctx context.Context, q Query) (Response, error) {
		if q.Text == "main" {
			return Response{}, fmt.Errorf("boom")
		}
		return Response{}, nil
	}
	res, err = fanOut(context.Background(), q, none)
	if err == nil || res.Requests != 2 {
		t.Fatalf("main failing and reworded empty: %v %+v", err, res)
	}
	res, err = fanOut(context.Background(), Query{Text: "main"}, one("", "main"))
	if err == nil {
		t.Fatalf("a panic without reworded queries is an error too: %+v", res)
	}
}

// A request whose answer was lost (timed out after it was sent) or could not be read may have been
// charged: the fan-out counts it in Lost; one never sent, an HTTP error and our own limits are not counted.
func TestRewordedLostAnswersAreCounted(t *testing.T) {
	lostErr := &providers.Error{Kind: "timeout"}
	unread := fmt.Errorf("%w: test", decode.ErrUnexpected)
	notSent := &providers.Error{Kind: "timeout", NotSent: true}
	status := &providers.Error{Kind: "status", Status: 502}
	errs := map[string]error{}
	fn := func(ctx context.Context, q Query) (Response, error) {
		if err := errs[q.Text]; err != nil {
			return Response{}, err
		}
		return Response{Results: []Result{{URL: "https://" + q.Text + ".example/1"}}}, nil
	}
	q := Query{Text: "main", Depth: DepthDeep, Extra: []string{"a", "b"}, Max: 5}

	errs = map[string]error{"a": lostErr, "b": unread}
	res, err := fanOut(context.Background(), q, fn)
	if err != nil || res.Requests != 1 || res.Lost != 2 {
		t.Fatalf("two reworded answers lost: %v %+v", err, res)
	}
	errs = map[string]error{"a": notSent, "b": status}
	res, err = fanOut(context.Background(), q, fn)
	if err != nil || res.Requests != 1 || res.Lost != 0 {
		t.Fatalf("never sent and an HTTP error are not charged: %v %+v", err, res)
	}
	// the main answer lost while a reworded one answered: the search succeeds and the main request counts
	errs = map[string]error{"main": lostErr, "b": lostErr}
	res, err = fanOut(context.Background(), q, fn)
	if err != nil || res.Requests != 1 || res.Lost != 2 {
		t.Fatalf("main lost, one reworded answered: %v %+v", err, res)
	}
	// everything failed: the main error is the search's (charged by the caller), the reworded ones are Lost
	errs = map[string]error{"main": lostErr, "a": lostErr, "b": status}
	res, err = fanOut(context.Background(), q, fn)
	if err == nil || res.Requests != 0 || res.Lost != 1 {
		t.Fatalf("all failed: %v %+v", err, res)
	}
}

func TestMaybeCharged(t *testing.T) {
	for _, c := range []struct {
		err  error
		want bool
	}{
		{nil, false},
		{fmt.Errorf("a limit of ours"), false},
		{&providers.Error{Kind: "timeout"}, true},
		{&providers.Error{Kind: "connection"}, true},
		{&providers.Error{Kind: "timeout", NotSent: true}, false},
		{&providers.Error{Kind: "status", Status: 500}, false},
		{&providers.Error{Kind: "too_large"}, true},
		{&providers.Error{Kind: "cancelled"}, true},
		{&providers.Error{Kind: "cancelled", NotSent: true}, false},
		{fmt.Errorf("%w: linkup results", decode.ErrUnexpected), true},
	} {
		if got := MaybeCharged(c.err); got != c.want {
			t.Errorf("MaybeCharged(%v) = %v, want %v", c.err, got, c.want)
		}
	}
}

// Kakao's blog search runs in a goroutine of its own: a panic there is that request's error.
func TestKakaoBlogPanicIsAnError(t *testing.T) {
	d := &daum{}
	res, err := d.deepMain(context.Background(), Query{Text: "x", Depth: DepthDeep}, true) // no adapter: both requests fail
	if err == nil {
		t.Fatalf("expected an error, got %+v", res)
	}
}
