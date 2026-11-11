package websearch

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestHostCacheCountsOnlyWhatTheTargetCaused(t *testing.T) {
	now := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	h := NewHostCache()
	h.Now = func() time.Time { return now }

	// a platform host: one private post does not close the platform, only that author's part of it
	h.Note("https://m.blog.naver.com/alice/111", "login")
	if why, skip := h.Skip("https://m.blog.naver.com/alice/222"); !skip || why != "blocked" {
		t.Fatalf("the same blog stays closed: %q %v", why, skip)
	}
	if _, skip := h.Skip("https://m.blog.naver.com/bob/333"); skip {
		t.Fatal("another blog of the platform is still asked")
	}
	if _, skip := h.Skip("https://blog.naver.com/alice/111"); skip {
		t.Fatal("blog.naver.com is another host key; only the one that was noted is closed")
	}
	h.Note("https://n.news.naver.com/mnews/article/1", "blocked")
	if _, skip := h.Skip("https://n.news.naver.com/article/2"); skip {
		t.Fatal("another section of the news platform is still asked")
	}
	if _, skip := h.Skip("https://n.news.naver.com/mnews/article/9"); !skip {
		t.Fatal("the section that blocked stays closed")
	}

	// a timeout on a platform host needs three strikes, elsewhere two
	for i := 1; i <= 3; i++ {
		if _, skip := h.Skip("https://velog.io/@x/y"); skip {
			t.Fatalf("after %d timeouts the platform is still asked", i-1)
		}
		h.Note("https://velog.io/@x/"+string(rune('a'+i)), "timeout")
	}
	if why, skip := h.Skip("https://velog.io/@z/q"); !skip || why != "timeout" {
		t.Fatalf("third timeout: %q %v", why, skip)
	}
	h.Note("https://wiki.example.com/a", "timeout")
	h.Note("https://wiki.example.com/b", "timeout")
	if _, skip := h.Skip("https://wiki.example.com/c"); !skip {
		t.Fatal("two timeouts close an ordinary host")
	}

	// a page read resets the host
	h.OK("https://wiki.example.com/d")
	if _, skip := h.Skip("https://wiki.example.com/c"); skip {
		t.Fatal("a success resets the host")
	}
	h.Note("https://wiki.example.com/a", "timeout")
	h.OK("https://wiki.example.com/x")
	h.Note("https://wiki.example.com/a", "timeout")
	if _, skip := h.Skip("https://wiki.example.com/c"); skip {
		t.Fatal("strikes before a success do not count after it")
	}
	h.OK("https://m.blog.naver.com/alice/444")
	if _, skip := h.Skip("https://m.blog.naver.com/alice/222"); skip {
		t.Fatal("a post read opens the blog again")
	}

	// expired entries are pruned by the next note
	now = now.Add(2 * time.Hour)
	h.Note("https://fresh.example.com/", "blocked")
	if n := h.Len(); n != 1 {
		t.Fatalf("expired entries stay: %d", n)
	}
}

// hungOrErrFetch serves TinyFish Fetch: a request whose address contains "late" answers a 500 at the
// moment the caller's deadline passes, the others answer a page at once.
func lateErrorServer(t *testing.T, lateAfter time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		if strings.Contains(string(raw), "late") {
			time.Sleep(lateAfter)
			w.WriteHeader(500)
			return
		}
		_, _ = w.Write([]byte(`{"results":[{"url":"u","title":"t","text":"본문"}]}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// A service error that lands around the deadline races nothing: each worker's error travels with its
// own result (run with -race).
func TestFetchersServiceErrorAroundTheDeadline(t *testing.T) {
	for _, kind := range []string{"tinyfish", "jina"} {
		t.Run(kind, func(t *testing.T) {
			srv := lateErrorServer(t, 60*time.Millisecond)
			c, cred := fetchClient(t)
			var f Fetcher
			if kind == "tinyfish" {
				f = NewTinyFishFetcher(c, srv.URL, cred)
			} else {
				f = NewJinaFetcher(NewReader(c, srv.URL, cred))
			}
			for i := 0; i < 12; i++ {
				ctx, cancel := context.WithTimeout(context.Background(), time.Duration(40+i*4)*time.Millisecond)
				urls := []string{"https://late.example.com/a", "https://late.example.com/b", "https://ok.example.com/c"}
				got, err := f.Fetch(ctx, urls, 100)
				cancel()
				if len(got) != 3 {
					t.Fatal("results lost")
				}
				for j, g := range got {
					if g.Target {
						t.Fatalf("round %d page %d: an error of ours or the service's is not the target's: %+v", i, j, g)
					}
				}
				_ = err // whether the late error counts depends on the moment; reading it must not race
			}
		})
	}
}

// A worker that panics is that page's failure; the others are read.
func TestGatherRecoversAWorkerPanic(t *testing.T) {
	out, errs := gather(context.Background(), 3, func(ctx context.Context, i int) (Fetched, error) {
		if i == 1 {
			panic("parser")
		}
		return Fetched{URL: "u", Content: "c"}, nil
	})
	if out[0].Err != "" || out[1].Err != "error" || out[2].Err != "" || errs[1] != nil || out[1].Target {
		t.Fatalf("%+v %v", out, errs)
	}
}

// A page the fetch service reports as timed out, blocked or login-walled is the target's failure; our
// cut-off, a rate limit of the host (429) and the service's own failures are not.
func TestFetchTargetFlag(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		hits.Add(1)
		u := string(raw)
		switch {
		case strings.Contains(u, "slowhost"):
			_, _ = w.Write([]byte(`{"results":[],"errors":[{"error":"timeout"}]}`))
		case strings.Contains(u, "walled"):
			_, _ = w.Write([]byte(`{"results":[],"errors":[{"error":"login_required"}]}`))
		case strings.Contains(u, "limited"):
			_, _ = w.Write([]byte(`{"results":[],"errors":[{"error":"target_http_error","status":429}]}`))
		case strings.Contains(u, "forbidden"):
			_, _ = w.Write([]byte(`{"results":[],"errors":[{"error":"target_http_error","status":403}]}`))
		case strings.Contains(u, "big"):
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"results":[{"url":"u","text":"` + strings.Repeat("x", 5000) + `"}]}`))
		default:
			_, _ = w.Write([]byte(`{"results":[{"url":"u","text":"본문"}]}`))
		}
	}))
	defer srv.Close()
	c, cred := fetchClient(t)
	c.MaxBody = 1000
	f := NewTinyFishFetcher(c, srv.URL, cred)
	got, err := f.Fetch(context.Background(), []string{"https://slowhost.example.com/", "https://walled.example.com/", "https://limited.example.com/",
		"https://forbidden.example.com/", "https://big.example.com/", "https://fine.example.com/"}, 100)
	if err != nil {
		t.Fatalf("a page's failure is not the service's: %v", err)
	}
	want := []struct {
		err    string
		target bool
	}{{"timeout", true}, {"login", true}, {"error", false}, {"blocked", true}, {"error", false}, {"", false}}
	for i, w := range want {
		if got[i].Err != w.err || got[i].Target != w.target {
			t.Errorf("page %d: %q target %v, want %q %v", i, got[i].Err, got[i].Target, w.err, w.target)
		}
	}
	// the page that was too big is its own failure: with nothing else wrong, the service is not blamed
	got, err = f.Fetch(context.Background(), []string{"https://big.example.com/"}, 100)
	if err != nil || got[0].Err != "error" {
		t.Fatalf("too large: %v %+v", err, got)
	}
	// our own cut-off is not the target's
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	got, _ = f.Fetch(ctx, []string{"https://fine.example.com/"}, 100)
	if got[0].Err != "timeout" || got[0].Target {
		t.Fatalf("cancelled: %+v", got[0])
	}
}

func TestFetchTargetNaverQueryForms(t *testing.T) {
	for in, want := range map[string]string{
		"https://blog.naver.com/PostView.naver?blogId=someone&logNo=223456789&redirect=Dlog": "https://m.blog.naver.com/someone/223456789",
		"https://blog.naver.com/PostView.nhn?blogId=someone&logNo=223456789":                 "https://m.blog.naver.com/someone/223456789",
		"https://blog.naver.com/someone?Redirect=Log&logNo=223456789":                        "https://m.blog.naver.com/someone/223456789",
		"https://blog.naver.com/PostView.naver?blogId=someone":                               "https://blog.naver.com/PostView.naver?blogId=someone",
		"https://blog.naver.com/PostView.naver?blogId=a/b&logNo=1":                           "https://blog.naver.com/PostView.naver?blogId=a/b&logNo=1",
		"https://blog.naver.com/someone?Redirect=Log&logNo=notnumber":                        "https://blog.naver.com/someone?Redirect=Log&logNo=notnumber",
		"https://blog.naver.com/someone/223456789":                                           "https://m.blog.naver.com/someone/223456789",
		"https://blog.naver.com/someone?logNo=223456789":                                     "https://blog.naver.com/someone?logNo=223456789",
	} {
		got, ok := FetchTarget(in)
		if got != want || !ok {
			t.Errorf("%s -> %q %v, want %q", in, got, ok, want)
		}
	}
}

// Exa asks for a crawl from today only for a search that reads pages and is not quick.
func TestExaMaxAgeOnlyForPageSearches(t *testing.T) {
	has := func(q Query) bool {
		got, _ := serve(t, "exa", `{"results":[]}`, q)
		_, ok := got.body["contents"].(map[string]any)["maxAgeHours"]
		return ok
	}
	if !has(Query{Text: "x", Fresh: "day", Full: true}) || !has(Query{Text: "x", Fresh: "day", Full: true, Depth: DepthDeep}) {
		t.Error("a standard or deep search that reads pages asks for today's crawl")
	}
	if has(Query{Text: "x", Fresh: "day"}) || has(Query{Text: "x", Fresh: "day", Full: true, Depth: DepthQuick}) || has(Query{Text: "x", Fresh: "week", Full: true}) {
		t.Error("no pages, quick or not a day: no forced crawl")
	}
	got, _ := serve(t, "exa", `{"results":[]}`, Query{Text: "x", Depth: DepthDeep})
	if got.body["type"] != "deep-lite" {
		t.Errorf("deep is deep-lite: %v", got.body["type"])
	}
	got, _ = serve(t, "exa", `{"results":[]}`, Query{Text: "x", Depth: DepthQuick})
	if got.body["type"] != "instant" {
		t.Errorf("quick stays instant: %v", got.body["type"])
	}
}
