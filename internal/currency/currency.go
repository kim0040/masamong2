// Package currency reads what people type for a currency (an ISO 4217 code or a name in any loaded
// language) and writes a rate the way a person would say it: "100엔 ≈ 930.2원", "10,000 KRW ≈ 7.2 USD".
//
// Everything about a language lives in the message catalogs, never here:
//
//	fx.currency.<CODE>  the names of one currency, comma separated; the first is the display name
//	fx.unit.<CODE>      the smallest unit a locale quotes the currency in (ko: 100 for JPY)
//	fx.home             the currency a locale's members think in (ko: KRW)
//	fx.amount.named     how a number and a display name read together
//	fx.amount.coded     the same for a currency without a name in that locale
package currency

import (
	"math"
	"strconv"
	"strings"

	xcur "golang.org/x/text/currency"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"golang.org/x/text/number"

	"masamong/internal/i18n"
)

const (
	namePrefix = "fx.currency."
	unitPrefix = "fx.unit."
	// maxUnit stops the unit search for a rate so small that no power of ten reaches 1.
	maxUnit = 1e15
	// MaxAmount is the largest amount a person may ask about.
	MaxAmount = 1e15
	// significant is the number of significant digits a converted value is shown with.
	significant = 4
)

// Board is the currencies the bare command compares with the home currency.
var Board = []string{"USD", "JPY", "EUR", "CNY", "GBP", "KRW"}

// Lexicon is the currency data of a message catalog.
type Lexicon struct{ cat *i18n.Catalog }

// New reads currency data from cat.
func New(cat *i18n.Catalog) *Lexicon { return &Lexicon{cat: cat} }

// ValidCode reports whether code is an ISO 4217 currency code, without asking any service.
func ValidCode(code string) bool {
	code = strings.TrimSpace(code)
	if len(code) != 3 {
		return false
	}
	for _, c := range code {
		if !(c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z') {
			return false
		}
	}
	_, err := xcur.ParseISO(strings.ToUpper(code))
	return err == nil
}

// fold is how names are compared: case and spaces do not matter ("미국 달러" is "미국달러").
func fold(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	return strings.Map(func(r rune) rune {
		switch r {
		case ' ', '\t', '-', '·', ' ':
			return -1
		}
		return r
	}, s)
}

func names(list string) []string {
	var out []string
	for _, n := range strings.Split(list, ",") {
		if n = strings.TrimSpace(n); n != "" {
			out = append(out, n)
		}
	}
	return out
}

// Resolve turns what a person typed (USD, usd, 달러, 엔화, euro, 人民元 …) into a currency code.
func (l *Lexicon) Resolve(text string) (string, bool) {
	t := strings.TrimSpace(text)
	if t == "" {
		return "", false
	}
	if len(t) == 3 && ValidCode(t) {
		return strings.ToUpper(t), true
	}
	key := fold(t)
	for _, loc := range l.cat.Locales() {
		for code, list := range l.cat.Prefixed(loc, namePrefix) {
			if !ValidCode(code) {
				continue
			}
			for _, n := range names(list) {
				if fold(n) == key {
					return strings.ToUpper(code), true
				}
			}
		}
	}
	return "", false
}

// Name is the display name of code in locale ("" when the locale has none: show the code).
func (l *Lexicon) Name(locale, code string) string {
	list, _ := l.cat.Lookup(locale, namePrefix+code)
	if all := names(list); len(all) > 0 {
		return all[0]
	}
	return ""
}

// Home is the currency a locale's members think in: the one its catalog names, else USD.
func (l *Lexicon) Home(locale string) string {
	if v, ok := l.cat.Lookup(locale, "fx.home"); ok && ValidCode(v) {
		return strings.ToUpper(strings.TrimSpace(v))
	}
	return "USD"
}

// convention is the smallest unit locale quotes code in (1 unless its catalog says otherwise).
func (l *Lexicon) convention(locale, code string) float64 {
	if v, ok := l.cat.Lookup(locale, unitPrefix+code); ok {
		if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && n >= 1 && n <= maxUnit {
			return n
		}
	}
	return 1
}

// Quote is one rate in words: Unit of From is about Value of To.
type Quote struct {
	From  string  `json:"from"`
	Unit  float64 `json:"unit"`
	To    string  `json:"to"`
	Value float64 `json:"value"`
	Text  string  `json:"text"`
}

// Quote states what unit of from is worth in to, given rate (units of to per one of from). With amount
// above zero the unit is that amount. Without one the unit is the locale's convention for from or, when
// that gives less than 1 of to, the smallest power of ten (1, 10, 100 …) that gives at least 1.
func (l *Lexicon) Quote(locale, from, to string, rate, amount float64) (Quote, bool) {
	if !(rate > 0) || math.IsInf(rate, 0) || math.IsNaN(rate) {
		return Quote{}, false
	}
	unit := amount
	if !(unit > 0) {
		unit = l.convention(locale, from)
		for unit*rate < 1-1e-12 && unit < maxUnit {
			unit *= 10
		}
	}
	value := roundSignificant(unit * rate)
	p := message.NewPrinter(language.Make(locale))
	return Quote{
		From: from, Unit: unit, To: to, Value: value,
		Text: l.text(locale, p, from, unit) + " ≈ " + l.text(locale, p, to, value),
	}, true
}

func (l *Lexicon) text(locale string, p *message.Printer, code string, v float64) string {
	// the shortest decimal that reads back as v: no float noise, no digit dropped
	shortest := strconv.FormatFloat(v, 'f', -1, 64)
	fraction := 0
	if dot := strings.IndexByte(shortest, '.'); dot >= 0 {
		fraction = len(shortest) - dot - 1
	}
	n := p.Sprint(number.Decimal(v, number.MaxFractionDigits(fraction)))
	if name := l.Name(locale, code); name != "" {
		return l.cat.T(locale, "fx.amount.named", "value", n, "name", name)
	}
	return l.cat.T(locale, "fx.amount.coded", "value", n, "code", code)
}

// roundSignificant keeps the first four significant digits: 1350.4 → 1350, 930.23 → 930.2.
func roundSignificant(v float64) float64 {
	r, err := strconv.ParseFloat(strconv.FormatFloat(v, 'e', significant-1, 64), 64)
	if err != nil {
		return v
	}
	return r
}

// separators are the group and decimal separators of locale as x/text writes them (en "," and ".",
// es "." and ","): read off a number that has both.
func separators(locale string) (group, decimal string) {
	text := message.NewPrinter(language.Make(locale)).Sprint(number.Decimal(1234567.5, number.MaxFractionDigits(1)))
	var runs []string // the non-digit runs: group, group, decimal
	cur := ""
	for _, r := range text {
		if r >= '0' && r <= '9' {
			if cur != "" {
				runs = append(runs, cur)
				cur = ""
			}
			continue
		}
		cur += string(r)
	}
	if len(runs) < 2 {
		return ",", "."
	}
	return runs[0], runs[len(runs)-1]
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// ParseAmount reads the number a person typed, in the syntax of locale: digits, optional thousands groups
// of exactly three digits joined by the locale's group separator, then optionally the locale's decimal
// separator and digits ("1,234.5" in English, "1.234,5" in Spanish). Anything else (".5", "1,5" where the
// comma is a group, "5,00,0", a sign, an exponent) is not an amount, and neither is a zero or negative
// number or one beyond MaxAmount.
func ParseAmount(locale, text string) (float64, bool) {
	t := strings.TrimSpace(text)
	group, decimal := separators(locale)
	whole, fraction := t, ""
	if i := strings.Index(t, decimal); i >= 0 {
		whole, fraction = t[:i], t[i+len(decimal):]
		if !isDigits(fraction) {
			return 0, false
		}
	}
	digits := whole
	if strings.Contains(whole, group) {
		parts := strings.Split(whole, group)
		for i, part := range parts {
			if i == 0 && (len(part) < 1 || len(part) > 3) || i > 0 && len(part) != 3 || !isDigits(part) {
				return 0, false
			}
		}
		digits = strings.Join(parts, "")
	}
	if !isDigits(digits) {
		return 0, false
	}
	if fraction != "" {
		digits += "." + fraction
	}
	n, err := strconv.ParseFloat(digits, 64)
	if err != nil || !ValidAmount(n) {
		return 0, false
	}
	return n, true
}

// MaxAmountText is MaxAmount written for locale; AmountExample shows how a locale writes an amount.
func MaxAmountText(locale string) string {
	return message.NewPrinter(language.Make(locale)).Sprint(number.Decimal(MaxAmount))
}

// AmountExample is a sample amount in locale's syntax ("1,234.5"), for the message that refuses one.
func AmountExample(locale string) string {
	return message.NewPrinter(language.Make(locale)).Sprint(number.Decimal(1234.5, number.MaxFractionDigits(1)))
}

// ValidAmount reports whether n is an amount a person may ask about: above zero and not beyond MaxAmount.
func ValidAmount(n float64) bool { return n > 0 && n <= MaxAmount }

// IsNumber reports whether a word of a command is written as a number, an amount to be read by
// ParseAmount: it holds digits and nothing but the "." and "," every supported locale uses as separators
// (a test pins that), so a malformed amount ("5,00,0", ".5") is still an amount, refused as such, and
// never taken for a currency name.
func IsNumber(text string) bool {
	t := strings.TrimSpace(text)
	digit := false
	for _, r := range t {
		switch {
		case r >= '0' && r <= '9':
			digit = true
		case r == '.' || r == ',':
		default:
			return false
		}
	}
	return digit
}
