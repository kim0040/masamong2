package discord

import (
	"encoding/json"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/currency"
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
		{"en", "USD", "", 5, "USD", []string{"1,000 JPY ≈ 6.757 USD", "10,000 KRW ≈ 7.407 USD"}},
		{"en", "CHF", "CHF", 6, "CHF", []string{"10 USD ≈ 8 CHF", "10,000 KRW ≈ 5.926 CHF"}},
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
	// the board in each base, one unit of each (the unit that reads naturally); the home currency is a
	// row first, then the other five managed currencies
	koInJPY := []string{"1,000원 ≈ 109.6엔", "1달러 ≈ 148엔", "1유로 ≈ 164.4엔", "1위안 ≈ 21.93엔", "1파운드 ≈ 197.3엔"}
	koWonInEach := []string{"10,000원 ≈ 7.407달러", "10,000원 ≈ 1,096엔", "10,000원 ≈ 6.667유로", "10,000원 ≈ 50위안", "10,000원 ≈ 5.556파운드"}
	enInEUR := []string{"10 USD ≈ 9 EUR", "10,000 KRW ≈ 6.667 EUR", "1,000 JPY ≈ 6.081 EUR", "10 CNY ≈ 1.333 EUR", "1 GBP ≈ 1.2 EUR"}
	koYen := []string{"100엔 ≈ 912.2원", "100엔 ≈ 0.6757달러", "100엔 ≈ 0.6081유로", "100엔 ≈ 4.561위안", "100엔 ≈ 0.5068파운드"}
	enYen := []string{"1 JPY ≈ 0.006757 USD", "1 JPY ≈ 9.122 KRW", "1 JPY ≈ 0.006081 EUR", "1 JPY ≈ 0.04561 CNY", "1 JPY ≈ 0.005068 GBP"}
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
		{"en none", "en", way{map[string]string{}, "!fx"}, []string{"10,000 KRW ≈ 7.407 USD", "1,000 JPY ≈ 6.757 USD", "1 EUR ≈ 1.111 USD", "10 CNY ≈ 1.481 USD", "1 GBP ≈ 1.333 USD"}},
		// base only: the board valued in it (slash only: a bare word of a message is a currency)
		{"ko base only", "ko", way{slash: map[string]string{"base": "엔화"}}, koInJPY},
		{"ko base only by code", "ko", way{slash: map[string]string{"base": "JPY"}}, koInJPY},
		{"en base only", "en", way{slash: map[string]string{"base": "euro"}}, enInEUR},
		{"ko base only is the home currency", "ko", way{slash: map[string]string{"base": "원"}}, []string{"1달러 ≈ 1,350원", "100엔 ≈ 912.2원", "1유로 ≈ 1,500원", "1위안 ≈ 200원", "1파운드 ≈ 1,800원"}},
		// currency only: one unit of it in the home currency first, then in each board currency
		{"ko currency only", "ko", way{map[string]string{"currency": "엔화"}, "!환율 엔화"}, koYen},
		{"ko one dollar in each", "ko", way{map[string]string{"currency": "달러"}, "!환율 달러"}, []string{"1달러 ≈ 1,350원", "1달러 ≈ 148엔", "1달러 ≈ 0.9유로", "1달러 ≈ 6.75위안", "1달러 ≈ 0.75파운드"}},
		{"en currency only", "en", way{map[string]string{"currency": "yen"}, "!fx yen"}, enYen},
		{"en dollars are home, so a euro is shown in dollars first", "en", way{map[string]string{"currency": "euro"}, "!fx euro"}, []string{"1 EUR ≈ 1.111 USD", "1 EUR ≈ 1,500 KRW", "1 EUR ≈ 164.4 JPY", "1 EUR ≈ 7.5 CNY", "1 EUR ≈ 0.8333 GBP"}},
		{"en won against home first, then the board", "en", way{map[string]string{"currency": "won"}, "!fx krw"}, []string{"10 KRW ≈ 0.007407 USD", "10 KRW ≈ 1.096 JPY", "10 KRW ≈ 0.006667 EUR", "10 KRW ≈ 0.05 CNY", "10 KRW ≈ 0.005556 GBP"}},
		// currency + base: against the base, both directions
		{"ko currency and base", "ko", way{map[string]string{"currency": "달러", "base": "유로"}, "!환율 달러 유로"}, []string{"1달러 ≈ 0.9유로", "1유로 ≈ 1.111달러"}},
		{"ko currency and base, names with a space", "ko", way{map[string]string{"currency": "미국 달러", "base": "유로"}, "!환율 미국 달러 유로"}, []string{"1달러 ≈ 0.9유로", "1유로 ≈ 1.111달러"}},
		{"ko currency and base, base with a space", "ko", way{map[string]string{"currency": "유로", "base": "미국 달러"}, "!환율 유로 미국 달러"}, []string{"1유로 ≈ 1.111달러", "1달러 ≈ 0.9유로"}},
		{"ko currency and base by code", "ko", way{map[string]string{"currency": "USD", "base": "EUR"}, "!환율 USD EUR"}, []string{"1달러 ≈ 0.9유로", "1유로 ≈ 1.111달러"}},
		{"ko the home currency against a base", "ko", way{map[string]string{"currency": "원", "base": "엔"}, "!환율 원 엔"}, []string{"1,000원 ≈ 109.6엔", "100엔 ≈ 912.2원"}},
		{"en currency and base", "en", way{map[string]string{"currency": "usd", "base": "eur"}, "!fx usd eur"}, []string{"1 USD ≈ 0.9 EUR", "1 EUR ≈ 1.111 USD"}},
		{"en currency and base by name", "en", way{map[string]string{"currency": "Japanese Yen", "base": "euro"}, "!fx japanese yen euro"}, []string{"1 JPY ≈ 0.006081 EUR", "1 EUR ≈ 164.4 JPY"}},
		// amount of the currency, in the base or the home currency
		{"ko amount of a currency in each", "ko", way{map[string]string{"currency": "엔", "amount": "50000"}, "!환율 50000 엔"}, []string{"50,000엔 ≈ 456,100원", "50,000엔 ≈ 337.8달러", "50,000엔 ≈ 304.1유로", "50,000엔 ≈ 2,280위안", "50,000엔 ≈ 253.4파운드"}},
		{"ko amount of a currency in a base", "ko", way{map[string]string{"currency": "엔", "base": "달러", "amount": "5000"}, "!환율 엔 달러 5000"}, []string{"5,000엔 ≈ 33.78달러", "1달러 ≈ 148엔"}},
		{"ko amount of a currency in a base, number first", "ko", way{map[string]string{"currency": "달러", "base": "유로", "amount": "100"}, "!환율 100 달러 유로"}, []string{"100달러 ≈ 90유로", "1유로 ≈ 1.111달러"}},
		{"en amount of a currency in a base", "en", way{map[string]string{"currency": "gbp", "base": "eur", "amount": "100"}, "!fx 100 gbp eur"}, []string{"100 GBP ≈ 120 EUR", "1 EUR ≈ 0.8333 GBP"}},
		// amount with no currency: that much of the base (or home) in each board currency
		{"ko amount in home", "ko", way{map[string]string{"amount": "10000"}, "!환율 10000"}, koWonInEach},
		{"ko amount of the base", "ko", way{slash: map[string]string{"base": "엔", "amount": "100"}}, []string{"100엔 ≈ 912.2원", "100엔 ≈ 0.6757달러", "100엔 ≈ 0.6081유로", "100엔 ≈ 4.561위안", "100엔 ≈ 0.5068파운드"}},
		{"en amount of the base", "en", way{slash: map[string]string{"base": "eur", "amount": "10"}}, []string{"10 EUR ≈ 11.11 USD", "10 EUR ≈ 15,000 KRW", "10 EUR ≈ 1,644 JPY", "10 EUR ≈ 75 CNY", "10 EUR ≈ 8.333 GBP"}},
		{"ko amount of the home currency named", "ko", way{map[string]string{"currency": "원", "amount": "10000"}, "!환율 원 10000"}, koWonInEach},
		// the currency is the base: the same as a base alone; one that is the home currency stays the
		// subject, quoted against the rest of the board
		{"ko currency equals base", "ko", way{map[string]string{"currency": "엔화", "base": "JPY"}, "!환율 엔화 JPY"}, koInJPY},
		{"ko currency equals home", "ko", way{map[string]string{"currency": "원"}, "!환율 원"}, []string{"1,000원 ≈ 0.7407달러", "1,000원 ≈ 109.6엔", "1,000원 ≈ 0.6667유로", "1,000원 ≈ 5위안", "1,000원 ≈ 0.5556파운드"}},
		{"ko currency equals base, with an amount", "ko", way{map[string]string{"currency": "엔", "base": "엔화", "amount": "100"}, "!환율 100 엔 엔화"}, []string{"100엔 ≈ 912.2원", "100엔 ≈ 0.6757달러", "100엔 ≈ 0.6081유로", "100엔 ≈ 4.561위안", "100엔 ≈ 0.5068파운드"}},
		{"en currency equals home", "en", way{map[string]string{"currency": "usd"}, "!fx usd"}, []string{"1 USD ≈ 1,350 KRW", "1 USD ≈ 148 JPY", "1 USD ≈ 0.9 EUR", "1 USD ≈ 6.75 CNY", "1 USD ≈ 0.75 GBP"}},
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

// The home currency is the currency of the person's region: the region in the request's locale, else the
// region Discord reports when it is the same language, else the likely region of the language.
func TestFXHomeCurrencyFollowsTheRegion(t *testing.T) {
	for _, tc := range []struct {
		name, locale, tag string
		home              string
		rows              int // the board valued in home: five major currencies, home included, minus home
	}{
		{"ko", "ko", "", "KRW", 5},
		{"ko-KR", "ko-KR", "", "KRW", 5},
		{"ja", "ja", "", "JPY", 5},
		{"en-US", "en-US", "", "USD", 5},
		{"en-GB", "en-GB", "", "GBP", 5},
		{"es-ES", "es-ES", "", "EUR", 5},
		{"es-419 is not a country", "es-419", "", "USD", 5},
		{"zh-CN", "zh-CN", "", "CNY", 5},
		{"pt-BR", "pt-BR", "", "BRL", 6},
		{"a bare language takes its likely region", "es", "", "EUR", 5},
		// a saved tag without a region yields to the region Discord reports, whatever its language
		{"saved en, Discord en-GB", "en", "en-GB", "GBP", 5},
		{"saved en, Discord pt-BR", "en", "pt-BR", "BRL", 6},
		{"saved es, Discord en-GB", "es", "en-GB", "GBP", 5},
		{"saved ko, Discord en-GB", "ko", "en-GB", "GBP", 5},
		{"saved ko, Discord ko", "ko", "ko", "KRW", 5},
		// a tag that names no region (ko) falls back to the saved language's likely region; one that
		// names a region without one currency (es-419) is still the tag used, and gives USD
		{"saved es, Discord ko", "es", "ko", "EUR", 5},
		{"saved es, Discord es-419", "es", "es-419", "USD", 5},
		// a region the saved language names itself wins over Discord's
		{"en-US, Discord en-GB", "en-US", "en-GB", "USD", 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			embed, out := fxRun(t, InvokeContext{Locale: tc.locale, Tag: tc.tag, UserID: "tester"}, nil)
			if out["base"] != tc.home {
				t.Errorf("home = %v, want %s (%s)", out["base"], tc.home, embed.Description)
			}
			if lines := strings.Split(embed.Description, "\n"); len(lines) != tc.rows {
				t.Errorf("%d lines, want %d: %v", len(lines), tc.rows, lines)
			}
			if tc.home != "KRW" {
				// the won is a managed currency: every board of a non-Korean home shows its row too
				won := currency.New(messages).Name(tc.locale, "KRW")
				if won == "" {
					won = "KRW"
				}
				if !strings.Contains(embed.Description, won+" ≈") && !strings.Contains(embed.Description, "≈ "+won) {
					t.Errorf("no won row on a board of a home that is not Korean: %v", embed.Description)
				}
			}
		})
	}
}

// The Discord locale reaches the action as an input of the command, only when there is one.
func TestFXBuildCarriesTheDiscordLocaleTag(t *testing.T) {
	cmd, _ := ByName("fx")
	_, in := cmd.Build(nil, InvokeContext{Locale: "en", Tag: "en-GB"})
	if in["locale_tag"] != "en-GB" {
		t.Errorf("input = %v", in)
	}
	if _, in = cmd.Build(nil, InvokeContext{Locale: "en"}); in["locale_tag"] != nil {
		t.Errorf("a tag was sent without one: %v", in)
	}
}

// A message has no locale of its own: the server's preferred one stands in.
func TestFXPrefixReadsTheServersPreferredLocale(t *testing.T) {
	d := newFXDelivery(t, nil)
	if err := d.g.sess.State.GuildAdd(&discordgo.Guild{ID: "g", PreferredLocale: "en-GB"}); err != nil {
		t.Fatal(err)
	}
	if got := guildTag(d.g.sess, "g"); got != "en-GB" {
		t.Fatalf("guildTag = %q", got)
	}
	if guildTag(d.g.sess, "") != "" || guildTag(d.g.sess, "other") != "" {
		t.Error("a tag for a server that is not known")
	}
}

// The board is the six managed currencies in every locale: an English board shows a won row too, and a
// base among the six leaves itself out but keeps the rest.
func TestFXBoardOffersTheSixEverywhere(t *testing.T) {
	embed := fxEmbed(t, "en", nil, false)
	if !strings.Contains(embed.Description, "KRW ≈") {
		t.Errorf("no won row on an English board: %v", embed.Description)
	}
	embed = fxEmbed(t, "en", map[string]string{"base": "KRW"}, false)
	if !strings.Contains(embed.Description, "USD ≈") || !strings.Contains(embed.Title, "KRW") {
		t.Errorf("base won: %v / %s", embed.Description, embed.Title)
	}
}
