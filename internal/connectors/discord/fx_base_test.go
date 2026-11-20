package discord

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"
)

// The slash option 기준통화 (base) is the currency to show values in: the board, each currency stated in
// it. It is the option the command had before 통화 (currency) was added, so an old definition still works.
// Rates here: 1 USD = 148 JPY = 1,350 KRW = 0.9 EUR = 6.75 CNY = 0.75 GBP = 0.8 CHF.
func TestFXCardShowsOneForeignUnitInSelectedCurrency(t *testing.T) {
	for _, tc := range []struct {
		locale, base, option string
		rows                 int
		notFrom              string // the board never states the base in itself
		want                 []string
	}{
		{"ko", "KRW", "", 5, "원", []string{"1달러 ≈ 1,350원", "100엔 ≈ 912.2원", "1위안 ≈ 200원"}},
		{"ko", "JPY", "jpy", 5, "엔", []string{"1달러 ≈ 148엔", "1유로 ≈ 164.4엔", "1위안 ≈ 21.93엔", "1파운드 ≈ 197.3엔", "1,000원 ≈ 109.6엔"}},
		{"ko", "JPY", "엔화", 5, "엔", []string{"1달러 ≈ 148엔"}},
		{"en", "USD", "", 5, "USD", []string{"1,000 JPY ≈ 6.757 USD"}},
		{"en", "CHF", "CHF", 6, "CHF", []string{"10 USD ≈ 8 CHF"}},
	} {
		t.Run(tc.locale+"_"+tc.base, func(t *testing.T) {
			opts := map[string]string{}
			if tc.option != "" {
				opts["base"] = tc.option
			}
			embed := fxEmbed(t, tc.locale, opts, false)
			lines := strings.Split(embed.Description, "\n")
			if !strings.HasSuffix(embed.Title, tc.base) {
				t.Errorf("title %q does not name %s", embed.Title, tc.base)
			}
			if len(lines) != tc.rows {
				t.Fatalf("unexpected comparison currencies: %v", lines)
			}
			from := regexp.MustCompile(`^[\d,.]+ ?` + tc.notFrom + ` ≈`)
			for _, l := range lines {
				if from.MatchString(l) {
					t.Errorf("%q states the base in itself", l)
				}
			}
			for _, want := range tc.want {
				if !slices.Contains(lines, want) {
					t.Errorf("missing %q in %v", want, lines)
				}
			}
		})
	}
}

// Every way to combine currency (통화), base (기준통화) and amount (금액), as a slash command and as the
// words of a message, in Korean (home KRW) and English (home USD).
func TestFXCurrencyBaseAmountCombinations(t *testing.T) {
	// the board in each base, one unit of each (the unit that reads naturally)
	koInJPY := []string{"1달러 ≈ 148엔", "1유로 ≈ 164.4엔", "1위안 ≈ 21.93엔", "1파운드 ≈ 197.3엔", "1,000원 ≈ 109.6엔"}
	koWonInEach := []string{"10,000원 ≈ 7.407달러", "10,000원 ≈ 1,096엔", "10,000원 ≈ 6.667유로", "10,000원 ≈ 50위안", "10,000원 ≈ 5.556파운드"}
	enInEUR := []string{"10 USD ≈ 9 EUR", "1,000 JPY ≈ 6.081 EUR", "10 CNY ≈ 1.333 EUR", "1 GBP ≈ 1.2 EUR", "10,000 KRW ≈ 6.667 EUR"}
	type way struct {
		slash  map[string]string // nil: not a slash command
		prefix string            // "": not a prefix command
	}
	for _, tc := range []struct {
		name, locale string
		way          way
		want         []string
	}{
		// nothing: the board in the home currency
		{"ko none", "ko", way{map[string]string{}, "!환율"}, []string{"1달러 ≈ 1,350원", "100엔 ≈ 912.2원", "1유로 ≈ 1,500원", "1위안 ≈ 200원", "1파운드 ≈ 1,800원"}},
		{"en none", "en", way{map[string]string{}, "!fx"}, []string{"1,000 JPY ≈ 6.757 USD", "1 EUR ≈ 1.111 USD", "10 CNY ≈ 1.481 USD", "1 GBP ≈ 1.333 USD", "10,000 KRW ≈ 7.407 USD"}},
		// base only: the board valued in it (slash only: a bare word of a message is a currency)
		{"ko base only", "ko", way{slash: map[string]string{"base": "엔화"}}, koInJPY},
		{"ko base only by code", "ko", way{slash: map[string]string{"base": "JPY"}}, koInJPY},
		{"en base only", "en", way{slash: map[string]string{"base": "euro"}}, enInEUR},
		{"ko base only is the home currency", "ko", way{slash: map[string]string{"base": "원"}}, []string{"1달러 ≈ 1,350원", "100엔 ≈ 912.2원", "1유로 ≈ 1,500원", "1위안 ≈ 200원", "1파운드 ≈ 1,800원"}},
		// currency only: against the home currency, both directions
		{"ko currency only", "ko", way{map[string]string{"currency": "엔화"}, "!환율 엔화"}, []string{"100엔 ≈ 912.2원", "1,000원 ≈ 109.6엔"}},
		{"en currency only", "en", way{map[string]string{"currency": "yen"}, "!fx yen"}, []string{"1,000 JPY ≈ 6.757 USD", "1 USD ≈ 148 JPY"}},
		// currency + base: against the base, both directions
		{"ko currency and base", "ko", way{map[string]string{"currency": "달러", "base": "유로"}, "!환율 달러 유로"}, []string{"10달러 ≈ 9유로", "1유로 ≈ 1.111달러"}},
		{"ko currency and base, names with a space", "ko", way{map[string]string{"currency": "미국 달러", "base": "유로"}, "!환율 미국 달러 유로"}, []string{"10달러 ≈ 9유로", "1유로 ≈ 1.111달러"}},
		{"ko currency and base, base with a space", "ko", way{map[string]string{"currency": "유로", "base": "미국 달러"}, "!환율 유로 미국 달러"}, []string{"1유로 ≈ 1.111달러", "10달러 ≈ 9유로"}},
		{"ko currency and base by code", "ko", way{map[string]string{"currency": "USD", "base": "EUR"}, "!환율 USD EUR"}, []string{"10달러 ≈ 9유로", "1유로 ≈ 1.111달러"}},
		{"ko the home currency against a base", "ko", way{map[string]string{"currency": "원", "base": "엔"}, "!환율 원 엔"}, []string{"1,000원 ≈ 109.6엔", "100엔 ≈ 912.2원"}},
		{"en currency and base", "en", way{map[string]string{"currency": "usd", "base": "eur"}, "!fx usd eur"}, []string{"10 USD ≈ 9 EUR", "1 EUR ≈ 1.111 USD"}},
		{"en currency and base by name", "en", way{map[string]string{"currency": "Japanese Yen", "base": "euro"}, "!fx japanese yen euro"}, []string{"1,000 JPY ≈ 6.081 EUR", "1 EUR ≈ 164.4 JPY"}},
		// amount of the currency, in the base or the home currency
		{"ko amount of a currency in home", "ko", way{map[string]string{"currency": "엔", "amount": "50000"}, "!환율 50000 엔"}, []string{"50,000엔 ≈ 456,100원"}},
		{"ko amount of a currency in a base", "ko", way{map[string]string{"currency": "엔", "base": "달러", "amount": "5000"}, "!환율 엔 달러 5000"}, []string{"5,000엔 ≈ 33.78달러"}},
		{"ko amount of a currency in a base, number first", "ko", way{map[string]string{"currency": "달러", "base": "유로", "amount": "100"}, "!환율 100 달러 유로"}, []string{"100달러 ≈ 90유로"}},
		{"en amount of a currency in a base", "en", way{map[string]string{"currency": "gbp", "base": "eur", "amount": "100"}, "!fx 100 gbp eur"}, []string{"100 GBP ≈ 120 EUR"}},
		// amount with no currency: that much of the base (or home) in each board currency
		{"ko amount in home", "ko", way{map[string]string{"amount": "10000"}, "!환율 10000"}, koWonInEach},
		{"ko amount of the base", "ko", way{slash: map[string]string{"base": "엔", "amount": "100"}}, []string{"100엔 ≈ 0.6757달러", "100엔 ≈ 0.6081유로", "100엔 ≈ 4.561위안", "100엔 ≈ 0.5068파운드", "100엔 ≈ 912.2원"}},
		{"en amount of the base", "en", way{slash: map[string]string{"base": "eur", "amount": "10"}}, []string{"10 EUR ≈ 11.11 USD", "10 EUR ≈ 1,644 JPY", "10 EUR ≈ 75 CNY", "10 EUR ≈ 8.333 GBP", "10 EUR ≈ 15,000 KRW"}},
		{"ko amount of the home currency named", "ko", way{map[string]string{"currency": "원", "amount": "10000"}, "!환율 원 10000"}, koWonInEach},
		// the currency is the base (or the home currency when there is no base): the same as a base alone
		{"ko currency equals base", "ko", way{map[string]string{"currency": "엔화", "base": "JPY"}, "!환율 엔화 JPY"}, koInJPY},
		{"ko currency equals home", "ko", way{map[string]string{"currency": "원"}, "!환율 원"}, []string{"1달러 ≈ 1,350원", "100엔 ≈ 912.2원", "1유로 ≈ 1,500원", "1위안 ≈ 200원", "1파운드 ≈ 1,800원"}},
		{"ko currency equals base, with an amount", "ko", way{map[string]string{"currency": "엔", "base": "엔화", "amount": "100"}, "!환율 100 엔 엔화"}, []string{"100엔 ≈ 0.6757달러", "100엔 ≈ 0.6081유로", "100엔 ≈ 4.561위안", "100엔 ≈ 0.5068파운드", "100엔 ≈ 912.2원"}},
		{"en currency equals home", "en", way{map[string]string{"currency": "usd"}, "!fx usd"}, []string{"1,000 JPY ≈ 6.757 USD", "1 EUR ≈ 1.111 USD", "10 CNY ≈ 1.481 USD", "1 GBP ≈ 1.333 USD", "10,000 KRW ≈ 7.407 USD"}},
	} {
		check := func(t *testing.T, got []string) {
			t.Helper()
			if !slices.Equal(got, tc.want) {
				t.Errorf("got  %q\nwant %q", got, tc.want)
			}
		}
		if tc.way.slash != nil {
			t.Run(tc.name+" /", func(t *testing.T) { check(t, fxCard(t, tc.locale, tc.way.slash)) })
		}
		if tc.way.prefix != "" {
			t.Run(tc.name+" !", func(t *testing.T) { check(t, fxPrefixCard(t, tc.locale, tc.way.prefix)) })
		}
	}
}

// A base is checked like a currency: before anything is fetched or counted, and told to the member alone.
func TestFXBaseIsRefusedLikeACurrency(t *testing.T) {
	for _, tc := range []struct {
		name  string
		opts  []*discordgo.ApplicationCommandInteractionDataOption
		calls int32
	}{
		{"malformed base", []*discordgo.ApplicationCommandInteractionDataOption{strOption("base", "U$D")}, 0},
		{"blank base is not the default", []*discordgo.ApplicationCommandInteractionDataOption{strOption("base", "   ")}, 0},
		{"unknown base next to a currency", []*discordgo.ApplicationCommandInteractionDataOption{strOption("currency", "엔"), strOption("base", "nope")}, 0},
		{"base the service has no rates for", []*discordgo.ApplicationCommandInteractionDataOption{strOption("base", "BTN")}, 1},
		{"currency the service has no rates for", []*discordgo.ApplicationCommandInteractionDataOption{strOption("currency", "BTN"), strOption("base", "USD")}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFXDelivery(t, nil)
			d.slash(t, tc.opts...)
			if got := d.calls.Load(); got != tc.calls {
				t.Errorf("provider calls = %d (%v), want %d", got, d.paths, tc.calls)
			}
			if _, ok := d.fake.find(http.MethodPatch, "/messages/@original"); ok {
				t.Error("the error was written into the public response")
			}
			follow, ok := d.fake.find(http.MethodPost, "/webhooks/app/tok")
			if !ok {
				t.Fatalf("no follow-up: %+v", d.fake.calls)
			}
			var body struct{ Flags int }
			if err := json.Unmarshal([]byte(follow.body), &body); err != nil || body.Flags != ephemeral {
				t.Errorf("follow-up flags = %d (%v), want ephemeral", body.Flags, err)
			}
			if !strings.Contains(follow.body, "통화") {
				t.Errorf("follow-up should say how to write a currency, got %s", follow.body)
			}
		})
	}
}

// Words that name no pair of currencies stay one name and fail as an invalid currency.
func TestFXPrefixWordsThatDoNotSplitFailAsOneCurrency(t *testing.T) {
	for _, text := range []string{"!환율 달러 foo", "!환율 foo bar", "!환율 foo 달러"} {
		t.Run(text, func(t *testing.T) {
			d := newFXDelivery(t, nil)
			cmd, o, _ := ParsePrefix("!", text)
			r := d.g.svc.Execute(t.Context(), requestFor(cmd, o, InvokeContext{Locale: "ko", Typed: true}, "x"))
			if r.OK || r.Error.MessageKey != "fx.invalid_currency" || d.calls.Load() != 0 {
				t.Errorf("%q: %+v (provider calls %d)", text, r, d.calls.Load())
			}
		})
	}
}

// The command's option list: currency, base and amount, all optional; base keeps its pre-통화 name.
func TestFXCommandOptions(t *testing.T) {
	cmd, _ := ByName("fx")
	var names []string
	for _, o := range cmd.Options {
		names = append(names, o.Name)
		if o.Required {
			t.Errorf("%s is required", o.Name)
		}
	}
	if !slices.Equal(names, []string{"currency", "base", "amount"}) {
		t.Errorf("options = %v", names)
	}
	if got := localizations("discord.command.fx.option.base.name")[discordgo.Korean]; got != "기준통화" {
		t.Errorf("ko name of base = %q, want the name the option always had", got)
	}
}
