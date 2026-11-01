// Package finnhub adapts the Finnhub quote endpoint. Finnhub returns HTTP 200 with all-zero fields
// for an unknown symbol, so "unknown symbol" is a validation outcome, not a transport error.
package finnhub

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

const DefaultBaseURL = "https://finnhub.io/api/v1"

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

type Quote struct {
	Symbol        string
	Current       float64
	Change        float64
	ChangePercent float64
	High, Low     float64
	Open, PrevClose float64
	At            time.Time
}

var (
	ErrUnknownSymbol = errors.New("finnhub: unknown symbol")
	ErrRateLimited   = errors.New("finnhub: rate limited")
	ErrForbidden     = errors.New("finnhub: symbol not available on this plan")
)

func (a *Adapter) Quote(ctx context.Context, symbol string) (Quote, error) {
	if !a.Configured() {
		return Quote{}, providers.ErrNoCredential
	}
	symbol = strings.ToUpper(strings.TrimSpace(symbol))
	if symbol == "" || len(symbol) > 16 {
		return Quote{}, fmt.Errorf("finnhub: invalid symbol %q", symbol)
	}
	q := url.Values{}
	q.Set("symbol", symbol)
	resp, err := a.Client.Get(ctx, a.BaseURL+"/quote?"+q.Encode(), map[string]string{"X-Finnhub-Token": a.Cred.Value()})
	if err != nil {
		var pe *providers.Error
		if errors.As(err, &pe) {
			switch pe.Status {
			case 429:
				return Quote{}, ErrRateLimited
			case 403:
				return Quote{}, ErrForbidden
			}
		}
		return Quote{}, err
	}
	return ParseQuote(symbol, resp.ContentType, resp.Body)
}

func ParseQuote(symbol, contentType string, body []byte) (Quote, error) {
	obj, err := decode.Object(contentType, body)
	if err != nil {
		return Quote{}, err
	}
	if e := decode.Str(obj["error"]); e != "" {
		return Quote{}, fmt.Errorf("finnhub: %s", e)
	}
	c, okC := decode.Number(obj["c"])
	ts, _ := decode.Number(obj["t"])
	if !okC || (c == 0 && ts == 0) {
		return Quote{}, ErrUnknownSymbol
	}
	q := Quote{Symbol: symbol, Current: c}
	q.Change, _ = decode.Number(obj["d"])
	q.ChangePercent, _ = decode.Number(obj["dp"])
	q.High, _ = decode.Number(obj["h"])
	q.Low, _ = decode.Number(obj["l"])
	q.Open, _ = decode.Number(obj["o"])
	q.PrevClose, _ = decode.Number(obj["pc"])
	if ts > 0 {
		q.At = time.Unix(int64(ts), 0).UTC()
	}
	return q, nil
}
