package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

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
