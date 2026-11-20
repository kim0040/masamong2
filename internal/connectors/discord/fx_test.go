package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/application"
)

// The command always stays finance.fx and hands the action what was typed. The home currency is the
// action's to pick from the request's language (KRW for Korean, USD otherwise), so the language that
// writes the quotes is the one that picks it. Nothing is checked or rewritten here.
func TestFXBuildHandsOverWhatWasTyped(t *testing.T) {
	cmd, _ := ByName("fx")
	for _, tc := range []struct {
		locale string
		typed  bool // the options are words of a message
		opts   map[string]string
		want   map[string]any
	}{
		// the command always sends its own base (blank when none was typed): a configured tool default
		// is merged only into a key that is absent, so it cannot become the command's base
		{"ko", false, nil, map[string]any{"home": "", "base": ""}},
		{"ko-KR", false, nil, map[string]any{"home": "", "base": ""}},
		{"en-US", false, nil, map[string]any{"home": "", "base": ""}},
		{"ja", false, nil, map[string]any{"home": "", "base": ""}},
		// a Discord number option reaches the action as a number, the words of a message as the text typed
		{"ko", false, map[string]string{"currency": "엔화", "amount": "50000"}, map[string]any{"home": "", "base": "", "currency": "엔화", "amount": 50000.0}},
		{"es", false, map[string]string{"amount": "1.5"}, map[string]any{"home": "", "base": "", "amount": 1.5}},
		{"es", true, map[string]string{"amount": "1,5"}, map[string]any{"home": "", "base": "", "amount": "1,5"}},
		{"es", true, map[string]string{"amount": "1.5"}, map[string]any{"home": "", "base": "", "amount": "1.5"}},
		{"en", false, map[string]string{"currency": "U$D"}, map[string]any{"home": "", "base": "", "currency": "U$D"}},
		{"ko", false, map[string]string{"currency": "   "}, map[string]any{"home": "", "base": "", "currency": "   "}}, // given but blank: not the default
		{"ko", false, map[string]string{"base": "jpy"}, map[string]any{"home": "", "base": "jpy"}},
		{"ko", false, map[string]string{"currency": "달러", "base": "유로", "amount": "5"}, map[string]any{"home": "", "currency": "달러", "base": "유로", "amount": 5.0}},
		{"en", false, map[string]string{"base": ""}, map[string]any{"home": "", "base": ""}}, // given but blank: not the default
	} {
		act, input := cmd.Build(tc.opts, InvokeContext{Locale: tc.locale, Typed: tc.typed})
		if act != "finance.fx" || len(input) != len(tc.want) {
			t.Errorf("%s %v: got %s %v, want finance.fx %v", tc.locale, tc.opts, act, input, tc.want)
			continue
		}
		for k, v := range tc.want {
			if input[k] != v {
				t.Errorf("%s %v: %s = %v, want %v", tc.locale, tc.opts, k, input[k], v)
			}
		}
	}
}

func TestFXPrefixParsing(t *testing.T) {
	for _, tc := range []struct{ text, currency, base, amount string }{
		{"!환율", "", "", ""},
		{"!환율 엔화", "엔화", "", ""},
		{"!환율 USD", "USD", "", ""},
		{"!환율 50000 엔화", "엔화", "", "50000"},
		{"!환율 엔화 50000", "엔화", "", "50000"},
		{"!환율 50,000 엔화", "엔화", "", "50,000"},
		{"!환율 미국 달러 100", "미국 달러", "", "100"},
		{"!환율 100 미국 달러", "미국 달러", "", "100"},
		{"!환율 50000", "", "", "50000"},
		{"!환율 2 3 엔", "3 엔", "", "2"}, // one amount: what is left is read as the currency, and fails there
		{"!fx yen 10", "yen", "", "10"},
		// two names: the first is the currency, the second the base to value it in
		{"!환율 달러 유로", "달러", "유로", ""},
		{"!환율 달러 유로 100", "달러", "유로", "100"},
		{"!환율 100 달러 유로", "달러", "유로", "100"},
		{"!환율 달러 100 유로", "달러", "유로", "100"},
		{"!환율 USD EUR", "USD", "EUR", ""},
		{"!fx usd eur 5", "usd", "eur", "5"},
		// names with spaces on either side, or both
		{"!환율 미국 달러 유로", "미국 달러", "유로", ""},
		{"!환율 달러 미국 달러", "달러", "미국 달러", ""},
		{"!환율 미국 달러 엔화 100", "미국 달러", "엔화", "100"},
		// no split resolves both: everything is one currency name, and fails as such
		{"!환율 달러 foo", "달러 foo", "", ""},
		{"!환율 foo bar", "foo bar", "", ""},
	} {
		cmd, opts, ok := ParsePrefix("!", tc.text)
		if !ok || cmd.Name != "fx" {
			t.Errorf("%q: not parsed as fx (%v)", tc.text, ok)
			continue
		}
		if opts["currency"] != tc.currency || opts["base"] != tc.base || opts["amount"] != tc.amount {
			t.Errorf("%q: currency %q base %q amount %q, want %q %q %q", tc.text, opts["currency"], opts["base"], opts["amount"], tc.currency, tc.base, tc.amount)
		}
	}
}

// A tail that is no currency stays one currency for the action to refuse — however long it is:
// only splits where each side could still be a name are tried, so a long unknown input parses at
// once (the benchmark keeps it that way).
func TestFXPrefixLongUnknownInputStaysOneName(t *testing.T) {
	tail := strings.Repeat("x ", 999) + "x" // 1,999 characters of unknown words
	cmd, opts, ok := ParsePrefix("!", "!fx "+tail)
	if !ok || cmd.Name != "fx" {
		t.Fatalf("the long input did not parse as fx")
	}
	if opts["currency"] != tail || opts["base"] != "" || opts["amount"] != "" {
		t.Errorf("opts = %v, want the whole tail as one currency", opts)
	}
}

func BenchmarkFXPrefixLongUnknownInput(b *testing.B) {
	for _, n := range []int{1, 100, 998} {
		b.Run(fmt.Sprint(n), func(b *testing.B) {
			text := "!fx" + strings.Repeat(" x", n)
			for b.Loop() {
				ParsePrefix("!", text)
			}
		})
	}
}

// fxCard runs /환율 against a local rate service whose dollar prices are fixed.
func fxCard(t *testing.T, locale string, opts map[string]string) []string {
	t.Helper()
	return fxCardOf(t, locale, opts, false)
}

// fxPrefixCard runs the words of a prefix command ("!환율 달러 유로") the way the gateway does.
func fxPrefixCard(t *testing.T, locale, text string) []string {
	t.Helper()
	_, opts, ok := ParsePrefix("!", text)
	if !ok {
		t.Fatalf("%q did not parse as a command", text)
	}
	return fxCardOf(t, locale, opts, true)
}

func fxCardOf(t *testing.T, locale string, opts map[string]string, typed bool) []string {
	t.Helper()
	return strings.Split(fxEmbed(t, locale, opts, typed).Description, "\n")
}

func fxEmbed(t *testing.T, locale string, opts map[string]string, typed bool) *discordgo.MessageEmbed {
	t.Helper()
	embed, _ := fxRun(t, InvokeContext{Locale: locale, UserID: "tester", Typed: typed}, opts)
	return embed
}

// fxRun runs the command for ictx and returns the card with the action's raw output.
func fxRun(t *testing.T, ictx InvokeContext, opts map[string]string) (*discordgo.MessageEmbed, map[string]any) {
	t.Helper()
	svc := svcForTest(t)
	g := &Gateway{svc: svc}
	unitsPerDollar := map[string]float64{"USD": 1, "KRW": 1350, "JPY": 148, "CNY": 6.75, "EUR": 0.9, "GBP": 0.75, "CHF": 0.8, "BRL": 5.4}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		base := strings.TrimPrefix(r.URL.Path, "/latest/")
		rates := make(map[string]float64)
		for code, units := range unitsPerDollar {
			rates[code] = units / unitsPerDollar[base]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"result": "success", "base_code": base, "rates": rates, "time_last_update_unix": 1757894401,
		})
	}))
	defer server.Close()
	adapter := svc.Registry().FX
	adapter.OpenBase, adapter.KeyedBase = server.URL, server.URL

	cmd, _ := ByName("fx")
	res := g.svc.Execute(context.Background(), requestFor(cmd, opts, ictx, ""))
	if !res.OK {
		t.Fatalf("%s %v: %+v", ictx.Locale, opts, res.Error)
	}
	embed := g.renderInvoked(res, cmd, ictx).Embed
	if len(embed.Fields) != 1 || embed.Footer == nil {
		t.Error("missing update time or source")
	}
	return embed, res.Output
}

func TestFXCardsStateRatesInReadableUnits(t *testing.T) {
	for _, tc := range []struct {
		name, locale string
		opts         map[string]string
		want         []string
	}{
		{"ko board in won", "ko", nil, []string{"1달러 ≈ 1,350원", "100엔 ≈ 912.2원", "1유로 ≈ 1,500원", "1위안 ≈ 200원", "1파운드 ≈ 1,800원"}},
		{"ko the home currency is the subject like any other", "ko", map[string]string{"currency": "원"}, []string{"1,000원 ≈ 0.7407달러", "1,000원 ≈ 109.6엔", "1,000원 ≈ 0.6667유로", "1,000원 ≈ 5위안", "1,000원 ≈ 0.5556파운드"}},
		{"ko one currency against the home currency first, then the board", "ko", map[string]string{"currency": "엔화"}, []string{"100엔 ≈ 912.2원", "100엔 ≈ 0.6757달러", "100엔 ≈ 0.6081유로", "100엔 ≈ 4.561위안", "100엔 ≈ 0.5068파운드"}},
		{"ko by code", "ko", map[string]string{"currency": "jpy"}, []string{"100엔 ≈ 912.2원", "100엔 ≈ 0.6757달러", "100엔 ≈ 0.6081유로", "100엔 ≈ 4.561위안", "100엔 ≈ 0.5068파운드"}},
		{"ko dollar, one dollar in each", "ko", map[string]string{"currency": "달러"}, []string{"1달러 ≈ 1,350원", "1달러 ≈ 148엔", "1달러 ≈ 0.9유로", "1달러 ≈ 6.75위안", "1달러 ≈ 0.75파운드"}},
		{"ko an amount of a currency in each", "ko", map[string]string{"currency": "엔", "amount": "50000"}, []string{"50,000엔 ≈ 456,100원", "50,000엔 ≈ 337.8달러", "50,000엔 ≈ 304.1유로", "50,000엔 ≈ 2,280위안", "50,000엔 ≈ 253.4파운드"}},
		{"ko an amount of won in every board currency", "ko", map[string]string{"amount": "10000"}, []string{"10,000원 ≈ 7.407달러", "10,000원 ≈ 1,096엔", "10,000원 ≈ 6.667유로", "10,000원 ≈ 50위안", "10,000원 ≈ 5.556파운드"}},
		{"ko the home currency with an amount", "ko", map[string]string{"currency": "원", "amount": "10000"}, []string{"10,000원 ≈ 7.407달러", "10,000원 ≈ 1,096엔", "10,000원 ≈ 6.667유로", "10,000원 ≈ 50위안", "10,000원 ≈ 5.556파운드"}},
		{"en board in dollars, won included", "en", nil, []string{"10,000 KRW ≈ 7.407 USD", "1,000 JPY ≈ 6.757 USD", "1 EUR ≈ 1.111 USD", "10 CNY ≈ 1.481 USD", "1 GBP ≈ 1.333 USD"}},
		{"en by name", "en", map[string]string{"currency": "Japanese Yen"}, []string{"1 JPY ≈ 0.006757 USD", "1 JPY ≈ 9.122 KRW", "1 JPY ≈ 0.006081 EUR", "1 JPY ≈ 0.04561 CNY", "1 JPY ≈ 0.005068 GBP"}},
		{"en won against home first, then the board", "en", map[string]string{"currency": "krw"}, []string{"10 KRW ≈ 0.007407 USD", "10 KRW ≈ 1.096 JPY", "10 KRW ≈ 0.006667 EUR", "10 KRW ≈ 0.05 CNY", "10 KRW ≈ 0.005556 GBP"}},
		{"ja reads names in other languages; yen is home", "ja", map[string]string{"currency": "엔화", "amount": "100"}, []string{"100円 ≈ 0.6757ドル", "100円 ≈ 912.2ウォン", "100円 ≈ 0.6081ユーロ", "100円 ≈ 4.561人民元", "100円 ≈ 0.5068ポンド"}},
		// a pair sizes each direction like that currency's own board: the forward line reads as it does
		// in the single view, and an amount drops nothing — the reverse keeps its one-unit line
		{"ko a pair both ways in each one's own unit", "ko", map[string]string{"currency": "달러", "base": "유로"}, []string{"1달러 ≈ 0.9유로", "1유로 ≈ 1.111달러"}},
		{"ko a pair with an amount: the amount forward, one unit back", "ko", map[string]string{"currency": "달러", "base": "유로", "amount": "100"}, []string{"100달러 ≈ 90유로", "1유로 ≈ 1.111달러"}},
		{"ko dollars and won, each in its own unit", "ko", map[string]string{"currency": "달러", "base": "원"}, []string{"1달러 ≈ 1,350원", "1,000원 ≈ 0.7407달러"}},
		{"en dollars and won", "en", map[string]string{"currency": "USD", "base": "KRW"}, []string{"1 USD ≈ 1,350 KRW", "10 KRW ≈ 0.007407 USD"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := fxCard(t, tc.locale, tc.opts)
			if !slices.Equal(got, tc.want) {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		})
	}
}

// The raw rates keep their shape next to the quotes.
func TestFXRawRatesStayInTheResult(t *testing.T) {
	svc := svcForTest(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":"success","base_code":"KRW","rates":{"USD":0.00074,"JPY":0.1096,"EUR":0.00067,"CNY":0.0049,"GBP":0.00056},"time_last_update_unix":1757894401}`))
	}))
	defer server.Close()
	adapter := svc.Registry().FX
	adapter.OpenBase, adapter.KeyedBase = server.URL, server.URL
	cmd, _ := ByName("fx")
	res := svc.Execute(context.Background(), requestFor(cmd, nil, InvokeContext{Locale: "ko", UserID: "tester"}, ""))
	if !res.OK || res.Output["base"] != "KRW" || res.Output["source"] == nil || res.Output["updated_at"] == nil {
		t.Fatalf("raw fields changed: %+v", res.Output)
	}
	if rates, _ := res.Output["rates"].(map[string]any); rates["JPY"] != 0.1096 || len(rates) != 5 {
		t.Errorf("rates = %v", res.Output["rates"])
	}
}

// A real code the service has no rate for fails as input, like a typo.
func TestFXMissingRateIsAnInputError(t *testing.T) {
	svc := svcForTest(t)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"result":"success","base_code":"KRW","rates":{"USD":0.00074}}`))
	}))
	defer server.Close()
	adapter := svc.Registry().FX
	adapter.OpenBase, adapter.KeyedBase = server.URL, server.URL
	cmd, _ := ByName("fx")
	res := svc.Execute(context.Background(), requestFor(cmd, map[string]string{"currency": "BTN", "base": "KRW"}, InvokeContext{Locale: "ko", UserID: "tester"}, ""))
	if !application.IsFXInputError(res) || res.Error.MessageKey != "fx.unsupported_currency" {
		t.Fatalf("got %+v", res)
	}
}
