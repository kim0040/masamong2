// Package fx adapts ExchangeRate-API. Two endpoints, two field names:
//   keyed  https://v6.exchangerate-api.com/v6/latest/{base} → "conversion_rates", Authorization: Bearer
//   open   https://open.er-api.com/v6/latest/{base}         → "rates", no key
// Both share {"result":"success"|"error","error-type":...}. We accept either field so a plan change
// on the provider side does not break the feature.
package fx

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

const (
	KeyedBase = "https://v6.exchangerate-api.com/v6"
	OpenBase  = "https://open.er-api.com/v6"
)

type Adapter struct {
	Client    *providers.Client
	KeyedBase string
	OpenBase  string
	Cred      providers.Credential
}

func New(client *providers.Client, cred providers.Credential) *Adapter {
	return &Adapter{Client: client, KeyedBase: KeyedBase, OpenBase: OpenBase, Cred: cred}
}

// Configured is always true: the open endpoint needs no key. Keyed reports whether the paid plan is in use.
func (a *Adapter) Configured() bool { return true }
func (a *Adapter) Keyed() bool      { return a.Cred.Present() }

type Rates struct {
	Base       string
	Rates      map[string]float64
	NextUpdate time.Time
	Updated    time.Time
	Source     string // keyed | open
}

var ErrProvider = errors.New("fx: provider error")

func (a *Adapter) Latest(ctx context.Context, base string) (Rates, error) {
	base = strings.ToUpper(strings.TrimSpace(base))
	if len(base) != 3 {
		return Rates{}, fmt.Errorf("fx: base currency must be a 3-letter code, got %q", base)
	}
	url := a.OpenBase + "/latest/" + base
	headers := map[string]string{}
	source := "open"
	if a.Keyed() {
		url = a.KeyedBase + "/latest/" + base
		headers["Authorization"] = "Bearer " + a.Cred.Value()
		source = "keyed"
	}
	resp, err := a.Client.Get(ctx, url, headers)
	if err != nil {
		var pe *providers.Error
		// Provider returns 4xx with a JSON body describing the error type; surface it.
		if errors.As(err, &pe) && resp != nil && pe.Status >= 400 && pe.Status < 500 {
			if r, perr := Parse(resp.ContentType, resp.Body); perr != nil {
				return r, perr
			}
		}
		return Rates{}, err
	}
	r, err := Parse(resp.ContentType, resp.Body)
	r.Source = source
	return r, err
}

// Parse validates the envelope and normalizes the rate map. Non-positive or non-numeric rates are dropped.
func Parse(contentType string, body []byte) (Rates, error) {
	obj, err := decode.Object(contentType, body)
	if err != nil {
		return Rates{}, err
	}
	if decode.Str(obj["result"]) != "success" {
		et := decode.Str(obj["error-type"])
		if et == "" {
			et = "unknown"
		}
		return Rates{}, fmt.Errorf("%w: %s", ErrProvider, et)
	}
	raw, ok := obj["conversion_rates"]
	if !ok {
		raw, ok = obj["rates"]
	}
	if !ok {
		return Rates{}, fmt.Errorf("%w: no rate map", decode.ErrUnexpected)
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return Rates{}, fmt.Errorf("%w: rate map: %v", decode.ErrUnexpected, err)
	}
	out := Rates{Base: strings.ToUpper(decode.Str(obj["base_code"])), Rates: map[string]float64{}}
	for code, v := range m {
		if f, ok := decode.Number(v); ok && f > 0 {
			out.Rates[strings.ToUpper(code)] = f
		}
	}
	if len(out.Rates) == 0 {
		return Rates{}, fmt.Errorf("%w: empty rate map", decode.ErrUnexpected)
	}
	if u, ok := decode.Number(obj["time_next_update_unix"]); ok {
		out.NextUpdate = time.Unix(int64(u), 0).UTC()
	}
	if u, ok := decode.Number(obj["time_last_update_unix"]); ok {
		out.Updated = time.Unix(int64(u), 0).UTC()
	}
	return out, nil
}
