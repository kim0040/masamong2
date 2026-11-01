package notices

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"golang.org/x/text/encoding/korean"
	"golang.org/x/text/transform"
)

func TestBuiltInCatalogLoads(t *testing.T) {
	c, origin, err := Load("")
	if err != nil || origin != "built-in" {
		t.Fatal(err, origin)
	}
	school, transfer, manual := 0, 0, 0
	for _, s := range c.Sources {
		switch {
		case s.Manual:
			manual++
		case s.Kind == KindSchool:
			school++
		default:
			transfer++
		}
	}
	if school != 17 || transfer != 20 || manual != 2 || len(c.Schools) == 0 {
		t.Fatalf("school %d transfer %d manual %d", school, transfer, manual)
	}
	if _, err := Parse([]byte(`{"schema_version":1,"schools":[],"sources":[{"id":"x","kind":"school","school":"nope"}]}`)); err == nil {
		t.Fatal("a source of an unknown school is refused")
	}
	if _, err := Parse([]byte(`{"schema_version":1,"schools":[{"id":"a","name":"A"}],"sources":[{"id":"x","kind":"school","school":"a","manual":true,"official_url":"javascript:alert(1)"}]}`)); err == nil {
		t.Fatal("a link must be a web address")
	}
	if _, err := Parse([]byte(`{"schema_version":1,"schools":[{"id":"a","name":"A"}],"sources":[{"id":"x","kind":"school","school":"a","list_url":"https://evil.example/","allowed_hosts":["a.ac.kr"],"list":{"link_selector":"a"}}]}`)); err == nil {
		t.Fatal("a list URL outside the allowed hosts is refused")
	}
}

const rowsHTML = `<table><tbody>
<tr class="notice"><td class="t"><a href="/bbs/1/1001/artclView.do"><strong>[학사] 2026학년도 2학기 수강신청 안내</strong></a></td><td class="d">2026.09.20</td></tr>
<tr><td class="t"><a href="/bbs/1/1002/artclView.do"><strong>장학금 신청 안내 (학부생)</strong></a></td><td class="d">2026-09-25</td></tr>
<tr><td class="t"><a href="/bbs/1/1002/artclView.do"><strong>중복</strong></a></td><td class="d">2026-09-25</td></tr>
<tr><td class="t"><a href="javascript:void(0)">깨진 줄</a></td></tr>
</tbody></table>`

func schoolSource() Source {
	return Source{ID: "s1", Kind: KindSchool, School: "a", University: "가대학교", ListURL: "https://www.a.ac.kr/bbs/list.do", AllowedHosts: []string{"www.a.ac.kr"},
		List:   ListSpec{RowSelector: "tbody tr", LinkSelector: "td.t a", IDAttribute: "href", IDRegex: `/(\d+)/artclView`, TitleSelector: "strong", DateSelector: "td.d", PinnedClasses: []string{"notice"}},
		Detail: DetailSpec{BodySelectors: []string{".body"}}}
}

func catalogWith(t *testing.T, srcs ...Source) *Catalog {
	t.Helper()
	c, _, err := Load("")
	if err != nil {
		t.Fatal(err)
	}
	c.Sources = srcs
	return c
}

func TestParseRows(t *testing.T) {
	src := schoolSource()
	c := catalogWith(t, src)
	items, warnings := c.ParseList(rowsHTML, src)
	if len(items) != 2 {
		t.Fatalf("items %+v warnings %v", items, warnings)
	}
	if items[0].ExternalID != "1001" || !items[0].Pinned || items[0].Published != "2026-09-20" || items[0].URL != "https://www.a.ac.kr/bbs/1/1001/artclView.do" {
		t.Fatalf("first: %+v", items[0])
	}
	if items[1].Title != "장학금 신청 안내 (학부생)" || items[1].Published != "2026-09-25" {
		t.Fatalf("second: %+v", items[1])
	}
	if _, w := c.ParseList("<html></html>", src); len(w) != 1 || w[0] != "row_selector_found_nothing" {
		t.Fatalf("a changed site is reported: %v", w)
	}
}

func TestParseTransferLinks(t *testing.T) {
	src := Source{ID: "t1", Kind: KindTransfer, University: "나대학교", ListURL: "https://ipsi.b.ac.kr/notice/list.do?page=2", AllowedHosts: []string{"ipsi.b.ac.kr"},
		List: ListSpec{LinkSelector: "a[href]", HrefRegex: `/notice/view\.do`}, MixedBoard: true}
	c := catalogWith(t, src)
	html := `<ul>
	<li><a href="/notice/view.do?id=7&page=3">2027학년도 편입학 모집요강 안내 작성일 2026.09.01</a> <span>2026-09-01</span></li>
	<li><a href="/notice/view.do?id=8">2027학년도 대학원 편입학 모집 안내</a></li>
	<li><a href="/notice/view.do?id=9">2027학년도 수시모집 원서접수 안내</a></li>
	<li><a href="https://evil.example/notice/view.do?id=10">2027학년도 편입학 합격자 발표</a></li>
	<li><a href="/other">편입학 안내입니다만 다른 곳</a></li>
	</ul>`
	items, _ := c.ParseList(html, src)
	if len(items) != 1 {
		t.Fatalf("only the transfer notice on the allowed host: %+v", items)
	}
	it := items[0]
	if it.Title != "2027학년도 편입학 모집요강 안내" || strings.Contains(it.URL, "page=") || it.Published != "2026-09-01" {
		t.Fatalf("%+v", it)
	}
}

func TestDecodeEUCKR(t *testing.T) {
	enc, _, _ := transform.Bytes(korean.EUCKR.NewEncoder(), []byte("<html><title>공지사항</title></html>"))
	if got := decode(enc, "text/html; charset=euc-kr"); !strings.Contains(got, "공지사항") {
		t.Fatalf("declared: %q", got)
	}
	if got := decode(enc, "text/html"); !strings.Contains(got, "공지사항") {
		t.Fatalf("undeclared: %q", got)
	}
	meta, _, _ := transform.Bytes(korean.EUCKR.NewEncoder(), []byte(`<html><head><meta charset="euc-kr"></head><body>편입</body></html>`))
	if got := decode(meta, ""); !strings.Contains(got, "편입") {
		t.Fatalf("meta: %q", got)
	}
}

func TestRobots(t *testing.T) {
	r := parseRobots("User-agent: *\nDisallow: /admin\nDisallow: /*.pdf$\nAllow: /admin/public\n\nUser-agent: masamong\nDisallow: /private\n", "masamong")
	if r.allows("/private/x") || !r.allows("/admin") {
		t.Fatal("our own group applies, not *")
	}
	r = parseRobots("User-agent: *\nDisallow: /admin\nDisallow: /*.pdf$\nAllow: /admin/public\n", "masamong")
	for path, want := range map[string]bool{"/": true, "/admin/x": false, "/admin/public/x": true, "/files/a.pdf": false, "/files/a.pdf?x": true} {
		if r.allows(path) != want {
			t.Fatalf("%s: want %v", path, want)
		}
	}
	if !parseRobots("", "masamong").allows("/anything") {
		t.Fatal("an empty robots.txt allows")
	}
}

func TestFetcherFollowsTheRules(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/robots.txt":
			_, _ = w.Write([]byte("User-agent: *\nDisallow: /secret\n"))
		case "/away":
			// same server, another host name: not one of the source's hosts
			http.Redirect(w, r, strings.Replace("http://"+r.Host, "127.0.0.1", "localhost", 1)+"/page", http.StatusFound)
		case "/big":
			_, _ = w.Write([]byte(strings.Repeat("x", 5000)))
		default:
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte("<p>안녕</p>"))
		}
	}))
	defer srv.Close()
	u, _ := url.Parse(srv.URL)
	src := Source{ID: "x", AllowedHosts: []string{u.Hostname()}}
	f := &Fetcher{AllowPrivate: true, MaxBytes: 1000, MaxRequests: 5, HostInterval: 10 * time.Millisecond}
	ctx := context.Background()
	if body, err := f.Get(ctx, src, srv.URL+"/page"); err != nil || !strings.Contains(body, "안녕") {
		t.Fatalf("%q %v", body, err)
	}
	if _, err := f.Get(ctx, src, srv.URL+"/secret/x"); !errors.Is(err, ErrRobots) {
		t.Fatalf("robots: %v", err)
	}
	if _, err := f.Get(ctx, src, srv.URL+"/big"); err == nil {
		t.Fatal("size cap")
	}
	if _, err := f.Get(ctx, Source{ID: "y", AllowedHosts: []string{"other.example"}}, srv.URL+"/page"); err == nil {
		t.Fatal("hosts outside the source are refused")
	}
	for i := 0; i < 5; i++ {
		_, _ = f.Get(ctx, src, srv.URL+"/page")
	}
	if _, err := f.Get(ctx, src, srv.URL+"/page"); !errors.Is(err, ErrBudget) {
		t.Fatalf("budget: %v", err)
	}
	if _, err := (&Fetcher{AllowPrivate: true}).Get(ctx, src, srv.URL+"/away"); err == nil || !strings.Contains(err.Error(), "not one of the source's hosts") {
		t.Fatalf("a redirect must stay on the source's hosts: %v", err)
	}
	// without the test switch a private address is refused
	strict := &Fetcher{}
	t.Setenv("MASAMONG_OFFLINE", "")
	if _, err := strict.Get(ctx, src, srv.URL+"/page"); err == nil {
		t.Fatal("a loopback address must be refused")
	}
}

func TestSignalsAndScore(t *testing.T) {
	c, _, _ := Load("")
	today := time.Date(2026, 9, 20, 0, 0, 0, 0, time.UTC)
	src := schoolSource()
	body := "학부 재학생 대상 국가장학금 2차 신청 안내\n신청 기간: 2026.09.22 ~ 2026.09.30"
	sig := c.Signals("국가장학금 2차 신청", body)
	if !contains(sig.Audiences, "undergraduate") || !contains(sig.Topics, "scholarship") || !contains(sig.Actions, "apply") || len(sig.Dates) != 2 {
		t.Fatalf("signals %+v", sig)
	}
	p := Profile{School: "a", Degree: "undergraduate", Grade: 2, Interests: []string{"scholarship"}}
	sc := c.Score(p, src, Item{Title: "국가장학금 2차 신청"}, sig, body, today)
	if sc.Band != "action" || sc.Deadline != "2026-09-22" || sc.Score < 60 {
		t.Fatalf("score %+v", sc)
	}
	grad := Profile{School: "a", Degree: "master"}
	if g := c.Score(grad, src, Item{Title: "x"}, sig, body, today); g.Score >= sc.Score {
		t.Fatalf("a graduate notice scores lower for undergraduates and vice versa: %+v", g)
	}
	muted := p
	muted.Muted = []string{"scholarship"}
	if m := c.Score(muted, src, Item{}, sig, body, today); m.Band != "hidden" {
		t.Fatalf("muted: %+v", m)
	}
	dep := src
	dep.Tags = map[string][]string{"department": {"소프트웨어공학과"}}
	other := p
	other.Department = "경영학과"
	if d := c.Score(other, dep, Item{}, sig, body, today); d.Band != "hidden" {
		t.Fatalf("another department's board: %+v", d)
	}
	if id, ok := c.TopicID("장학"); !ok || id != "scholarship" {
		t.Fatal("topic words")
	}
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}
