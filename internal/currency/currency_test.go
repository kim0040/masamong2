package currency

import (
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
		for _, code := range Board {
			if New(cat).Name(loc, code) == "" {
				t.Errorf("%s has no display name for board currency %s", loc, code)
			}
		}
	}
}

func TestHome(t *testing.T) {
	l := New(i18n.New())
	for locale, want := range map[string]string{"ko": "KRW", "ko-KR": "KRW", "en": "USD", "en-US": "USD", "ja": "USD", "fr": "USD", "": "USD"} {
		if got := l.Home(locale); got != want {
			t.Errorf("Home(%q) = %s, want %s", locale, got, want)
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

func TestParseAmount(t *testing.T) {
	for in, want := range map[string]float64{"50000": 50000, "50,000": 50000, " 0.5 ": 0.5, ".5": 0.5, "1,234.5": 1234.5} {
		if got, ok := ParseAmount(in); !ok || got != want {
			t.Errorf("ParseAmount(%q) = %v, %v", in, got, ok)
		}
	}
	for _, bad := range []string{"", " ", "0", "-5", "abc", "1e3", "inf", "NaN", "1.2.3", "5만", "1000000000000000000"} {
		if got, ok := ParseAmount(bad); ok {
			t.Errorf("ParseAmount(%q) = %v, want invalid", bad, got)
		}
	}
	if !IsNumber("50,000") || IsNumber("엔화") || IsNumber("") || IsNumber("e5") {
		t.Error("IsNumber misreads tokens")
	}
	_ = strings.TrimSpace
}
