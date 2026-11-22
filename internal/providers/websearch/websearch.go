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
	"strings"
	"time"
	"unicode"

	"masamong/internal/observability"
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
	for _, w := range []string{"논문", "연구", "학술"} {
		if strings.Contains(low, w) {
			return text
		}
	}
	// English words whole: "paperclip", "newspaper" and "journalism" do not ask for papers
	for _, w := range strings.FieldsFunc(low, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) }) {
		switch w {
		case "paper", "papers", "study", "studies", "journal", "journals", "arxiv", "preprint", "preprints":
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

// ErrCredentialTooShort refuses a credential shorter than minScrubbed before any call: the service's
// answer can repeat the key where scrub cannot cut it, so the call is not made. It is an ErrAuth —
// the key is refused as unusable, not answered by the service.
var ErrCredentialTooShort = fmt.Errorf("%w: shorter than %d bytes", ErrAuth, minScrubbed)

// authError is a refused key: ErrAuth to errors.Is, and it keeps the HTTP status as a
// *providers.Error to errors.As, so the console can show "refused (401)" without the body's text.
type authError struct {
	pe  *providers.Error
	msg string // the service's own words, scrubbed; "" for none
}

func (e *authError) Error() string {
	if e.msg == "" {
		return ErrAuth.Error()
	}
	return ErrAuth.Error() + ": " + e.msg
}

func (e *authError) Unwrap() []error {
	if e.pe == nil {
		return []error{ErrAuth}
	}
	return []error{ErrAuth, e.pe}
}

// shortCred is the searcher for a key too short to send: every search refuses before the wire.
type shortCred struct{ kind string }

func (s shortCred) Kind() string     { return s.kind }
func (s shortCred) Configured() bool { return true }
func (s shortCred) Search(context.Context, Query) (Response, error) {
	return Response{}, ErrCredentialTooShort
}

// shortFetch is the page fetcher for the same key.
type shortFetch struct{}

func (shortFetch) Kind() string { return "tinyfish_fetch" }
func (shortFetch) Fetch(context.Context, []string, int) ([]Fetched, error) {
	return nil, ErrCredentialTooShort
}

// snippetRunes bounds one result's text: enough for a model to answer from, small enough that five
// results stay well inside a prompt.
const snippetRunes = 700

// New builds the searcher for a provider kind.
func New(kind string, client *providers.Client, baseURL string, cred providers.Credential) (Searcher, error) {
	if baseURL == "" {
		baseURL = DefaultBaseURL[kind]
	}
	base := strings.TrimRight(baseURL, "/")
	var out Searcher
	switch kind {
	case "tavily":
		out = &tavily{c: client, base: base, cred: cred}
	case "linkup":
		out = &linkup{c: client, base: base, cred: cred}
	case "exa":
		out = &exa{c: client, base: base, cred: cred}
	case "serper":
		out = &serper{c: client, base: base, cred: cred}
	case "brave":
		out = &brave{c: client, base: base, cred: cred}
	case "tinyfish":
		out = &tinyfish{c: client, base: base, cred: cred}
	case "kakao_search":
		out = &daum{a: kakao.New(client, base, cred)}
	default:
		return nil, fmt.Errorf("websearch: unknown kind %q", kind)
	}
	// A key shorter than minScrubbed cannot be cut back out of the service's answer, so it is refused
	// here rather than sent once and echoed where scrub cannot reach.
	if !scrubbable(cred) {
		return shortCred{kind: kind}, nil
	}
	return out, nil
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
		secrets := headerSecrets(headers)
		if errors.As(err, &pe) && (pe.Status == 401 || pe.Status == 403) {
			return nil, &authError{pe: pe, msg: serviceMessage(resp, secrets...)}
		}
		if msg := serviceMessage(resp, secrets...); msg != "" {
			return nil, fmt.Errorf("%w: %s", err, msg)
		}
		return nil, err
	}
	return decode.Object(resp.ContentType, resp.Body)
}

// minScrubbed is the shortest key value cut out of a service message by value: a shorter one would also
// eat ordinary words of the message.
const minScrubbed = 6

// headerSecrets is the credential values a request carries in its headers: the whole header value and,
// for "Scheme value", the value alone. Headers that name a format, not a credential, are skipped.
func headerSecrets(headers map[string]string) []string {
	var out []string
	for k, v := range headers {
		if strings.EqualFold(k, "Accept") || strings.EqualFold(k, "Content-Type") || strings.EqualFold(k, "User-Agent") {
			continue
		}
		out = append(out, v)
		if _, rest, ok := strings.Cut(v, " "); ok {
			out = append(out, rest)
		}
	}
	return out
}

// scrubbable reports whether a key can be sent: absent, or long enough for scrub to cut it back out
// of the service's answer. A shorter one is refused as ErrCredentialTooShort instead of sent.
func scrubbable(cred providers.Credential) bool {
	return !cred.Present() || len(cred.Value()) >= minScrubbed
}

// scrub removes every given secret value, and anything that looks like a key, from text a service sent
// back: a service that refuses a key often repeats it ("invalid key abc123"). The text is shown in the
// console and logs, so it never carries the key.
func scrub(text string, secrets ...string) string {
	for _, sec := range secrets {
		if len(sec) >= minScrubbed {
			text = strings.ReplaceAll(text, sec, "***")
		}
	}
	return observability.Redact(text)
}

// serviceMessage is the error text a service put in its body ({"detail":…}, {"error":…}, {"message":…}),
// with the request's key values (secrets) and key-shaped text removed.
func serviceMessage(resp *providers.Response, secrets ...string) string {
	if resp == nil || len(resp.Body) == 0 {
		return ""
	}
	return clip(scrub(rawServiceMessage(resp), secrets...))
}

// rawServiceMessage is the message as the service sent it: not clipped yet, because a cut in the middle of
// a key would hide it from scrub.
func rawServiceMessage(resp *providers.Response) string {
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
			return s
		}
		var inner map[string]json.RawMessage
		if json.Unmarshal(v, &inner) == nil {
			for _, ik := range []string{"message", "error"} {
				if s := decode.Str(inner[ik]); s != "" {
					return s
				}
			}
		}
	}
	return ""
}
func hasHangul(s string) bool {
	for _, r := range s {
		if r >= 0xAC00 && r <= 0xD7A3 {
			return true
		}
	}
	return false
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
