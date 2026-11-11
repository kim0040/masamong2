// Package websearch puts the web search services behind one shape, so search.web can use whichever
// the operator has a key for and say which one answered:
//
//	tavily   POST api.tavily.com/search          Bearer     results[].title/url/content/published_date, answer
//	linkup   POST api.linkup.so/v1/search        Bearer     results[].name/url/content (searchResults)
//	exa      POST api.exa.ai/search              x-api-key  results[].title/url/publishedDate/highlights, costDollars
//	serper   POST google.serper.dev/search       X-API-KEY  organic[].title/link/snippet/date, answerBox
//	brave    GET  api.search.brave.com/res/v1/web/search  X-Subscription-Token  web.results[].title/url/description/page_age
//	tinyfish GET  api.search.tinyfish.ai          X-API-Key  results[].title/url/snippet/site_name/date
//	kakao    GET  dapi.kakao.com/v2/search/web   KakaoAK    documents[].title/contents/url/datetime (Daum)
//
// The first three are built for language models (Tavily, Linkup, Exa return page text chosen for the
// query; Serper is Google's result page). Brave is its own independent index ($5 per 1,000, $5 of
// free credit a month); TinyFish is free up to 12,000 searches a day (30 requests a minute). Kakao is
// Daum's index: strongest for Korean pages, snippets only. Formats were read from each service's API
// reference on 2026-09-30, 2026-10-15 (the depths and the news searches were added) and again on
// 2026-11-07, when each was also called live (Exa, TinyFish, Linkup and Kakao; the other three have
// no key here, so their formats are the references' alone):
//
//	depth    quick                 standard (default)     deep
//	tavily   search_depth fast     basic, 3 chunks        advanced (2 credits), 3 chunks, detailed answer
//	linkup   depth flash           standard               deep (7–41 s, $0.05; several passes)
//	exa      type instant          auto                   deep-lite (~4 s, $0.012) + additionalQueries
//	serper   one Google page whatever the depth
//	brave    web search        web + extra_snippets   GET /res/v1/llm/context (text chunks taken from the pages)
//	tinyfish one page whatever the depth
//	kakao    one Daum page             one Daum page          Daum web + Daum blogs for the main query
//
//	deep also runs up to MaxExtra reworded queries (Query.Extra) beside the main one: Exa takes them in
//	its request, Linkup plans its own, every other service gets a request of its own for each (fanOut);
//	Requests says how many requests that is, the quota and the fee are reserved by it.
//
//	news: tavily topic=news, exa category=news, serper POST /news (news[]), brave GET /res/v1/news/search,
//	tinyfish domain_type=news (a headline, an outlet and a date; the snippet is mostly empty). Linkup and
//	Kakao have no news search: search.web asks them after the others for news (NewsSearch). Kakao's
//	newest-first order was tried and dropped (2026-10-15): for "국제 정세" it ranked unrelated new posts first.
//
//	research: exa category="research paper", tinyfish domain_type=research_paper (pub_year_min for
//	freshness), serper POST /scholar (organic[] with publicationInfo and year), tavily include_domains of
//	scholarly sites with include_domains_mode prefer; linkup, brave and kakao have no search of papers and
//	are asked for papers in the query's words (researchWords). Read from the references 2026-11-12.
//
//	dates: Exa, Tavily (include_published_date), Serper, Brave and Kakao date their results, TinyFish does
//	for news and some pages (RFC 1123 or ISO), Linkup never does (its results have no date field).
//
//	not used, though the references list them: Exa types lite (instant's replacement; its parameter list
//	is shorter and it was not measured here), deep and deep-reasoning, userLocation, Tavily
//	search_depth ultra-fast (one summary per page) and topic finance, TinyFish include/exclude_domains,
//	after/before_date and page, Kakao sort=recency and page, Brave Api-Version pinning.
package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
	"masamong/internal/providers/kakao"
)

// Kinds are the config provider kinds of search services, in the order search.web tries them when
// config does not say (the model-oriented services first, then Google, then Daum).
var Kinds = []string{"tavily", "linkup", "exa", "serper", "brave", "tinyfish", "kakao_search"}

// NewsSearch reports whether a kind has a search of its own for news articles (Query.Topic news).
func NewsSearch(kind string) bool {
	switch kind {
	case "tavily", "exa", "serper", "brave", "tinyfish":
		return true
	}
	return false
}

// DefaultBaseURL is each kind's API address.
var DefaultBaseURL = map[string]string{
	"tavily":       "https://api.tavily.com",
	"linkup":       "https://api.linkup.so",
	"exa":          "https://api.exa.ai",
	"serper":       "https://google.serper.dev",
	"brave":        "https://api.search.brave.com",
	"tinyfish":     "https://api.search.tinyfish.ai",
	"kakao_search": kakao.DefaultBaseURL,
}

// Query is one search. Fresh narrows to recent pages where the service can ("day", "week", "month").
// Depth trades speed for coverage (DepthQuick, "" = standard, DepthDeep); Topic "news" asks for news
// articles where the service has a news search.
type Query struct {
	Text  string
	Max   int
	Fresh string
	Depth string
	Topic string
	// Full asks a service that can for the page text of each result (Exa contents.text, which costs
	// nothing more on /search) for the passages a chat search builds. Unset, nothing extra is asked.
	Full bool
	// Extra holds up to three reworded queries a deep search runs beside Text: Exa takes them in its
	// own request (additionalQueries), Linkup's deep search plans its own, and every other service runs
	// up to MaxExtra of them as requests of its own beside the main one (RewordedQueries, fanOut).
	Extra []string
	// Requests is how many requests the caller reserved (the daily quota) for this search, the main
	// query's first; 0 means as many as Requests(kind, depth, extra) says. A service sends fewer when
	// it was given fewer, the main query before its reworded ones.
	Requests int
}

// Depths and topics search.web accepts.
const (
	DepthQuick    = "quick"
	DepthStandard = "standard"
	DepthDeep     = "deep"
	TopicNews     = "news"
	// TopicResearch asks for papers and studies: the services with a search of scholarly works of their own
	// use it (Exa category research paper, TinyFish research_paper, Serper Google Scholar), Tavily prefers
	// scholarly sites and still searches the rest, and the others are asked for papers in the query's words.
	TopicResearch = "research"
)

// ResearchSearch reports whether a kind has a search of its own for papers (Query.Topic research).
func ResearchSearch(kind string) bool {
	switch kind {
	case "exa", "tinyfish", "serper":
		return true
	}
	return false
}

// scholarlyDomains are the sites Tavily prefers for a research search (include_domains_mode prefer: it
// still searches the rest of the web): preprint servers, publishers, indexes and the Korean ones.
var scholarlyDomains = []string{
	"arxiv.org", "biorxiv.org", "medrxiv.org", "semanticscholar.org", "openreview.net", "aclanthology.org",
	"ncbi.nlm.nih.gov", "nature.com", "science.org", "sciencedirect.com", "springer.com", "wiley.com",
	"ieeexplore.ieee.org", "dl.acm.org", "ssrn.com", "researchgate.net", "plos.org", "mdpi.com",
	"dbpia.co.kr", "kci.go.kr", "riss.kr", "scienceon.kisti.re.kr", "koreascience.kr",
}

// researchWords asks a service without a search of papers for them in the query itself: "논문" for a
// Korean query, "paper" otherwise, unless the query already names papers or studies.
func researchWords(text string) string {
	low := strings.ToLower(text)
	for _, w := range []string{"논문", "연구", "학술", "paper", "study", "studies", "journal", "arxiv", "preprint"} {
		if strings.Contains(low, w) {
			return text
		}
	}
	if hasHangul(text) {
		return text + " 논문"
	}
	return text + " paper"
}

// pick is the service's own word (or number) for a depth: quick, standard (anything else) or deep.
func pick[T any](depth string, quick, standard, deep T) T {
	switch depth {
	case DepthQuick:
		return quick
	case DepthDeep:
		return deep
	}
	return standard
}

// freshSince is the first moment a freshness word lets in ("" for none).
func freshSince(fresh string, now time.Time) (time.Time, bool) {
	days := map[string]int{"day": 1, "week": 7, "month": 31}[fresh]
	if days == 0 {
		return time.Time{}, false
	}
	return now.AddDate(0, 0, -days).UTC(), true
}

// Result is one page found.
type Result struct {
	Title     string `json:"title"`
	URL       string `json:"url"`
	Snippet   string `json:"snippet"`
	Published string `json:"published,omitempty"`
	// Text is the page text a service chose for the query (Exa highlights, Tavily content, Linkup
	// content, Brave context), not cut to the snippet's length; "" when the service gives none.
	Text string `json:"text,omitempty"`
}

// Response is what a service returned: pages, and an answer when the service writes one.
type Response struct {
	Results []Result
	Answer  string
	CostUSD *float64 // when the service reports it (Exa)
	// Requests is how many requests of the search the service answered (what it may have charged for,
	// empty answers included); 0 is one when the search succeeded.
	Requests int
	// Lost is how many further requests of the search failed after they may have been charged
	// (MaybeCharged): a reworded query whose answer was lost, or the main query's when the reworded ones
	// answered the search. A failed search's own error is not counted here.
	Lost int
}

// Searcher is one configured search service.
type Searcher interface {
	Kind() string
	Configured() bool
	Search(ctx context.Context, q Query) (Response, error)
}

// ErrAuth means the service refused the key.
var ErrAuth = errors.New("websearch: key refused")

// snippetRunes bounds one result's text: enough for a model to answer from, small enough that five
// results stay well inside a prompt.
const snippetRunes = 700

// New builds the searcher for a provider kind.
func New(kind string, client *providers.Client, baseURL string, cred providers.Credential) (Searcher, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL[kind]
	}
	base := strings.TrimRight(baseURL, "/")
	switch kind {
	case "tavily":
		return &tavily{c: client, base: base, cred: cred}, nil
	case "linkup":
		return &linkup{c: client, base: base, cred: cred}, nil
	case "exa":
		return &exa{c: client, base: base, cred: cred}, nil
	case "serper":
		return &serper{c: client, base: base, cred: cred}, nil
	case "brave":
		return &brave{c: client, base: base, cred: cred}, nil
	case "tinyfish":
		return &tinyfish{c: client, base: base, cred: cred}, nil
	case "kakao_search":
		return &daum{a: kakao.New(client, base, cred)}, nil
	}
	return nil, fmt.Errorf("websearch: unknown kind %q", kind)
}

// size is how many results to ask for: 1–10, 5 when none is given; more than ten is ten (the schema's
// limit), not the default.
func size(n int) int {
	switch {
	case n < 1:
		return 5
	case n > 10:
		return 10
	}
	return n
}

// lead puts an outlet's name ahead of a snippet; a result with no snippet (TinyFish news is a headline,
// an outlet and a date) is just the outlet.
func lead(name, text string) string {
	switch {
	case name == "":
		return text
	case strings.TrimSpace(text) == "":
		return name
	}
	return name + " — " + text
}

// textRunes bounds Result.Text: the page text a service chose for the query, kept past the snippet's
// length so a search that reads pages can cut passages from it without opening the page.
const textRunes = 4000

// pageText cleans service text like a snippet (white space collapsed) but keeps the paragraph breaks
// (blank lines) Passages splits on, and bounds it to textRunes.
func pageText(s string) string {
	var paras []string
	for _, p := range strings.Split(strings.ReplaceAll(s, "\r\n", "\n"), "\n\n") {
		if p = strings.Join(strings.Fields(p), " "); p != "" {
			paras = append(paras, p)
		}
	}
	out := strings.Join(paras, "\n\n")
	if r := []rune(out); len(r) > textRunes {
		return string(r[:textRunes]) + "…"
	}
	return out
}

// cleanExtra keeps up to three non-empty reworded queries.
func cleanExtra(in []string) []string {
	var out []string
	for _, q := range in {
		if q = strings.Join(strings.Fields(q), " "); q != "" && len(out) < 3 {
			out = append(out, q)
		}
	}
	return out
}

func clip(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) <= snippetRunes {
		return s
	}
	return string(r[:snippetRunes]) + "…"
}

// post sends a JSON search and maps a refused key and the common error envelopes.
func post(ctx context.Context, c *providers.Client, url string, headers map[string]string, body any) (map[string]json.RawMessage, error) {
	raw, _ := json.Marshal(body)
	resp, err := c.PostJSON(ctx, url, headers, raw)
	if err != nil {
		var pe *providers.Error
		if errors.As(err, &pe) && (pe.Status == 401 || pe.Status == 403) {
			return nil, fmt.Errorf("%w: %s", ErrAuth, serviceMessage(resp))
		}
		if msg := serviceMessage(resp); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return decode.Object(resp.ContentType, resp.Body)
}

// serviceMessage is the error text a service put in its body ({"detail":…}, {"error":…}, {"message":…}).
func serviceMessage(resp *providers.Response) string {
	if resp == nil || len(resp.Body) == 0 {
		return ""
	}
	obj, err := decode.Object(resp.ContentType, resp.Body)
	if err != nil {
		return ""
	}
	for _, k := range []string{"detail", "error", "message"} {
		v, ok := obj[k]
		if !ok {
			continue
		}
		if s := decode.Str(v); s != "" {
			return clip(s)
		}
		var inner map[string]json.RawMessage
		if json.Unmarshal(v, &inner) == nil {
			for _, ik := range []string{"message", "error"} {
				if s := decode.Str(inner[ik]); s != "" {
					return clip(s)
				}
			}
		}
	}
	return ""
}

// ---------------------------------------------------------------------------
// Tavily
// ---------------------------------------------------------------------------

type tavily struct {
	c    *providers.Client
	base string
	cred providers.Credential
}

func (t *tavily) Kind() string                                          { return "tavily" }
func (t *tavily) Configured() bool                                      { return t.cred.Present() }
func (t *tavily) Search(ctx context.Context, q Query) (Response, error) { return fanOut(ctx, q, t.one) }

func (t *tavily) one(ctx context.Context, q Query) (Response, error) {
	body := map[string]any{"query": q.Text, "max_results": size(q.Max), "search_depth": pick(q.Depth, "fast", "basic", "advanced"), "include_answer": "basic",
		"include_published_date": true} // general results carry no date otherwise (news always does)
	if q.Depth != DepthQuick {
		body["chunks_per_source"] = 3 // the best three passages of each page, not one
	}
	if q.Depth == DepthDeep {
		body["include_answer"] = "advanced"
	}
	if q.Topic == TopicNews {
		body["topic"] = "news" // published_date comes with every news result
	} else if hasHangul(q.Text) {
		body["country"] = "south korea" // a boost, not a filter (general topic only)
	}
	if q.Topic == TopicResearch {
		// no search of papers: the scholarly sites first, the rest of the web still searched
		body["include_domains"], body["include_domains_mode"] = scholarlyDomains, "prefer"
	}
	if q.Fresh != "" {
		body["time_range"] = q.Fresh
	}
	obj, err := post(ctx, t.c, t.base+"/search", map[string]string{"Authorization": "Bearer " + t.cred.Value()}, body)
	if err != nil {
		return Response{}, err
	}
	return ParseTavily(obj)
}

// ParseTavily reads {"answer", "results":[{title,url,content,published_date}]}.
func ParseTavily(obj map[string]json.RawMessage) (Response, error) {
	var rows []struct {
		Title     string `json:"title"`
		URL       string `json:"url"`
		Content   string `json:"content"`
		Published string `json:"published_date"`
	}
	if err := json.Unmarshal(obj["results"], &rows); err != nil {
		return Response{}, fmt.Errorf("%w: tavily results", decode.ErrUnexpected)
	}
	out := Response{Answer: clip(decode.Str(obj["answer"]))}
	for _, r := range rows {
		if r.URL != "" {
			out.Results = append(out.Results, Result{Title: r.Title, URL: r.URL, Snippet: clip(r.Content), Published: r.Published, Text: pageText(r.Content)})
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Linkup
// ---------------------------------------------------------------------------

type linkup struct {
	c    *providers.Client
	base string
	cred providers.Credential
}

func (l *linkup) Kind() string     { return "linkup" }
func (l *linkup) Configured() bool { return l.cred.Present() }
func (l *linkup) Search(ctx context.Context, q Query) (Response, error) {
	text := q.Text
	if q.Topic == TopicResearch {
		text = researchWords(text) // no search of papers; a domain filter would leave Korean ones out
	}
	body := map[string]any{"q": text, "depth": pick(q.Depth, "flash", "standard", "deep"), "outputType": "searchResults", "maxResults": size(q.Max)}
	if from := freshFrom(q.Fresh, time.Now()); from != "" {
		body["fromDate"] = from
	}
	obj, err := post(ctx, l.c, l.base+"/v1/search", map[string]string{"Authorization": "Bearer " + l.cred.Value()}, body)
	if err != nil {
		return Response{}, err
	}
	return ParseLinkup(obj)
}

// freshFrom is Linkup's fromDate for a freshness word (it takes dates, not ranges).
func freshFrom(fresh string, now time.Time) string {
	since, ok := freshSince(fresh, now)
	if !ok {
		return ""
	}
	return since.Format("2006-01-02")
}

// ParseLinkup reads {"results":[{type,name,url,content}]} (text results only).
func ParseLinkup(obj map[string]json.RawMessage) (Response, error) {
	var rows []struct {
		Type    string `json:"type"`
		Name    string `json:"name"`
		URL     string `json:"url"`
		Content string `json:"content"`
	}
	if err := json.Unmarshal(obj["results"], &rows); err != nil {
		return Response{}, fmt.Errorf("%w: linkup results", decode.ErrUnexpected)
	}
	var out Response
	for _, r := range rows {
		if r.URL != "" && (r.Type == "" || r.Type == "text") {
			out.Results = append(out.Results, Result{Title: r.Name, URL: r.URL, Snippet: clip(r.Content), Text: pageText(r.Content)})
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Exa
// ---------------------------------------------------------------------------

type exa struct {
	c    *providers.Client
	base string
	cred providers.Credential
}

func (e *exa) Kind() string     { return "exa" }
func (e *exa) Configured() bool { return e.cred.Present() }
func (e *exa) Search(ctx context.Context, q Query) (Response, error) {
	// the highlights make the snippet as ever; the page text rides along on the same request at no
	// extra charge (measured 2026-11-11: $0.007 with or without text maxCharacters 3000)
	contents := map[string]any{"highlights": map[string]any{"maxCharacters": snippetRunes}}
	if q.Full {
		contents["text"] = map[string]any{"maxCharacters": pick(q.Depth, 3000, 3000, 4000)}
	}
	if q.Fresh == "day" && q.Full && q.Depth != DepthQuick {
		contents["maxAgeHours"] = 24 // a crawl from today, not the cached copy: only for a search that reads pages, never the light ones
	}
	// instant is marked legacy in Exa's reference (lite replaces it) but stays served and is what the
	// quick prices and timings here were measured on; deep-lite is the deep search (about 4 s, the same
	// content as deep in 2026-11-11's comparison)
	body := map[string]any{"query": q.Text, "type": pick(q.Depth, "instant", "auto", "deep-lite"), "numResults": size(q.Max),
		"contents": contents}
	switch q.Topic {
	case TopicNews:
		body["category"] = "news"
	case TopicResearch:
		body["category"] = "research paper"
	}
	if extra := RewordedQueries(q); len(extra) > 0 {
		body["additionalQueries"] = extra // only the deep types take them
	}
	if since, ok := freshSince(q.Fresh, time.Now()); ok {
		body["startPublishedDate"] = since.Format(time.RFC3339)
	}
	obj, err := post(ctx, e.c, e.base+"/search", map[string]string{"x-api-key": e.cred.Value()}, body)
	if err != nil {
		return Response{}, err
	}
	return ParseExa(obj)
}

// ParseExa reads {"results":[{title,url,publishedDate,highlights[],text}], "costDollars":{"total"}}.
func ParseExa(obj map[string]json.RawMessage) (Response, error) {
	var rows []struct {
		Title      string   `json:"title"`
		URL        string   `json:"url"`
		Published  string   `json:"publishedDate"`
		Highlights []string `json:"highlights"`
		Text       string   `json:"text"`
	}
	if err := json.Unmarshal(obj["results"], &rows); err != nil {
		return Response{}, fmt.Errorf("%w: exa results", decode.ErrUnexpected)
	}
	var out Response
	for _, r := range rows {
		if r.URL == "" {
			continue
		}
		text := strings.Join(r.Highlights, " … ")
		full := strings.Join(r.Highlights, "\n\n") // one paragraph per highlight
		if text == "" {
			text, full = r.Text, r.Text
		} else if strings.TrimSpace(r.Text) != "" {
			full = r.Text // the page text asked for with the highlights; the snippet stays the highlights
		}
		out.Results = append(out.Results, Result{Title: r.Title, URL: r.URL, Snippet: clip(text), Published: r.Published, Text: pageText(full)})
	}
	var cost struct {
		Total *float64 `json:"total"`
	}
	if json.Unmarshal(obj["costDollars"], &cost) == nil && cost.Total != nil {
		out.CostUSD = cost.Total
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Serper (Google)
// ---------------------------------------------------------------------------

type serper struct {
	c    *providers.Client
	base string
	cred providers.Credential
}

func (s *serper) Kind() string                                          { return "serper" }
func (s *serper) Configured() bool                                      { return s.cred.Present() }
func (s *serper) Search(ctx context.Context, q Query) (Response, error) { return fanOut(ctx, q, s.one) }

func (s *serper) one(ctx context.Context, q Query) (Response, error) {
	body := map[string]any{"q": q.Text, "num": size(q.Max)}
	if hasHangul(q.Text) {
		body["gl"], body["hl"] = "kr", "ko"
	}
	switch q.Fresh {
	case "day":
		body["tbs"] = "qdr:d"
	case "week":
		body["tbs"] = "qdr:w"
	case "month":
		body["tbs"] = "qdr:m"
	}
	path := "/search"
	switch q.Topic {
	case TopicNews:
		path = "/news" // Google News: news[].title/link/snippet/date/source
	case TopicResearch:
		path = "/scholar" // Google Scholar: organic[].title/link/snippet/publicationInfo/year
		delete(body, "tbs")
	}
	obj, err := post(ctx, s.c, s.base+path, map[string]string{"X-API-KEY": s.cred.Value()}, body)
	if err != nil {
		return Response{}, err
	}
	switch path {
	case "/news":
		return ParseSerperNews(obj)
	case "/scholar":
		return ParseSerperScholar(obj)
	}
	return ParseSerper(obj)
}

// ParseSerperNews reads {"news":[{title,link,snippet,date,source}]}; the outlet leads the snippet.
func ParseSerperNews(obj map[string]json.RawMessage) (Response, error) {
	var rows []struct {
		Title   string `json:"title"`
		Link    string `json:"link"`
		Snippet string `json:"snippet"`
		Date    string `json:"date"`
		Source  string `json:"source"`
	}
	if err := json.Unmarshal(obj["news"], &rows); err != nil {
		return Response{}, fmt.Errorf("%w: serper news", decode.ErrUnexpected)
	}
	var out Response
	for _, r := range rows {
		if r.Link == "" {
			continue
		}
		out.Results = append(out.Results, Result{Title: r.Title, URL: r.Link, Snippet: clip(lead(r.Source, r.Snippet)), Published: r.Date})
	}
	return out, nil
}

// ParseSerperScholar reads Google Scholar's {"organic":[{title,link,snippet,publicationInfo,year,citedBy}]};
// the authors and venue lead the snippet and the year stands for the date.
func ParseSerperScholar(obj map[string]json.RawMessage) (Response, error) {
	var rows []struct {
		Title           string `json:"title"`
		Link            string `json:"link"`
		Snippet         string `json:"snippet"`
		PublicationInfo string `json:"publicationInfo"`
		Year            int    `json:"year"`
	}
	if err := json.Unmarshal(obj["organic"], &rows); err != nil {
		return Response{}, fmt.Errorf("%w: serper scholar", decode.ErrUnexpected)
	}
	var out Response
	for _, r := range rows {
		if r.Link == "" {
			continue
		}
		res := Result{Title: r.Title, URL: r.Link, Snippet: clip(lead(r.PublicationInfo, r.Snippet))}
		if r.Year > 0 {
			res.Published = strconv.Itoa(r.Year)
		}
		out.Results = append(out.Results, res)
	}
	return out, nil
}

// ParseSerper reads {"answerBox":{answer|snippet}, "organic":[{title,link,snippet,date}]}.
func ParseSerper(obj map[string]json.RawMessage) (Response, error) {
	raw, ok := obj["organic"]
	if !ok {
		return Response{}, fmt.Errorf("%w: serper organic", decode.ErrUnexpected)
	}
	var rows []struct {
		Title   string `json:"title"`
		Link    string `json:"link"`
		Snippet string `json:"snippet"`
		Date    string `json:"date"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return Response{}, fmt.Errorf("%w: serper organic", decode.ErrUnexpected)
	}
	var out Response
	var box struct {
		Answer  string `json:"answer"`
		Snippet string `json:"snippet"`
	}
	if json.Unmarshal(obj["answerBox"], &box) == nil {
		out.Answer = clip(box.Answer)
		if out.Answer == "" {
			out.Answer = clip(box.Snippet)
		}
	}
	for _, r := range rows {
		if r.Link != "" {
			out.Results = append(out.Results, Result{Title: r.Title, URL: r.Link, Snippet: clip(r.Snippet), Published: r.Date})
		}
	}
	return out, nil
}

func hasHangul(s string) bool {
	for _, r := range s {
		if r >= 0xAC00 && r <= 0xD7A3 {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Kakao (Daum web)
// ---------------------------------------------------------------------------

type daum struct{ a *kakao.Adapter }

func (d *daum) Kind() string     { return "kakao_search" }
func (d *daum) Configured() bool { return d.a.Configured() }
func (d *daum) Search(ctx context.Context, q Query) (Response, error) {
	if q.Depth != DepthDeep {
		return d.single(ctx, q)
	}
	// deep: the main query asks Daum web and Daum blogs at once (same key; the blog posts are
	// Korean-heavy and readable) and the reworded queries ask the web only; with a single request
	// reserved the main query asks the web alone
	blog := q.Requests == 0 || q.Requests >= 2
	main := 1
	if blog {
		main = 2
	}
	return fanOutWith(ctx, q, main, func(ctx context.Context, q Query) (Response, error) { return d.deepMain(ctx, q, blog) }, d.web)
}

// single is one web request, counted.
func (d *daum) single(ctx context.Context, q Query) (Response, error) {
	res, err := d.web(ctx, q)
	if err == nil {
		res.Requests = 1
	}
	return res, err
}

// deepMain is the main query of a deep search: web and blog merged, cut to the size asked.
func (d *daum) deepMain(ctx context.Context, q Query, blog bool) (Response, error) {
	if !blog {
		return d.single(ctx, q)
	}
	type answer struct {
		res Response
		err error
	}
	bc := make(chan answer, 1)
	go func() {
		res, err := safely(ctx, q, d.blog)
		bc <- answer{res, err}
	}()
	web, err := safely(ctx, q, d.web)
	b := <-bc
	switch {
	case err != nil && b.err != nil:
		return Response{}, err
	case err != nil:
		b.res.Requests = 1
		return b.res, nil
	case b.err != nil:
		web.Requests = 1
		return web, nil
	}
	web.Results = MergeResults(web.Results, b.res.Results)
	if len(web.Results) > size(q.Max) {
		web.Results = web.Results[:size(q.Max)]
	}
	web.Requests = 2
	return web, nil
}

func (d *daum) web(ctx context.Context, q Query) (Response, error) {
	res, err := d.a.SearchWeb(ctx, daumText(q), size(q.Max))
	return d.response(res, err)
}

// daumText is the query Daum is sent: Daum has no search of papers or news of its own, so a research
// search asks for papers in its words.
func daumText(q Query) string {
	if q.Topic == TopicResearch {
		return researchWords(q.Text)
	}
	return q.Text
}

// blog asks Daum's blog search (/v2/search/blog), whose documents have the shape of the web ones.
func (d *daum) blog(ctx context.Context, q Query) (Response, error) {
	if !d.a.Configured() {
		return Response{}, providers.ErrNoCredential
	}
	v := url.Values{}
	v.Set("query", strings.TrimSpace(daumText(q)))
	v.Set("size", strconv.Itoa(size(q.Max)))
	resp, err := d.a.Client.Get(ctx, d.a.BaseURL+"/v2/search/blog?"+v.Encode(), map[string]string{"Authorization": "KakaoAK " + d.a.Cred.Value()})
	if err != nil {
		return d.response(kakao.Result{}, err)
	}
	res, err := kakao.Parse(resp.ContentType, resp.Body)
	return d.response(res, err)
}

func (d *daum) response(res kakao.Result, err error) (Response, error) {
	if err != nil {
		var pe *providers.Error
		if errors.As(err, &pe) && (pe.Status == 401 || pe.Status == 403) {
			return Response{}, fmt.Errorf("%w: %v", ErrAuth, err)
		}
		return Response{}, err
	}
	var out Response
	for _, doc := range res.Documents {
		out.Results = append(out.Results, Result{Title: doc.Title, URL: doc.URL, Snippet: clip(doc.Contents), Published: doc.Datetime})
	}
	return out, nil
}

// Host is a web service the console offers to add: a search service, or the page reader.
type Host struct {
	Kind    string `json:"kind"`
	ID      string `json:"id"` // the provider name config uses by default
	Name    string `json:"name"`
	BaseURL string `json:"base_url"`
	KeyEnv  string `json:"key_env"`
	KeyURL  string `json:"key_url"`
	Keyless bool   `json:"keyless,omitempty"` // works without a key (a key raises its limits)
}

// Hosts lists them in the order search.web tries them, then the reader.
var Hosts = []Host{
	{Kind: "tavily", ID: "tavily", Name: "Tavily", BaseURL: DefaultBaseURL["tavily"], KeyEnv: "MASAMONG_TAVILY_KEY", KeyURL: "https://app.tavily.com/home"},
	{Kind: "linkup", ID: "linkup", Name: "Linkup", BaseURL: DefaultBaseURL["linkup"], KeyEnv: "MASAMONG_LINKUP_KEY", KeyURL: "https://app.linkup.so/api-keys"},
	{Kind: "exa", ID: "exa", Name: "Exa", BaseURL: DefaultBaseURL["exa"], KeyEnv: "MASAMONG_EXA_KEY", KeyURL: "https://dashboard.exa.ai/api-keys"},
	{Kind: "serper", ID: "serper", Name: "Serper (Google)", BaseURL: DefaultBaseURL["serper"], KeyEnv: "MASAMONG_SERPER_KEY", KeyURL: "https://serper.dev/api-keys"},
	{Kind: "brave", ID: "brave", Name: "Brave Search", BaseURL: DefaultBaseURL["brave"], KeyEnv: "MASAMONG_BRAVE_KEY", KeyURL: "https://api-dashboard.search.brave.com/app/keys"},
	{Kind: "tinyfish", ID: "tinyfish", Name: "TinyFish", BaseURL: DefaultBaseURL["tinyfish"], KeyEnv: "MASAMONG_TINYFISH_KEY", KeyURL: "https://agent.tinyfish.ai/api-keys"},
	{Kind: "kakao_search", ID: "kakao", Name: "Kakao (Daum)", BaseURL: DefaultBaseURL["kakao_search"], KeyEnv: "MASAMONG_KAKAO_KEY", KeyURL: "https://developers.kakao.com/console/app"},
	{Kind: "jina_reader", ID: "jina", Name: "Jina Reader", BaseURL: ReaderBaseURL, KeyEnv: "MASAMONG_JINA_KEY", KeyURL: "https://jina.ai/api-dashboard", Keyless: true},
}

// HostFor is the listed host of a kind.
func HostFor(kind string) (Host, bool) {
	for _, h := range Hosts {
		if h.Kind == kind {
			return h, true
		}
	}
	return Host{}, false
}
