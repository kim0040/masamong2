package websearch

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"time"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

// ---------------------------------------------------------------------------
// Serper (Google)
// ---------------------------------------------------------------------------

type serper struct {
	c    *providers.Client
	base string
	cred providers.Credential
}

func (s *serper) Kind() string                                          { return "serper" }
func (s *serper) Configured() bool                                      { return s.cred.Present() }
func (s *serper) Search(ctx context.Context, q Query) (Response, error) { return fanOut(ctx, q, s.one) }

func (s *serper) one(ctx context.Context, q Query) (Response, error) {
	body := map[string]any{"q": q.Text, "num": size(q.Max)}
	if hasHangul(q.Text) {
		body["gl"], body["hl"] = "kr", "ko"
	}
	switch q.Fresh {
	case "day":
		body["tbs"] = "qdr:d"
	case "week":
		body["tbs"] = "qdr:w"
	case "month":
		body["tbs"] = "qdr:m"
	}
	path := "/search"
	switch q.Topic {
	case TopicNews:
		path = "/news" // Google News: news[].title/link/snippet/date/source
	case TopicResearch:
		path = "/scholar"   // Google Scholar: organic[].title/link/snippet/publicationInfo/year
		delete(body, "tbs") // Scholar takes no recency window: the year filters below instead
	}
	obj, err := post(ctx, s.c, s.base+path, map[string]string{"X-API-KEY": s.cred.Value()}, body)
	if err != nil {
		return Response{}, err
	}
	switch path {
	case "/news":
		return ParseSerperNews(obj)
	case "/scholar":
		res, err := ParseSerperScholar(obj)
		if since, ok := freshSince(q.Fresh, time.Now()); ok && err == nil {
			res.Results = fromYear(res.Results, since.Year()) // recent papers only: from the year the window starts
		}
		return res, err
	}
	return ParseSerper(obj)
}

// ParseSerperNews reads {"news":[{title,link,snippet,date,source}]}; the outlet leads the snippet.
func ParseSerperNews(obj map[string]json.RawMessage) (Response, error) {
	var rows []struct {
		Title   string `json:"title"`
		Link    string `json:"link"`
		Snippet string `json:"snippet"`
		Date    string `json:"date"`
		Source  string `json:"source"`
	}
	if err := json.Unmarshal(obj["news"], &rows); err != nil {
		return Response{}, fmt.Errorf("%w: serper news", decode.ErrUnexpected)
	}
	var out Response
	for _, r := range rows {
		if r.Link == "" {
			continue
		}
		out.Results = append(out.Results, Result{Title: r.Title, URL: r.Link, Snippet: clip(lead(r.Source, r.Snippet)), Published: r.Date})
	}
	return out, nil
}

// ParseSerperScholar reads Google Scholar's {"organic":[{title,link,snippet,publicationInfo,year,citedBy}]};
// the authors and venue lead the snippet and the year stands for the date.
func ParseSerperScholar(obj map[string]json.RawMessage) (Response, error) {
	var rows []struct {
		Title           string `json:"title"`
		Link            string `json:"link"`
		Snippet         string `json:"snippet"`
		PublicationInfo string `json:"publicationInfo"`
		Year            int    `json:"year"`
	}
	if err := json.Unmarshal(obj["organic"], &rows); err != nil {
		return Response{}, fmt.Errorf("%w: serper scholar", decode.ErrUnexpected)
	}
	var out Response
	for _, r := range rows {
		if r.Link == "" {
			continue
		}
		res := Result{Title: r.Title, URL: r.Link, Snippet: clip(lead(r.PublicationInfo, r.Snippet))}
		if r.Year > 0 {
			res.Published = strconv.Itoa(r.Year)
		}
		out.Results = append(out.Results, res)
	}
	return out, nil
}

// fromYear keeps the papers of a year or later (Published holds the year; a paper without one is kept).
func fromYear(results []Result, year int) []Result {
	kept := results[:0:0]
	for _, r := range results {
		if y, err := strconv.Atoi(r.Published); err == nil && y < year {
			continue
		}
		kept = append(kept, r)
	}
	return kept
}

// ParseSerper reads {"answerBox":{answer|snippet}, "organic":[{title,link,snippet,date}]}.
func ParseSerper(obj map[string]json.RawMessage) (Response, error) {
	raw, ok := obj["organic"]
	if !ok {
		return Response{}, fmt.Errorf("%w: serper organic", decode.ErrUnexpected)
	}
	var rows []struct {
		Title   string `json:"title"`
		Link    string `json:"link"`
		Snippet string `json:"snippet"`
		Date    string `json:"date"`
	}
	if err := json.Unmarshal(raw, &rows); err != nil {
		return Response{}, fmt.Errorf("%w: serper organic", decode.ErrUnexpected)
	}
	var out Response
	var box struct {
		Answer  string `json:"answer"`
		Snippet string `json:"snippet"`
	}
	if json.Unmarshal(obj["answerBox"], &box) == nil {
		out.Answer = clip(box.Answer)
		if out.Answer == "" {
			out.Answer = clip(box.Snippet)
		}
	}
	for _, r := range rows {
		if r.Link != "" {
			out.Results = append(out.Results, Result{Title: r.Title, URL: r.Link, Snippet: clip(r.Snippet), Published: r.Date})
		}
	}
	return out, nil
}
