package application

import (
	"context"
	"encoding/json"
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
	if key == "fx.invalid_amount" { // says what an amount may be, in the way the locale writes it
		return action.Fail(req, errcode.InvalidInput, key, s.msg(req, key, "max", currency.MaxAmountText(req.Locale), "example", currency.AmountExample(req.Locale)), nil)
	}
	return action.Fail(req, errcode.InvalidInput, key, s.msg(req, key), nil)
}

// defaultFXSymbols are the rates reported when a request names none.
var defaultFXSymbols = []string{"KRW", "JPY", "EUR", "CNY", "GBP"}

// fxPair is one quote to state: amount (0: the unit the locale reads naturally) of from, in to.
type fxPair struct {
	from, to string
	amount   float64
}

// fxPlan is what a request asks for, read and checked before any rate is fetched.
type fxPlan struct {
	base    string   // the currency the rates are fetched for
	symbols []string // the rates reported
	pairs   []fxPair // the quotes stated
	strict  string   // a currency that must have a rate (the one the person named), else ""
}

// fxText is a text field of the input: absent is ok=false, present may be blank.
func fxText(in map[string]any, key string) (string, bool) {
	v, given := in[key]
	if !given {
		return "", false
	}
	switch t := v.(type) {
	case string:
		return t, true
	case json.Number:
		return t.String(), true
	}
	return "", true // present, but not text: it will not read as a currency
}

// fxAmount reads the optional amount: absent is 0, anything but a positive number is an error. A JSON
// number is taken as it is; text is read in the syntax of the request's locale (currency.ParseAmount),
// the same as the prefix command's words.
func fxAmount(locale string, in map[string]any) (float64, bool) {
	v, given := in["amount"]
	if !given || v == nil {
		return 0, true
	}
	var f float64
	switch t := v.(type) {
	case string:
		return currency.ParseAmount(locale, t)
	case json.Number:
		n, err := t.Float64()
		if err != nil {
			return 0, false
		}
		f = n
	case float64:
		f = t
	case int:
		f = float64(t)
	default:
		return 0, false
	}
	return f, currency.ValidAmount(f)
}

// fxPlanFor reads and checks what the person typed. A refusal comes back as the result to give; nothing
// has been fetched or counted by then.
func (s *Service) fxPlanFor(lex *currency.Lexicon, req action.Request, in map[string]any) (fxPlan, *action.Result) {
	amount, ok := fxAmount(req.Locale, in)
	if !ok {
		res := s.fxInvalid(req, "fx.invalid_amount")
		return fxPlan{}, &res
	}
	var plan fxPlan
	if _, home := in["home"]; home {
		plan, ok = s.fxHomePlan(lex, req.Locale, in, amount)
	} else {
		plan, ok = s.fxRawPlan(lex, in, amount)
	}
	if !ok {
		res := s.fxInvalid(req, "fx.invalid_currency")
		return fxPlan{}, &res
	}
	return plan, nil
}

// checkFXInput is the tool's input check (toolInputChecks): it runs before the per-user limits and the
// provider quota are counted, so a typo uses neither.
func (s *Service) checkFXInput(req action.Request) *action.Result {
	_, refused := s.fxPlanFor(currency.New(s.I18n), req, inputMap(req))
	return refused
}

func (s *Service) financeFX(ctx context.Context, req action.Request, in map[string]any) action.Result {
	lex := currency.New(s.I18n)
	plan, refused := s.fxPlanFor(lex, req, in)
	if refused != nil {
		return *refused
	}
	if s.Registry().FX == nil {
		return action.OK(req, map[string]any{"status": "unconfigured", "live": false, "message": s.msg(req, "provider.unconfigured", "provider", "exchangerate")})
	}
	if err := s.Registry().reserve(s.Registry().fxID); err != nil {
		return s.providerFail(req, s.Registry().fxID, err)
	}
	rates, err := s.Registry().FX.Latest(ctx, plan.base)
	if errors.Is(err, fx.ErrUnsupportedCurrency) {
		return s.fxInvalid(req, "fx.unsupported_currency")
	}
	if err != nil {
		return s.providerFail(req, s.Registry().fxID, err)
	}
	if _, has := rates.Rates[plan.strict]; plan.strict != "" && plan.strict != rates.Base && !has {
		return s.fxInvalid(req, "fx.unsupported_currency") // a real code the service has no rate for
	}
	sel := map[string]any{}
	for _, c := range plan.symbols {
		if v, ok := rates.Rates[c]; ok {
			sel[c] = v
		}
	}
	out := map[string]any{"status": "ok", "live": true, "source": "exchangerate-api/" + rates.Source, "base": rates.Base, "rates": sel, "rate_count": len(rates.Rates)}
	if !rates.Updated.IsZero() {
		out["updated_at"] = rates.Updated.Format(time.RFC3339)
	}
	if !rates.NextUpdate.IsZero() {
		out["next_update_at"] = rates.NextUpdate.Format(time.RFC3339)
	}
	quotes := fxQuotes(lex, req.Locale, rates, plan.pairs)
	out["quotes"] = quotes
	if len(quotes) > 0 {
		// the model that words the answer sees this result, not the tool's description
		out["answer_hint"] = s.msg(req, "fx.answer_hint")
	}
	return action.OK(req, out)
}

// fxRawPlan is a request by base and symbols (chat, tasks, the console, the CLI): what 1 of the base is
// in each symbol, quoted in the unit that reads naturally. Direct callers keep what they always had: a
// missing, blank or null base is USD, blank entries of symbols are skipped, and a list of nothing but
// blanks selects no rate (an empty list or none at all selects the default five). A name that is not a
// currency is an error. (Only a command's own option is invalid when blank: fxHomePlan.)
func (s *Service) fxRawPlan(lex *currency.Lexicon, in map[string]any, amount float64) (fxPlan, bool) {
	plan := fxPlan{base: "USD"}
	if text, _ := fxText(in, "base"); strings.TrimSpace(text) != "" {
		code, ok := lex.Resolve(text)
		if !ok {
			return plan, false
		}
		plan.base = code
	}
	onlyBlanks := false
	if _, given := in["symbols"]; given {
		listed := strSlice(in, "symbols")
		for _, name := range listed {
			if strings.TrimSpace(name) == "" {
				continue
			}
			code, ok := lex.Resolve(name)
			if !ok {
				return plan, false
			}
			plan.symbols = append(plan.symbols, code)
		}
		onlyBlanks = len(listed) > 0 && len(plan.symbols) == 0
	}
	if len(plan.symbols) == 0 && !onlyBlanks {
		plan.symbols = defaultFXSymbols
	}
	for _, c := range plan.symbols {
		if c != plan.base {
			plan.pairs = append(plan.pairs, fxPair{from: plan.base, to: c, amount: amount})
		}
	}
	return plan, true
}

// fxHomePlan is a request from a command: the person's home currency, and optionally a currency and an
// amount. The rates are fetched for the home currency, so one call serves both directions.
//
//	no currency (or the home one)  every board currency in home; with an amount, that much home in each
//	another currency               both directions; with an amount, that much of it in home
func (s *Service) fxHomePlan(lex *currency.Lexicon, locale string, in map[string]any, amount float64) (fxPlan, bool) {
	home := lex.Home(locale)
	if text, _ := fxText(in, "home"); text != "" {
		if code, ok := lex.Resolve(text); ok {
			home = code
		}
	}
	plan := fxPlan{base: home}
	other := ""
	if text, given := fxText(in, "currency"); given {
		code, ok := lex.Resolve(text)
		if !ok {
			return plan, false
		}
		other = code
	}
	if other == "" || other == home {
		for _, c := range currency.Board {
			if c != home {
				plan.symbols = append(plan.symbols, c)
			}
		}
		for _, c := range plan.symbols {
			if amount > 0 {
				plan.pairs = append(plan.pairs, fxPair{from: home, to: c, amount: amount})
			} else {
				plan.pairs = append(plan.pairs, fxPair{from: c, to: home})
			}
		}
		return plan, true
	}
	plan.symbols, plan.strict = []string{other}, other
	if amount > 0 {
		plan.pairs = []fxPair{{from: other, to: home, amount: amount}}
	} else {
		plan.pairs = []fxPair{{from: other, to: home}, {from: home, to: other}}
	}
	return plan, true
}

// fxQuotes states each pair from the fetched rates (units of a currency per one of the base).
func fxQuotes(lex *currency.Lexicon, locale string, rates fx.Rates, pairs []fxPair) []map[string]any {
	per := func(code string) (float64, bool) {
		if code == rates.Base {
			return 1, true
		}
		v, ok := rates.Rates[code]
		return v, ok && v > 0
	}
	out := []map[string]any{}
	for _, p := range pairs {
		from, ok1 := per(p.from)
		to, ok2 := per(p.to)
		if !ok1 || !ok2 {
			continue
		}
		if q, ok := lex.Quote(locale, p.from, p.to, to/from, p.amount); ok {
			out = append(out, map[string]any{"from": q.From, "unit": q.Unit, "to": q.To, "value": q.Value, "text": q.Text})
		}
	}
	return out
}
