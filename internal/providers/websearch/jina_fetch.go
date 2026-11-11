package websearch

import (
	"context"
	"errors"

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
	out, errs := gather(ctx, len(urls), func(ctx context.Context, i int) (Fetched, error) {
		return j.one(ctx, urls[i], maxChars)
	})
	return finishFetch(urls, out, serviceError(errs))
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
	case errors.As(err, &pe) && pe.Kind == "too_large":
		f.Err = "error" // this page's answer was too big: the page's failure, not the Reader's
	case errors.As(err, &pe) && pe.Kind == "timeout":
		f.Err = "timeout"
	case errors.As(err, &pe) && (pe.Status == 0 || pe.Kind == "connection"):
		f.Err = "service" // no connection to the Reader, or it was lost mid-answer
		return f, err
	default:
		f.Err = "error"
	}
	return f, nil
}
