// Package currency reads what people type for a currency (an ISO 4217 code or a name in any loaded
// language) and writes a rate the way a person would say it: "100엔 ≈ 930.2원", "10,000 KRW ≈ 7.2 USD".
//
// Everything about a language lives in the message catalogs, never here:
//
//	fx.currency.<CODE>  the names of one currency, comma separated; the first is the display name
//	fx.unit.<CODE>      the smallest unit a locale quotes the currency in (ko: 100 for JPY)
//	(the home currency is not catalog data: it is the currency of the region the locale tag names, Home)
//	fx.amount.named     how a number and a display name read together
//	fx.amount.coded     the same for a currency without a name in that locale
package currency

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unicode/utf8"

	xcur "golang.org/x/text/currency"
	"golang.org/x/text/language"
	"golang.org/x/text/message"
	"golang.org/x/text/number"

	"masamong/internal/i18n"
)

const (
	namePrefix = "fx.currency."
	unitPrefix = "fx.unit."
	// maxUnit is the largest unit the search tries (10^16: the cap on an amount is 10^15, and a rate that
	// needs one more power of ten than that to reach 1 is still stated). A rate so small that even this unit
	// stays under 1 is stated in this unit, as the fraction it is.
	maxUnit = 1e16
	// MaxAmount is the largest amount a person may ask about.
	MaxAmount = 1e15
	// significant is the number of significant digits a converted value is shown with.
	significant = 4
)

// Board is the six managed currencies every comparison offers in every locale; the board never loses
// one of them.
var Board = []string{"USD", "KRW", "JPY", "EUR", "CNY", "GBP"}

// BoardFor is the currencies a card compares: the home currency first, then the managed six it is not
// among (all six when home is none of them).
func BoardFor(home string) []string {
	out := []string{home}
	for _, c := range Board {
		if c != home {
			out = append(out, c)
		}
	}
	return out
}

// Lexicon is the currency data of a message catalog.
type Lexicon struct{ cat *i18n.Catalog }

// New reads currency data from cat.
func New(cat *i18n.Catalog) *Lexicon { return &Lexicon{cat: cat} }

// nameIndex is a catalog's folded name -> code map for every name every locale lists, with the folded
// length of the longest name. It is built once per catalog (they can be swapped for a bundle) and is
// read-only after.
type nameIndex struct {
	byName  map[string]string
	maxName int // runes in the longest folded name: more typed words than this cannot be one name
}

var nameIndexes sync.Map // *i18n.Catalog -> *nameIndex

func (l *Lexicon) index() *nameIndex {
	if v, ok := nameIndexes.Load(l.cat); ok {
		return v.(*nameIndex)
	}
	idx := &nameIndex{byName: map[string]string{}, maxName: 3}
	for _, loc := range l.cat.Locales() {
		pre := l.cat.Prefixed(loc, namePrefix)
		codes := make([]string, 0, len(pre))
		for code := range pre {
			codes = append(codes, code)
		}
		sort.Strings(codes)
		for _, code := range codes {
			if !ValidCode(code) {
				continue
			}
			for _, n := range names(pre[code]) {
				key := fold(n)
				if key == "" {
					continue
				}
				if _, taken := idx.byName[key]; !taken {
					idx.byName[key] = strings.ToUpper(code)
				}
				if w := utf8.RuneCountInString(key); w > idx.maxName {
					idx.maxName = w
				}
			}
		}
	}
	actual, _ := nameIndexes.LoadOrStore(l.cat, idx)
	return actual.(*nameIndex)
}

// MaxNameWords bounds how many typed words one currency name is read as: a side of a split longer
// than the longest admissible name is never asked of Resolve, so a long unknown input stays one name.
func (l *Lexicon) MaxNameWords() int { return l.index().maxName }

// providerCodes is every currency code ExchangeRate-API publishes it serves rates for
// (https://www.exchangerate-api.com/docs/supported-currencies, checked 2026-11-20). Nine of them —
// FOK, GGP, IMP, JEP, KID, MRU, SLE, TVD and VES — are not ISO 4217 codes and are accepted anyway.
var providerCodes = func() map[string]bool {
	out := map[string]bool{}
	for _, c := range strings.Fields(providerCodeList) {
		out[c] = true
	}
	return out
}()

const providerCodeList = `AED AFN ALL AMD ANG AOA ARS AUD AWG AZN BAM BBD BDT BGN BHD BIF BMD BND BOB
BRL BSD BTN BWP BYN BZD CAD CDF CHF CLF CLP CNH CNY COP CRC CUP CVE CZK DJF DKK DOP DZD EGP ERN ETB
EUR FJD FKP FOK GBP GEL GGP GHS GIP GMD GNF GTQ GYD HKD HNL HRK HTG HUF IDR ILS IMP INR IQD IRR ISK
JEP JMD JOD JPY KES KGS KHR KID KMF KRW KWD KYD KZT LAK LBP LKR LRD LSL LYD MAD MDL MGA MKD MMK MNT
MOP MRU MUR MVR MWK MXN MYR MZN NAD NGN NIO NOK NPR NZD OMR PAB PEN PGK PHP PKR PLN PYG QAR RON RSD
RUB RWF SAR SBD SCR SDG SEK SGD SHP SLE SOS SRD SSP STN SYP SZL THB TJS TMT TND TOP TRY TTD TVD TWD
TZS UAH UGX USD UYU UZS VES VND VUV WST XAF XCD XDR XOF XPF YER ZAR ZMW ZWL`

// obsoleteCodes are currency codes CLDR still gives a region that the rate provider lists under a
// newer code (Mauritania's and Sierra Leone's redenominations, Venezuela's). A region whose code has
// no provider replacement, like North Korea's KPW, falls back to USD instead.
var obsoleteCodes = map[string]string{"MRO": "MRU", "SLL": "SLE", "VEF": "VES"}

// ValidCode reports whether code is a currency code a rate request may ask for: an ISO 4217 code, or
// one the rate provider lists that is not ISO, without asking any service.
func ValidCode(code string) bool {
	code = strings.ToUpper(strings.TrimSpace(code))
	if len(code) != 3 {
		return false
	}
	for _, c := range code {
		if !(c >= 'A' && c <= 'Z') {
			return false
		}
	}
	if _, err := xcur.ParseISO(code); err == nil {
		return true
	}
	return providerCodes[code]
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
	code, ok := l.index().byName[fold(t)]
	return code, ok
}

// Name is the display name of code in locale ("" when the locale has none: show the code).
func (l *Lexicon) Name(locale, code string) string {
	list, _ := l.cat.Lookup(locale, namePrefix+code)
	if all := names(list); len(all) > 0 {
		return all[0]
	}
	return ""
}

// Home is the currency of the region a locale tag names: its explicit region (ko-KR, en-GB, pt-BR), else
// the likely region of its language (ko: KR, ja: JP, es: ES). The code CLDR gives may predate a
// redenomination (MRO, SLL, VEF) and is read as the code the rate provider lists. A region that is not a
// country (es-419), one with no single currency, or one whose currency the provider does not serve
// (KPW) gives USD.
func (l *Lexicon) Home(locale string) string {
	if code, ok := l.HomeOf(locale); ok {
		return code
	}
	return "USD"
}

// HomeOf is Home without the USD fallback: ok is false when the tag's region is not a country with a
// single currency the provider serves, so a caller can try its next tag.
func (l *Lexicon) HomeOf(locale string) (string, bool) {
	region, _ := language.Make(strings.ReplaceAll(strings.TrimSpace(locale), "_", "-")).Region()
	if unit, ok := xcur.FromRegion(region); ok {
		code := unit.String()
		if replacement, ok := obsoleteCodes[code]; ok {
			code = replacement
		}
		if providerCodes[code] {
			return code, true
		}
	}
	return "", false
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

// SharedUnit is the one unit of from that states every rate (units of a currency per one of from) in a
// list: the locale's convention for from, made ten times larger while even the largest value would still
// be under 1 ("1 USD ≈ 0.92 EUR / 150 JPY" keeps 1, 1 KRW becomes 10 or 1,000 when its yen are few).
func (l *Lexicon) SharedUnit(locale, from string, rates []float64) float64 {
	best := 0.0
	for _, r := range rates {
		if r > best && !math.IsInf(r, 0) {
			best = r
		}
	}
	unit := l.convention(locale, from)
	for best > 0 && unit*best < 1-1e-12 && unit < maxUnit {
		unit *= 10
	}
	return unit
}

// Quote states what unit of from is worth in to, given rate (units of to per one of from). With amount
// above zero the unit is that amount. A product that overflows gives no quote. Without one the unit is the locale's convention for from or, when
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
	if math.IsInf(value, 0) || math.IsNaN(value) || math.IsInf(unit, 0) {
		return Quote{}, false // an overflow is no quote: it could not be written, nor sent as JSON
	}
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

// ParseMachineAmount reads an amount a program wrote (a chat tool call, the CLI, the console), the same in
// every locale: digits, optionally grouped by "," in threes ("50,000"), and an optional "." decimal part
// ("1.5"). ok is false for anything else ("1,5", ".5", "5,00,0") and for an amount ValidAmount refuses.
func ParseMachineAmount(text string) (float64, bool) {
	t := strings.TrimSpace(text)
	whole, frac, hasFrac := strings.Cut(t, ".")
	if whole == "" || (hasFrac && (frac == "" || !allDigits(frac))) {
		return 0, false
	}
	groups := strings.Split(whole, ",")
	for i, g := range groups {
		if !allDigits(g) || (i > 0 && len(g) != 3) || (len(groups) > 1 && (len(groups[0]) == 0 || len(groups[0]) > 3)) {
			return 0, false
		}
	}
	n, err := strconv.ParseFloat(strings.Join(groups, "")+"."+frac, 64)
	if !hasFrac {
		n, err = strconv.ParseFloat(strings.Join(groups, ""), 64)
	}
	if err != nil || !ValidAmount(n) {
		return 0, false
	}
	return n, true
}

func allDigits(s string) bool {
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
