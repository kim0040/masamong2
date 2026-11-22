package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

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
