package websearch

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"strings"

	"masamong/internal/providers"
	"masamong/internal/providers/kakao"
)

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
		if MaybeCharged(err) {
			b.res.Lost++ // the web request's answer was lost after it may have been charged
		}
		return b.res, nil
	case b.err != nil:
		web.Requests = 1
		if MaybeCharged(b.err) {
			web.Lost++ // the blog request's answer was lost after it may have been charged
		}
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
			return Response{}, &authError{pe: pe}
		}
		return Response{}, err
	}
	var out Response
	for _, doc := range res.Documents {
		out.Results = append(out.Results, Result{Title: doc.Title, URL: doc.URL, Snippet: clip(doc.Contents), Published: doc.Datetime})
	}
	return out, nil
}
