package websearch

import (
	"context"
	"errors"
	"sync"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

// jinaFetch reads pages through Jina Reader, the fallback when TinyFish Fetch is not configured. The
// Reader's own client gate decides how many go at once (one without a key), so pages beyond that
// wait their turn and the search's deadlines cut off the rest.
type jinaFetch struct{ r *Reader }

// NewJinaFetcher wraps a Reader as a Fetcher. Its requests are not retried.
func NewJinaFetcher(r *Reader) Fetcher {
	rr := *r
	cc := *r.c
	cc.MaxRetries = 0
	rr.c = &cc
	return &jinaFetch{r: &rr}
}

func (j *jinaFetch) Kind() string { return "jina_reader" }

func (j *jinaFetch) Fetch(ctx context.Context, urls []string, maxChars int) ([]Fetched, error) {
	var mu sync.Mutex
	var svc error
	out := gather(ctx, len(urls), func(ctx context.Context, i int) Fetched {
		f, err := j.one(ctx, urls[i], maxChars)
		if err != nil {
			mu.Lock()
			if svc == nil {
				svc = err
			}
			mu.Unlock()
		}
		return f
	})
	return finishFetch(urls, out, svc)
}

func (j *jinaFetch) one(ctx context.Context, raw string, maxChars int) (Fetched, error) {
	page, err := j.r.Read(ctx, raw, maxChars)
	if err == nil {
		return Fetched{URL: page.URL, Title: page.Title, Content: page.Content, Truncated: page.Truncated}, nil
	}
	f := Fetched{URL: raw}
	var pe *providers.Error
	switch {
	case errors.Is(err, ErrNotPublic):
		f.Err = "not_public"
	case ctx.Err() != nil:
		f.Err = "timeout"
	case errors.Is(err, ErrAuth):
		f.Err = "service"
		return f, err
	case errors.Is(err, decode.ErrUnexpected):
		f.Err = "empty"
	case errors.As(err, &pe) && (pe.Status == 402 || pe.Status == 429 || pe.Status >= 500):
		f.Err = "service"
		return f, err
	case errors.As(err, &pe) && pe.Kind == "timeout":
		f.Err = "timeout"
	case errors.As(err, &pe) && pe.Status == 0:
		f.Err = "service" // no connection to the Reader
		return f, err
	default:
		f.Err = "error"
	}
	return f, nil
}
