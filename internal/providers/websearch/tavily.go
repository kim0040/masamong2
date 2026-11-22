package websearch

import (
	"context"
	"encoding/json"
	"fmt"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

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
