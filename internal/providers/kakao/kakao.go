// Package kakao adapts Kakao Developers search (dapi.kakao.com/v2/search/*). Responses share
// {"documents":[...],"meta":{"total_count","pageable_count","is_end"}}; per-endpoint document fields differ.
package kakao

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

const DefaultBaseURL = "https://dapi.kakao.com"

type Adapter struct {
	Client  *providers.Client
	BaseURL string
	Cred    providers.Credential
}

func New(client *providers.Client, baseURL string, cred providers.Credential) *Adapter {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Adapter{Client: client, BaseURL: strings.TrimRight(baseURL, "/"), Cred: cred}
}

func (a *Adapter) Configured() bool { return a.Cred.Present() }

type Document struct {
	Title    string `json:"title"`
	Contents string `json:"contents"`
	URL      string `json:"url"`
	Datetime string `json:"datetime"`
}

type Result struct {
	Documents  []Document
	TotalCount int
	IsEnd      bool
}

var ErrQuotaExceeded = errors.New("kakao: quota exceeded")

func (a *Adapter) SearchWeb(ctx context.Context, query string, size int) (Result, error) {
	if !a.Configured() {
		return Result{}, providers.ErrNoCredential
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return Result{}, fmt.Errorf("kakao: empty query")
	}
	if size < 1 || size > 50 {
		size = 5
	}
	q := url.Values{}
	q.Set("query", query)
	q.Set("size", strconv.Itoa(size))
	resp, err := a.Client.Get(ctx, a.BaseURL+"/v2/search/web?"+q.Encode(), map[string]string{"Authorization": "KakaoAK " + a.Cred.Value()})
	if err != nil {
		var pe *providers.Error
		if errors.As(err, &pe) && pe.Status == 429 {
			return Result{}, ErrQuotaExceeded
		}
		return Result{}, err
	}
	return Parse(resp.ContentType, resp.Body)
}

func Parse(contentType string, body []byte) (Result, error) {
	obj, err := decode.Object(contentType, body)
	if err != nil {
		return Result{}, err
	}
	if e := decode.Str(obj["errorType"]); e != "" {
		return Result{}, fmt.Errorf("kakao: %s: %s", e, decode.Str(obj["message"]))
	}
	raw, ok := obj["documents"]
	if !ok {
		return Result{}, fmt.Errorf("%w: no documents", decode.ErrUnexpected)
	}
	var docs []Document
	if err := json.Unmarshal(raw, &docs); err != nil {
		return Result{}, fmt.Errorf("%w: documents: %v", decode.ErrUnexpected, err)
	}
	out := Result{}
	for _, d := range docs {
		if d.URL == "" {
			continue
		}
		d.Title = stripTags(d.Title)
		d.Contents = stripTags(d.Contents)
		out.Documents = append(out.Documents, d)
	}
	if m, ok := obj["meta"]; ok {
		var meta struct {
			TotalCount int  `json:"total_count"`
			IsEnd      bool `json:"is_end"`
		}
		_ = json.Unmarshal(m, &meta)
		out.TotalCount, out.IsEnd = meta.TotalCount, meta.IsEnd
	}
	return out, nil
}

// stripTags removes Kakao's <b> highlight markup; content is rendered as text, never as HTML.
func stripTags(s string) string {
	var b strings.Builder
	in := false
	for _, r := range s {
		switch {
		case r == '<':
			in = true
		case r == '>':
			in = false
		case !in:
			b.WriteRune(r)
		}
	}
	return strings.TrimSpace(b.String())
}

// Place is one result of Kakao Local's keyword search (Kakao Map).
type Place struct {
	Name        string `json:"place_name"`
	Category    string `json:"category_name"`
	Address     string `json:"address_name"`
	RoadAddress string `json:"road_address_name"`
	Phone       string `json:"phone"`
	URL         string `json:"place_url"`
	Distance    string `json:"distance"` // metres from x/y when given
}

// Places is a keyword search's result.
type Places struct {
	Places []Place
	// Region is the area Kakao read from the query ("서울 강남구 강남역"); "" means the query named no
	// area and the places are from anywhere in the country.
	Region string
}

// Places searches Kakao Map for places matching query ("강남역 맛집"): GET /v2/local/search/keyword.json
// with the same REST key as the web search (developers.kakao.com local dev-guide, read 2026-10-01).
func (a *Adapter) Places(ctx context.Context, query string, size int) (Places, error) {
	if !a.Configured() {
		return Places{}, providers.ErrNoCredential
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return Places{}, fmt.Errorf("kakao: empty query")
	}
	if size < 1 || size > 15 {
		size = 5
	}
	q := url.Values{}
	q.Set("query", query)
	q.Set("size", strconv.Itoa(size))
	resp, err := a.Client.Get(ctx, a.BaseURL+"/v2/local/search/keyword.json?"+q.Encode(), map[string]string{"Authorization": "KakaoAK " + a.Cred.Value()})
	if err != nil {
		var pe *providers.Error
		if errors.As(err, &pe) && pe.Status == 429 {
			return Places{}, ErrQuotaExceeded
		}
		return Places{}, err
	}
	return ParsePlaces(resp.ContentType, resp.Body)
}

// ParsePlaces reads documents[] of a keyword search, and meta.same_name.selected_region for the area
// Kakao matched; a place without a name is dropped.
func ParsePlaces(contentType string, body []byte) (Places, error) {
	obj, err := decode.Object(contentType, body)
	if err != nil {
		return Places{}, err
	}
	if e := decode.Str(obj["errorType"]); e != "" {
		return Places{}, fmt.Errorf("kakao: %s: %s", e, decode.Str(obj["message"]))
	}
	raw, ok := obj["documents"]
	if !ok {
		return Places{}, fmt.Errorf("%w: no documents", decode.ErrUnexpected)
	}
	var docs []Place
	if err := json.Unmarshal(raw, &docs); err != nil {
		return Places{}, fmt.Errorf("%w: documents: %v", decode.ErrUnexpected, err)
	}
	out := Places{Places: docs[:0]}
	for _, d := range docs {
		if strings.TrimSpace(d.Name) != "" {
			out.Places = append(out.Places, d)
		}
	}
	if m, ok := obj["meta"]; ok {
		var meta struct {
			SameName struct {
				Selected string `json:"selected_region"`
			} `json:"same_name"`
		}
		_ = json.Unmarshal(m, &meta)
		out.Region = strings.TrimSpace(meta.SameName.Selected)
	}
	return out, nil
}
