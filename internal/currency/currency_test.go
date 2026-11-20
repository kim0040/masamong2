package currency

import (
	"encoding/json"
	"math"
	"strings"
	"testing"

	"masamong/internal/i18n"
)

func TestResolveCodesAndNames(t *testing.T) {
	l := New(i18n.New())
	for in, want := range map[string]string{
		"USD": "USD", " usd ": "USD", "Krw": "KRW",
		"달러": "USD", "미국 달러": "USD", "미국달러": "USD", "엔": "JPY", "엔화": "JPY", "유로": "EUR", "위안": "CNY", "위안화": "CNY",
		"파운드": "GBP", "동": "VND", "바트": "THB", "루피아": "IDR", "원": "KRW", "호주 달러": "AUD",
		"yen": "JPY", "Japanese Yen": "JPY", "US dollar": "USD", "円": "JPY", "ウォン": "KRW", "人民币": "CNY", "欧元": "EUR", "libra esterlina": "GBP",
	} {
		got, ok := l.Resolve(in)
		if !ok || got != want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", in, got, ok, want)
		}
	}
	for _, bad := range []string{"", "   ", "U$D", "ZZZ", "USD KRW", "dollarz", "달러 50000", "XYZ1", "페소"} {
		if got, ok := l.Resolve(bad); ok {
			t.Errorf("Resolve(%q) = %q, want no match", bad, got)
		}
	}
}

func TestValidCodeIsISO(t *testing.T) {
	for _, c := range []string{"USD", "krw", "BTN", "JPY"} {
		if !ValidCode(c) {
			t.Errorf("%s should be valid", c)
		}
	}
	for _, c := range []string{"", "US", "USDD", "U$D", "ZZZ", "123", "ÜSD"} {
		if ValidCode(c) {
			t.Errorf("%q should be invalid", c)
		}
	}
}

// Every name is one currency's: a name two currencies share would make a typed word ambiguous.
func TestNamesBelongToOneCurrency(t *testing.T) {
	cat := i18n.New()
	seen := map[string]string{}
	for _, loc := range cat.Locales() {
		for code, list := range cat.Prefixed(loc, namePrefix) {
			if !ValidCode(code) {
				t.Errorf("%s: fx.currency.%s is not an ISO code", loc, code)
			}
			for _, n := range names(list) {
				k := fold(n)
				if prev, dup := seen[k]; dup && prev != code {
					t.Errorf("%s: name %q is both %s and %s", loc, n, prev, code)
				}
				seen[k] = code
			}
		}
	}
	for _, loc := range []string{"ko", "ja", "zh-Hans"} {
		for _, code := range BoardFor(New(cat).Home(loc)) {
			if New(cat).Name(loc, code) == "" {
				t.Errorf("%s has no display name for board currency %s", loc, code)
			}
		}
	}
}

// The home currency is the currency of the region the tag names: its own region when it has one, else the
// likely region of its language; no catalog entry says it.
func TestHome(t *testing.T) {
	l := New(i18n.New())
	for locale, want := range map[string]string{
		"ko": "KRW", "ko-KR": "KRW", "ko_KR": "KRW", "ja": "JPY", "ja-JP": "JPY",
		"en": "USD", "en-US": "USD", "en-GB": "GBP", "es": "EUR", "es-ES": "EUR", "es-419": "USD", "es-MX": "MXN",
		"zh-Hans": "CNY", "zh-CN": "CNY", "pt-BR": "BRL", "fr": "EUR", "de-CH": "CHF",
		"": "USD", "xx": "USD", "en-001": "USD",
	} {
		if got := l.Home(locale); got != want {
			t.Errorf("Home(%q) = %s, want %s", locale, got, want)
		}
	}
}

func TestBoardFor(t *testing.T) {
	for home, want := range map[string]string{
		"KRW": "USD JPY EUR CNY GBP KRW", "USD": "USD JPY EUR CNY GBP", "JPY": "USD JPY EUR CNY GBP", "BRL": "USD JPY EUR CNY GBP BRL",
	} {
		if got := strings.Join(BoardFor(home), " "); got != want {
			t.Errorf("BoardFor(%s) = %s, want %s", home, got, want)
		}
	}
	if len(Board) != 5 {
		t.Errorf("the major currencies were changed by BoardFor: %v", Board)
	}
}

// One currency against several is stated in one unit: the locale's, made larger only while even the
// largest value would stay under 1.
func TestSharedUnit(t *testing.T) {
	l := New(i18n.New())
	for _, tc := range []struct {
		name, locale, from string
		rates              []float64
		want               float64
	}{
		{"a dollar in won and euros", "ko", "USD", []float64{1400, 0.92}, 1},
		{"every rate under 1", "en", "KRW", []float64{0.00074, 0.11}, 10},
		{"won in dollars only", "en", "KRW", []float64{0.00074}, 10000},
		{"the locale's unit for yen", "ko", "JPY", []float64{9.3, 0.0067}, 100},
		{"no rates", "en", "USD", nil, 1},
	} {
		if got := l.SharedUnit(tc.locale, tc.from, tc.rates); got != tc.want {
			t.Errorf("%s: unit %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestQuoteUnits(t *testing.T) {
	l := New(i18n.New())
	for _, tc := range []struct {
		name, locale, from, to string
		rate, amount           float64
		want                   string
		unit                   float64
	}{
		// the convention a locale's catalog states: Korean banks quote the yen per 100
		{"ko yen per 100", "ko", "JPY", "KRW", 9.302, 0, "100엔 ≈ 930.2원", 100},
		{"ko won per 1000 to yen", "ko", "KRW", "JPY", 0.1075, 0, "1,000원 ≈ 107.5엔", 1000},
		{"ko dollar", "ko", "USD", "KRW", 1350.4, 0, "1달러 ≈ 1,350원", 1},
		// the general rule: the smallest power of ten that gives at least 1
		{"vnd", "ko", "VND", "KRW", 0.054, 0, "100동 ≈ 5.4원", 100},
		{"krw to usd", "ko", "KRW", "USD", 0.00072, 0, "10,000원 ≈ 7.2달러", 10000},
		{"en codes", "en", "KRW", "USD", 0.00072, 0, "10,000 KRW ≈ 7.2 USD", 10000},
		{"en jpy has no convention", "en", "JPY", "USD", 0.00625, 0, "1,000 JPY ≈ 6.25 USD", 1000},
		{"ja won per 100", "ja", "KRW", "USD", 0.00072, 0, "10,000ウォン ≈ 7.2ドル", 10000},
		{"ja won per 100 floor", "ja", "KRW", "JPY", 0.107, 0, "100ウォン ≈ 10.7円", 100},
		// an amount is the unit, shown with four significant digits
		{"amount", "ko", "JPY", "KRW", 9.30232, 50000, "50,000엔 ≈ 465,100원", 50000},
		{"small amount", "en", "USD", "KRW", 1350.456, 0.5, "0.5 USD ≈ 675.2 KRW", 0.5},
		// a currency without a name in the locale reads as its code
		{"unnamed", "ko", "XOF", "KRW", 2.3, 0, "1 XOF ≈ 2.3원", 1},
	} {
		got, ok := l.Quote(tc.locale, tc.from, tc.to, tc.rate, tc.amount)
		if !ok || got.Text != tc.want || got.Unit != tc.unit || got.From != tc.from || got.To != tc.to {
			t.Errorf("%s: got %+v ok=%v, want %q unit %v", tc.name, got, ok, tc.want, tc.unit)
		}
	}
	if q, _ := l.Quote("ko", "JPY", "KRW", 9.302, 0); q.Value != 930.2 {
		t.Errorf("value = %v, want 930.2", q.Value)
	}
	for _, rate := range []float64{0, -1} {
		if _, ok := l.Quote("ko", "JPY", "KRW", rate, 0); ok {
			t.Errorf("rate %v should give no quote", rate)
		}
	}
}

// One syntax per locale: digits, thousands groups of exactly three, the locale's decimal separator.
func TestParseAmount(t *testing.T) {
	for _, tc := range []struct {
		locale, in string
		want       float64
	}{
		{"en", "50000", 50000}, {"en", "50,000", 50000}, {"en", " 0.5 ", 0.5}, {"en", "1,234.5", 1234.5}, {"en", "1,234,567", 1234567}, {"en", "0.25", 0.25},
		{"ko", "1,234.56", 1234.56}, {"ja", "12,345", 12345}, {"zh-Hans", "1,000.5", 1000.5},
		{"es", "1,5", 1.5}, {"es", "1.234,56", 1234.56}, {"es", "1.234", 1234}, {"es", "50000", 50000}, {"es", "0,5", 0.5},
	} {
		if got, ok := ParseAmount(tc.locale, tc.in); !ok || got != tc.want {
			t.Errorf("ParseAmount(%s, %q) = %v, %v; want %v", tc.locale, tc.in, got, ok, tc.want)
		}
	}
	// ambiguous or malformed: never silently another number
	for _, tc := range []struct{ locale, in string }{
		{"en", "1,5"}, {"en", "5,00,0"}, {"en", ".5"}, {"en", "50,"}, {"en", "50."}, {"en", "1,23"}, {"en", "1,2345"}, {"en", "1.2.3"}, {"en", "1,234,56"}, {"en", ",500"},
		{"ko", "1,5"}, {"ko", "5,00,0"},
		{"es", "1.234,56,7"}, {"es", "1.5"}, {"es", ",5"}, {"es", "1.23.456"}, {"es", "1,234.5"},
		{"en", ""}, {"en", " "}, {"en", "0"}, {"en", "0.0"}, {"en", "-5"}, {"en", "+5"}, {"en", "abc"}, {"en", "1e3"}, {"en", "inf"}, {"en", "NaN"}, {"en", "5만"}, {"en", "1 000"},
		{"en", "1000000000000000.5"}, {"en", "1000000000000000000"},
	} {
		if got, ok := ParseAmount(tc.locale, tc.in); ok {
			t.Errorf("ParseAmount(%s, %q) = %v, want invalid", tc.locale, tc.in, got)
		}
	}
	if got, ok := ParseAmount("en", "1,000,000,000,000,000"); !ok || got != MaxAmount {
		t.Errorf("the cap itself is allowed: %v %v", got, ok)
	}
}

// The prefix lexer only needs the separators every supported locale uses; this pins that assumption.
func TestSeparatorsOfShippedLocales(t *testing.T) {
	for _, loc := range i18n.New().Locales() {
		g, d := separators(loc)
		if (g != "," && g != ".") || (d != "," && d != ".") || g == d {
			t.Errorf("%s: group %q decimal %q: IsNumber would not recognise its numbers", loc, g, d)
		}
	}
	if g, d := separators("es"); g != "." || d != "," {
		t.Errorf("es separators = %q %q", g, d)
	}
	if g, d := separators("en"); g != "," || d != "." {
		t.Errorf("en separators = %q %q", g, d)
	}
}

func TestIsNumber(t *testing.T) {
	for _, in := range []string{"50,000", "5", ".5", "1.234,56", "5,00,0", "50,"} {
		if !IsNumber(in) {
			t.Errorf("IsNumber(%q) = false: a malformed amount is still an amount", in)
		}
	}
	for _, in := range []string{"엔화", "", ",", ".", "e5", "5만", "USD", "1e3", "-5"} {
		if IsNumber(in) {
			t.Errorf("IsNumber(%q) = true", in)
		}
	}
}

func TestAmountMessageParts(t *testing.T) {
	if got := MaxAmountText("en"); got != "1,000,000,000,000,000" {
		t.Errorf("max = %q", got)
	}
	if got := MaxAmountText("es"); got != "1.000.000.000.000.000" {
		t.Errorf("max es = %q", got)
	}
	if got := AmountExample("es"); got != "1.234,5" {
		t.Errorf("example es = %q", got)
	}
	if got := AmountExample("ko"); got != "1,234.5" {
		t.Errorf("example ko = %q", got)
	}
}

// Extreme rates and amounts: no quote is ever infinite (it could not be sent as JSON), the unit search
// goes one power of ten past the amount cap when that is what reaches 1, and the boundaries round as stated.
func TestQuoteEdgeCases(t *testing.T) {
	l := New(i18n.New())
	for _, tc := range []struct {
		name, locale, from, to string
		rate, amount, unit     float64
		want                   string
	}{
		{"a tie at the fourth digit may round either way (binary floats)", "en", "USD", "EUR", 0.99995, 0, 10, "10 USD ≈ 9.999 EUR"},
		{"explicit 0.99995", "en", "USD", "EUR", 0.99995, 1, 1, "1 USD ≈ 1 EUR"},
		{"the power of ten past the cap reaches 1", "en", "USD", "EUR", 1e-16, 0, 1e16, "10,000,000,000,000,000 USD ≈ 1 EUR"},
		{"never reaching 1 is stated as the fraction it is", "en", "USD", "EUR", 1e-18, 0, 1e16, "10,000,000,000,000,000 USD ≈ 0.01 EUR"},
		{"the cap itself, grouped by the locale", "es", "USD", "VND", 25000, 1e15, 1e15, "1.000.000.000.000.000 USD ≈ 25.000.000.000.000.000.000 VND"},
	} {
		q, ok := l.Quote(tc.locale, tc.from, tc.to, tc.rate, tc.amount)
		if !ok || q.Unit != tc.unit || q.Text != tc.want {
			t.Errorf("%s: %+v ok=%v, want unit %v %q", tc.name, q, ok, tc.unit, tc.want)
		}
	}
	for _, tc := range []struct{ rate, amount float64 }{
		{math.MaxFloat64, 1e15}, {math.MaxFloat64, 10}, {1e308, 10}, {math.Inf(1), 1}, {math.NaN(), 1},
	} {
		if q, ok := l.Quote("en", "USD", "EUR", tc.rate, tc.amount); ok {
			if _, err := json.Marshal(q); err != nil || math.IsInf(q.Value, 0) {
				t.Errorf("rate %v amount %v: unusable quote %+v (%v)", tc.rate, tc.amount, q, err)
			} else {
				t.Errorf("rate %v amount %v: expected no quote, got %+v", tc.rate, tc.amount, q)
			}
		}
	}
}

// A program's amount (a chat tool call, the CLI, the console) reads the same in every locale: "." is the
// decimal point and "," only groups thousands, so a Spanish request's "1.500" is one and a half, never 1500.
func TestParseMachineAmount(t *testing.T) {
	for text, want := range map[string]float64{"1.5": 1.5, "1.500": 1.5, "50000": 50000, "50,000": 50000, "1,234,567.25": 1234567.25, " 7 ": 7} {
		if got, ok := ParseMachineAmount(text); !ok || got != want {
			t.Errorf("%q = %v, %v; want %v", text, got, ok, want)
		}
	}
	for _, text := range []string{"", "1,5", ".5", "5.", "5,00,0", "1.2.3", "1,234.5,6", "-1", "1e3", "0", "1 000", "1234,567"} {
		if got, ok := ParseMachineAmount(text); ok {
			t.Errorf("%q read as %v", text, got)
		}
	}
}
