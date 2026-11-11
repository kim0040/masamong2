package websearch

import "context"

// Fetcher reads web pages as text for the passages a search adds to its results (TinyFish Fetch, or
// Jina Reader as the fallback). Pages are fetched by the service, never by this process.
type Fetcher interface {
	Kind() string
	// Fetch reads urls; out[i] answers urls[i]. It returns when every page is in or ctx ends (a page
	// still missing then carries Err "timeout"). err is only the service's own failure (auth, 5xx, the
	// request refused), never one page's.
	Fetch(ctx context.Context, urls []string, maxChars int) (out []Fetched, err error)
}

// Fetched is one page a Fetcher read.
type Fetched struct {
	URL       string
	Title     string
	Content   string
	Truncated bool
	// Err is why the page has no content: "" (it has), timeout, blocked, login, empty, not_public, error,
	// service (the fetch service itself failed for this page).
	Err string
	// Target is true when the fetch service itself reported Err for this address (the page's own host
	// timed out, blocked the reader or wants a login). Our cut-offs (a soft or hard deadline, a page still
	// queued, a cancelled turn) and the service's failures leave it false; only a Target failure says
	// anything about the host (HostCache).
	Target bool
}
