package application

import (
	"context"
	"errors"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/currency"
	"masamong/internal/errcode"
	"masamong/internal/providers/fx"
)

// fxInputErrors are the message keys of a refusal of what the person typed. Such a result is an answer
// (the person is told how to write it), so a command never carries on from it as a conversation.
var fxInputErrors = map[string]bool{"fx.invalid_currency": true, "fx.invalid_amount": true, "fx.unsupported_currency": true}

// IsFXInputError reports whether res is finance.fx telling the person their currency or amount cannot be
// used. The Discord connector answers it privately.
func IsFXInputError(res action.Result) bool {
	return !res.OK && res.Action == "finance.fx" && res.Error != nil && res.Error.Code == errcode.InvalidInput && fxInputErrors[res.Error.MessageKey]
}

func (s *Service) fxInvalid(req action.Request, key string) action.Result {
	return action.Fail(req, errcode.InvalidInput, key, s.msg(req, key), nil)
}

// fxCurrency reads a currency field: absent gives the fallback, present must be a code or a name.
func (s *Service) fxCurrency(lex *currency.Lexicon, in map[string]any, key, fallback string) (string, bool) {
	v, given := in[key]
	if !given {
		return fallback, true
	}
	text, _ := v.(string)
	return lex.Resolve(text)
}

func (s *Service) financeFX(ctx context.Context, req action.Request, in map[string]any) action.Result {
	lex := currency.New(s.I18n)
	// what the person typed is checked before anything else: no provider call, no quota
	base, ok := s.fxCurrency(lex, in, "base", "USD")
	if !ok {
		return s.fxInvalid(req, "fx.invalid_currency")
	}
	var want []string
	if _, given := in["symbols"]; given {
		for _, name := range strSlice(in, "symbols") {
			if strings.TrimSpace(name) == "" {
				continue
			}
			code, ok := lex.Resolve(name)
			if !ok {
				return s.fxInvalid(req, "fx.invalid_currency")
			}
			want = append(want, code)
		}
	}
	if s.Registry().FX == nil {
		return action.OK(req, map[string]any{"status": "unconfigured", "live": false, "message": s.msg(req, "provider.unconfigured", "provider", "exchangerate")})
	}
	if err := s.Registry().reserve(s.Registry().fxID); err != nil {
		return s.providerFail(req, s.Registry().fxID, err)
	}
	rates, err := s.Registry().FX.Latest(ctx, base)
	if errors.Is(err, fx.ErrUnsupportedCurrency) {
		return s.fxInvalid(req, "fx.unsupported_currency")
	}
	if err != nil {
		return s.providerFail(req, s.Registry().fxID, err)
	}
	if len(want) == 0 {
		want = []string{"KRW", "JPY", "EUR", "CNY", "GBP"}
		if req.Invoker != nil && req.Invoker.Via == "command" && rates.Base != "USD" {
			want = append(want, "USD")
		}
	}
	sel := map[string]any{}
	for _, c := range want {
		if v, ok := rates.Rates[strings.ToUpper(c)]; ok {
			sel[strings.ToUpper(c)] = v
		}
	}
	out := map[string]any{"status": "ok", "live": true, "source": "exchangerate-api/" + rates.Source, "base": rates.Base, "rates": sel, "rate_count": len(rates.Rates)}
	if !rates.Updated.IsZero() {
		out["updated_at"] = rates.Updated.Format(time.RFC3339)
	}
	if !rates.NextUpdate.IsZero() {
		out["next_update_at"] = rates.NextUpdate.Format(time.RFC3339)
	}
	return action.OK(req, out)
}
