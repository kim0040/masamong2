package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

// get sends a search as a query string and maps a refused key and the error envelopes, as post does.
func get(ctx context.Context, c *providers.Client, rawURL string, headers map[string]string) (map[string]json.RawMessage, error) {
	resp, err := c.Get(ctx, rawURL, headers)
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

// ---------------------------------------------------------------------------
// Brave Search
// ---------------------------------------------------------------------------

type brave struct {
	c    *providers.Client
	base string
	cred providers.Credential
}

// braveFresh is Brave's freshness word: past day, week or month.
var braveFresh = map[string]string{"day": "pd", "week": "pw", "month": "pm"}

func (b *brave) Kind() string     { return "brave" }
func (b *brave) Configured() bool { return b.cred.Present() }

// Search uses the news search for news, the LLM context endpoint (chunks taken from the pages, not the
// result page's one-line description) for a deep search, and the web search otherwise.
func (b *brave) Search(ctx context.Context, q Query) (Response, error) {
	v := url.Values{}
	v.Set("q", q.Text)
	if hasHangul(q.Text) {
		v.Set("country", "KR")
		v.Set("search_lang", "ko")
	}
	if f := braveFresh[q.Fresh]; f != "" {
		v.Set("freshness", f)
	}
	path := "/res/v1/web/search"
	switch {
	case q.Topic == TopicNews:
		path = "/res/v1/news/search"
		v.Set("count", strconv.Itoa(size(q.Max)))
		v.Set("extra_snippets", "true")
	case q.Depth == DepthDeep:
		path = "/res/v1/llm/context"
		v.Set("count", "20") // results considered; the sources kept are maximum_number_of_urls
		v.Set("maximum_number_of_urls", strconv.Itoa(size(q.Max)))
		v.Set("maximum_number_of_tokens", "4096")
	default:
		v.Set("count", strconv.Itoa(size(q.Max)))
		v.Set("text_decorations", "false")
		if q.Depth != DepthQuick {
			v.Set("extra_snippets", "true")
		}
	}
	obj, err := get(ctx, b.c, b.base+path+"?"+v.Encode(), map[string]string{"X-Subscription-Token": b.cred.Value(), "Accept": "application/json"})
	if err != nil {
		return Response{}, err
	}
	switch path {
	case "/res/v1/news/search":
		return ParseBraveNews(obj)
	case "/res/v1/llm/context":
		return ParseBraveContext(obj)
	}
	return ParseBrave(obj)
}

type braveResult struct {
	Title         string   `json:"title"`
	URL           string   `json:"url"`
	Description   string   `json:"description"`
	Age           string   `json:"age"`
	PageAge       string   `json:"page_age"`
	ExtraSnippets []string `json:"extra_snippets"`
	Profile       struct {
		Name string `json:"name"`
	} `json:"profile"`
}

func (r braveResult) result(source bool) Result {
	text := strings.Join(append([]string{r.Description}, r.ExtraSnippets...), " … ")
	if source {
		text = lead(r.Profile.Name, text)
	}
	when := r.PageAge
	if when == "" {
		when = r.Age
	}
	return Result{Title: stripMarks(r.Title), URL: r.URL, Snippet: clip(stripMarks(text)), Published: when}
}

// ParseBrave reads {"web":{"results":[{title,url,description,age,page_age,extra_snippets}]}}. A search
// with no web results has no "web" at all.
func ParseBrave(obj map[string]json.RawMessage) (Response, error) {
	if _, ok := obj["web"]; !ok {
		if _, ok := obj["query"]; ok {
			return Response{}, nil
		}
		return Response{}, fmt.Errorf("%w: brave web", decode.ErrUnexpected)
	}
	var web struct {
		Results []braveResult `json:"results"`
	}
	if err := json.Unmarshal(obj["web"], &web); err != nil {
		return Response{}, fmt.Errorf("%w: brave web", decode.ErrUnexpected)
	}
	var out Response
	for _, r := range web.Results {
		if r.URL != "" {
			out.Results = append(out.Results, r.result(false))
		}
	}
	return out, nil
}

// ParseBraveNews reads {"type":"news","results":[{title,url,description,age,page_age,profile{name}}]};
// the outlet leads the snippet.
func ParseBraveNews(obj map[string]json.RawMessage) (Response, error) {
	var rows []braveResult
	if err := json.Unmarshal(obj["results"], &rows); err != nil {
		return Response{}, fmt.Errorf("%w: brave news", decode.ErrUnexpected)
	}
	var out Response
	for _, r := range rows {
		if r.URL != "" {
			out.Results = append(out.Results, r.result(true))
		}
	}
	return out, nil
}

// ParseBraveContext reads {"grounding":{"generic":[{url,title,snippets[]}]}, "sources":{url:{age:[…]}}}.
func ParseBraveContext(obj map[string]json.RawMessage) (Response, error) {
	var g struct {
		Generic []struct {
			URL      string   `json:"url"`
			Title    string   `json:"title"`
			Snippets []string `json:"snippets"`
		} `json:"generic"`
	}
	if err := json.Unmarshal(obj["grounding"], &g); err != nil {
		return Response{}, fmt.Errorf("%w: brave grounding", decode.ErrUnexpected)
	}
	var sources map[string]struct {
		Age []string `json:"age"`
	}
	_ = json.Unmarshal(obj["sources"], &sources)
	var out Response
	for _, r := range g.Generic {
		if r.URL == "" {
			continue
		}
		res := Result{Title: r.Title, URL: r.URL, Snippet: clip(strings.Join(r.Snippets, " … "))}
		res.Published = braveDate(sources[r.URL].Age)
		out.Results = append(out.Results, res)
	}
	return out, nil
}

// braveDate picks the machine-readable date out of a source's age list: [full date, YYYY-MM-DD, relative
// age, ISO 8601 timestamp] (the timestamp came in 2026-07-31; before that the list had three). The
// timestamp when there is one, else the plain date, else nothing (a page with no known date has none).
func braveDate(age []string) string {
	for _, i := range []int{3, 1} {
		if i < len(age) && len(age[i]) >= 10 && age[i][4] == '-' && age[i][7] == '-' {
			return age[i]
		}
	}
	return ""
}

// stripMarks removes the <strong> marks Brave puts around matched words when decorations are on.
func stripMarks(s string) string {
	return strings.NewReplacer("<strong>", "", "</strong>", "").Replace(s)
}

// ---------------------------------------------------------------------------
// TinyFish
// ---------------------------------------------------------------------------

type tinyfish struct {
	c    *providers.Client
	base string
	cred providers.Credential
}

// tinyfishMinutes is TinyFish's recency window for a freshness word.
var tinyfishMinutes = map[string]int{"day": 1440, "week": 10080, "month": 44640}

func (t *tinyfish) Kind() string     { return "tinyfish" }
func (t *tinyfish) Configured() bool { return t.cred.Present() }

// Search asks one page (about ten results; the API takes no count) of the web or, for news, of news
// articles. TinyFish has no depth setting.
func (t *tinyfish) Search(ctx context.Context, q Query) (Response, error) {
	v := url.Values{}
	v.Set("query", q.Text)
	if hasHangul(q.Text) {
		v.Set("location", "KR")
		v.Set("language", "ko")
	}
	if q.Topic == TopicNews {
		v.Set("domain_type", "news")
	}
	if m := tinyfishMinutes[q.Fresh]; m > 0 {
		v.Set("recency_minutes", strconv.Itoa(m))
	}
	obj, err := get(ctx, t.c, strings.TrimRight(t.base, "/")+"/?"+v.Encode(), map[string]string{"X-API-Key": t.cred.Value()})
	if err != nil {
		return Response{}, err
	}
	res, err := ParseTinyFish(obj)
	if err == nil && len(res.Results) > size(q.Max) {
		res.Results = res.Results[:size(q.Max)]
	}
	return res, err
}

// ParseTinyFish reads {"results":[{position,site_name,title,snippet,url,date,publisher}],"total_results"}.
func ParseTinyFish(obj map[string]json.RawMessage) (Response, error) {
	var rows []struct {
		Title     string `json:"title"`
		URL       string `json:"url"`
		Snippet   string `json:"snippet"`
		Date      string `json:"date"`
		Publisher string `json:"publisher"`
	}
	if err := json.Unmarshal(obj["results"], &rows); err != nil {
		return Response{}, fmt.Errorf("%w: tinyfish results", decode.ErrUnexpected)
	}
	var out Response
	for _, r := range rows {
		if r.URL == "" {
			continue
		}
		out.Results = append(out.Results, Result{Title: r.Title, URL: r.URL, Snippet: clip(lead(r.Publisher, r.Snippet)), Published: r.Date})
	}
	return out, nil
}
