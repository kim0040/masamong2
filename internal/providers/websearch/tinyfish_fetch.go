package websearch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

// FetchBaseURL is TinyFish Fetch's address. It takes the key of TinyFish Search (X-API-Key) and reads
// a page in a real browser: markdown out, 1,000 URLs a day free, 150 a minute.
const FetchBaseURL = "https://api.fetch.tinyfish.ai"

// The service batches up to ten URLs in a request, but answers only when the slowest is done; one
// slow page would hold the others past a search's deadline. So each page is its own request, all at
// once, and a request is never retried (a retry of a slow page doubles the wait for nothing).
type tinyfishFetch struct {
	c    *providers.Client
	base string
	cred providers.Credential
}

// NewTinyFishFetcher reads pages through TinyFish Fetch; baseURL "" is FetchBaseURL.
func NewTinyFishFetcher(c *providers.Client, baseURL string, cred providers.Credential) Fetcher {
	if baseURL == "" {
		baseURL = FetchBaseURL
	}
	cc := *c
	cc.MaxRetries = 0
	return &tinyfishFetch{c: &cc, base: strings.TrimRight(baseURL, "/"), cred: cred}
}

func (t *tinyfishFetch) Kind() string { return "tinyfish_fetch" }

// Fetch reads each address in its own request, all at once. A page that failed carries Err; err is
// set only when the service itself failed (key refused, daily allowance used, rate limit, 5xx, no
// connection) and no page could be read.
func (t *tinyfishFetch) Fetch(ctx context.Context, urls []string, maxChars int) ([]Fetched, error) {
	if maxChars <= 0 {
		maxChars = 6000
	}
	out, errs := gather(ctx, len(urls), func(ctx context.Context, i int) (Fetched, error) {
		return t.one(ctx, urls[i], maxChars)
	})
	return finishFetch(urls, out, serviceError(errs))
}

// finishFetch puts each page's address in and decides whether the service's failure is the call's.
func finishFetch(urls []string, out []Fetched, svc error) ([]Fetched, error) {
	read := 0
	for i := range out {
		if out[i].URL == "" && i < len(urls) {
			out[i].URL = urls[i]
		}
		if out[i].Err == "" {
			read++
		}
	}
	if svc != nil && read > 0 && !errors.Is(svc, ErrAuth) {
		svc = nil // some pages came: the service works, one request of it did not
	}
	return out, svc
}

// perURLMillis is the time a page may take at the service: what is left of ctx, in 1–30 s.
func perURLMillis(ctx context.Context) int {
	d := 15 * time.Second
	if dl, ok := ctx.Deadline(); ok {
		d = time.Until(dl) - 300*time.Millisecond
	}
	return int(min(max(d, time.Second), 30*time.Second) / time.Millisecond)
}

// fetchErrKind is the Fetched.Err for an error code of the service.
func fetchErrKind(code string, status int) string {
	switch code {
	case "timeout":
		return "timeout"
	case "bot_blocked":
		return "blocked"
	case "login_required":
		return "login"
	case "empty_content":
		return "empty"
	case "target_http_error":
		switch status {
		case 401:
			return "login"
		case 403:
			return "blocked"
		}
		// 429 and the rest: the host's rate limit is a bad minute, not a closed door
	}
	return "error" // page_not_found, target_unreachable, content_too_large, invalid_url, ...
}

// report puts the service's error code for the address into the page. It says the target caused the
// failure (Fetched.Target) when the service named a timeout, a block or a login for that address.
func (f *Fetched) report(code string, status int) {
	f.Err = fetchErrKind(code, status)
	f.Target = f.Err == "timeout" || f.Err == "blocked" || f.Err == "login"
}

type fetchBody struct {
	Results []struct {
		URL      string `json:"url"`
		FinalURL string `json:"final_url"`
		Title    string `json:"title"`
		Text     string `json:"text"`
	} `json:"results"`
	Errors []struct {
		URL    string `json:"url"`
		Error  string `json:"error"`
		Status int    `json:"status"`
	} `json:"errors"`
}

// one reads a page. The second result is the service's own failure, if that is what went wrong.
func (t *tinyfishFetch) one(ctx context.Context, raw string, maxChars int) (Fetched, error) {
	target, err := PublicURL(raw)
	if err != nil {
		return Fetched{URL: raw, Err: "not_public"}, nil
	}
	body, _ := json.Marshal(map[string]any{"urls": []string{target}, "format": "markdown", "links": false, "image_links": false,
		"per_url_timeout_ms": perURLMillis(ctx)})
	resp, err := t.c.PostJSON(ctx, t.base, map[string]string{"X-API-Key": t.cred.Value()}, body)
	page := Fetched{URL: target}
	if err != nil {
		if ctx.Err() != nil {
			page.Err = "timeout" // our deadline, not the service's failure
			return page, nil
		}
		var pe *providers.Error
		if errors.As(err, &pe) {
			switch {
			case pe.Status == 401 || pe.Status == 403:
				page.Err = "service"
				return page, fmt.Errorf("%w: tinyfish fetch", ErrAuth)
			case pe.Status == 402 || pe.Status == 429 || pe.Status >= 500:
				page.Err = "service"
				return page, err // 402: the day's allowance is used; 429: the rate limit
			case pe.Kind == "timeout":
				page.Err = "timeout" // the request to the service timed out: ours, not the page's
				return page, nil
			case pe.Kind == "too_large":
				page.Err = "error" // this page's answer was over the client's limit: the page's failure, not the service's
				return page, nil
			case pe.Status >= 400: // refused for this address: its error list says why
				if resp != nil {
					var b fetchBody
					if json.Unmarshal(resp.Body, &b) == nil && len(b.Errors) > 0 {
						page.report(b.Errors[0].Error, b.Errors[0].Status)
						return page, nil
					}
				}
				page.Err = "error"
				return page, nil
			}
		}
		page.Err = "service"
		return page, err
	}
	var b fetchBody
	if json.Unmarshal(resp.Body, &b) != nil {
		page.Err = "service"
		return page, fmt.Errorf("%w: tinyfish fetch", decode.ErrUnexpected)
	}
	if len(b.Results) > 0 {
		r := b.Results[0]
		text := strings.TrimSpace(r.Text)
		if text == "" {
			page.Err = "empty"
			return page, nil
		}
		if r.FinalURL != "" {
			page.URL = r.FinalURL
		}
		page.Title, page.Content = strings.TrimSpace(r.Title), text
		if rs := []rune(text); len(rs) > maxChars {
			page.Content, page.Truncated = string(rs[:maxChars])+"…", true
		}
		return page, nil
	}
	if len(b.Errors) > 0 {
		page.report(b.Errors[0].Error, b.Errors[0].Status)
		return page, nil
	}
	page.Err = "empty"
	return page, nil
}
