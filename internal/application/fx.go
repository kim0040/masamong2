package application

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"time"

	"golang.org/x/text/language"

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
		maxText, example := currency.MaxAmountText(req.Locale), currency.AmountExample(req.Locale)
		return action.Fail(req, errcode.InvalidInput, key, s.msg(req, key, "max", maxText, "example", example), map[string]any{"max": maxText, "example": example})
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
	shared  bool     // the pairs without an amount share one unit of the same "from" (one currency against several)
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
// number is taken as it is; text a person typed (human) is read in the syntax of the request's locale
// (currency.ParseAmount), text a program sent in the locale-free form (currency.ParseMachineAmount).
func fxAmount(locale string, human bool, in map[string]any) (float64, bool) {
	v, given := in["amount"]
	if !given || v == nil {
		return 0, true
	}
	var f float64
	switch t := v.(type) {
	case string:
		if !human {
			return currency.ParseMachineAmount(t)
		}
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
	// words a person typed (a Discord prefix command) are read in their locale's way of writing numbers;
	// what a program sent (a chat tool call, the CLI, the console) in the one locale-free form
	human := req.Invoker != nil && req.Invoker.Via == "command"
	amount, ok := fxAmount(req.Locale, human, in)
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
	quotes := fxQuotes(lex, req.Locale, rates, plan.pairs, plan.shared)
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
	switch in["base"].(type) {
	case nil, string, json.Number:
	default:
		return plan, false // a base that is not text (false, an object) was refused before, and still is
	}
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

// fxHomeTag is the locale tag the home currency is read from. The first tag that names a region wins:
// the person's saved language when it names one ("en-US" is a choice of theirs), else the tag Discord
// sent (the interaction's for a slash command, the server's for a message), else the saved language for
// its likely region. Home then reads the tag: a region with no single currency (es-419) gives USD.
func fxHomeTag(locale string, in map[string]any) string {
	tag, _ := fxText(in, "locale_tag")
	if !namesRegion(locale) && namesRegion(tag) {
		return tag
	}
	return locale
}

// namesRegion reports whether tag itself names a region ("en-GB" does; "en" only has a likely one).
func namesRegion(tag string) bool {
	_, conf := language.Make(strings.TrimSpace(tag)).Region()
	return conf == language.Exact
}

// fxHomePlan is a request from a command: the person's home currency (the currency of their region), and
// optionally a currency, a base and an amount.
//
//	nothing (or the home currency)    the board valued in home: every board currency, one unit of each in
//	                                  home; with an amount, that much home in each
//	base                              the same, valued in the base
//	currency                          one unit of it in home first, then each board currency; with an
//	                                  amount, that much of it in each
//	currency and base                 the two against each other, both directions; with an amount, that
//	                                  much of the currency in the base
//
// The currency being shown is never listed against itself, and a currency that is the base (or the home
// currency when there is no base) is the same as none. The rates are fetched for the one currency the
// others are stated against, so one call serves a card.
func (s *Service) fxHomePlan(lex *currency.Lexicon, locale string, in map[string]any, amount float64) (fxPlan, bool) {
	home := lex.Home(fxHomeTag(locale, in))
	if text, _ := fxText(in, "home"); text != "" {
		if code, ok := lex.Resolve(text); ok {
			home = code
		}
	}
	base, other := "", ""
	if text, given := fxText(in, "base"); given {
		if _, isText := in["base"].(string); !isText && in["base"] != nil {
			return fxPlan{}, false // a base that is not text is refused, as in fxRawPlan
		}
		if strings.TrimSpace(text) != "" {
			code, ok := lex.Resolve(text)
			if !ok {
				return fxPlan{}, false
			}
			base = code
		}
		// an absent, empty or whitespace-only base is the home currency's default, not an error
	}
	if text, given := fxText(in, "currency"); given {
		code, ok := lex.Resolve(text)
		if !ok {
			return fxPlan{}, false
		}
		other = code
	}
	board := currency.BoardFor(home)
	if other != "" && base != "" && other != base {
		// the pair, both directions
		plan := fxPlan{base: base, symbols: []string{other}, strict: other}
		if amount > 0 {
			plan.pairs = []fxPair{{from: other, to: base, amount: amount}}
		} else {
			plan.pairs = []fxPair{{from: other, to: base}, {from: base, to: other}}
		}
		return plan, true
	}
	if base == "" && other != "" && other != home {
		// one currency against the home currency and the others, the home currency first
		plan := fxPlan{base: other, strict: other, shared: true}
		for _, c := range append([]string{home}, board...) {
			if c != other && !slices.Contains(plan.symbols, c) {
				plan.symbols = append(plan.symbols, c)
			}
		}
		for _, c := range plan.symbols {
			plan.pairs = append(plan.pairs, fxPair{from: other, to: c, amount: amount})
		}
		return plan, true
	}
	// the board valued in the base, else in home
	valued := home
	if base != "" {
		valued = base
	}
	plan := fxPlan{base: valued}
	for _, c := range board {
		if c != valued {
			plan.symbols = append(plan.symbols, c)
		}
	}
	for _, c := range plan.symbols {
		if amount > 0 {
			plan.pairs = append(plan.pairs, fxPair{from: valued, to: c, amount: amount})
		} else {
			plan.pairs = append(plan.pairs, fxPair{from: c, to: valued})
		}
	}
	return plan, true
}

// fxQuotes states each pair from the fetched rates (units of a currency per one of the base).
func fxQuotes(lex *currency.Lexicon, locale string, rates fx.Rates, pairs []fxPair, shared bool) []map[string]any {
	per := func(code string) (float64, bool) {
		if code == rates.Base {
			return 1, true
		}
		v, ok := rates.Rates[code]
		return v, ok && v > 0
	}
	out := []map[string]any{}
	unit := 0.0 // shared: one unit of the one currency for every line, the largest value at least 1
	if shared && len(pairs) > 0 && pairs[0].amount == 0 {
		var all []float64
		for _, p := range pairs {
			if from, ok1 := per(p.from); ok1 {
				if to, ok2 := per(p.to); ok2 {
					all = append(all, to/from)
				}
			}
		}
		unit = lex.SharedUnit(locale, pairs[0].from, all)
	}
	for _, p := range pairs {
		from, ok1 := per(p.from)
		to, ok2 := per(p.to)
		if !ok1 || !ok2 {
			continue
		}
		amount := p.amount
		if unit > 0 {
			amount = unit
		}
		if q, ok := lex.Quote(locale, p.from, p.to, to/from, amount); ok {
			out = append(out, map[string]any{"from": q.From, "unit": q.Unit, "to": q.To, "value": q.Value, "text": q.Text})
		}
	}
	return out
}
