package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
)

func TestFXDefaultCurrency(t *testing.T) {
	cmd, _ := ByName("fx")
	for _, tc := range []struct{ locale, base, want string }{
		{"ko", "", "KRW"},
		{"ko-KR", "", "KRW"},
		{"en-US", "", "USD"},
		{"ja", "", "USD"},
		{"ko", "USD", "USD"},
		{"en", "JPY", "JPY"},
	} {
		opts := map[string]string{}
		if tc.base != "" {
			opts["currency"] = tc.base
		}
		act, input := cmd.Build(opts, InvokeContext{Locale: tc.locale})
		if act != "finance.fx" || input["base"] != tc.want {
			t.Errorf("locale %q, base %q: got %s %v, want finance.fx %s", tc.locale, tc.base, act, input, tc.want)
		}
	}
}

func TestFXCardShowsOneForeignUnitInSelectedCurrency(t *testing.T) {
	svc := svcForTest(t)
	g := &Gateway{svc: svc}
	unitsPerDollar := map[string]float64{"USD": 1, "KRW": 1350, "JPY": 160, "CNY": 6.75, "EUR": 0.9, "GBP": 0.75, "CHF": 0.8}
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
	for _, tc := range []struct {
		locale, option, base string
		rows                 int
		want                 []string
	}{
		{"ko", "", "KRW", 5, []string{"1 USD ≈ 1,350 KRW", "1 JPY ≈ 8.4375 KRW", "1 CNY ≈ 200 KRW"}},
		{"ko", "jpy", "JPY", 5, []string{"1 USD ≈ 160 JPY"}},
		{"en", "", "USD", 5, []string{"1 JPY ≈ 0.00625 USD"}},
		{"en", "CHF", "CHF", 6, []string{"1 USD ≈ 0.8 CHF"}},
	} {
		t.Run(tc.locale+"_"+tc.base, func(t *testing.T) {
			ictx := InvokeContext{Locale: tc.locale, UserID: "tester"}
			opts := map[string]string{}
			if tc.option != "" {
				opts["currency"] = tc.option
			}
			res := g.svc.Execute(context.Background(), requestFor(cmd, opts, ictx, ""))
			if !res.OK || res.Output["base"] != tc.base {
				t.Fatalf("unexpected action result: %+v", res)
			}
			embed := g.renderInvoked(res, cmd, ictx).Embed
			lines := strings.Split(embed.Description, "\n")
			if len(lines) != tc.rows || strings.Contains(embed.Description, "1 "+tc.base+" ≈") {
				t.Fatalf("unexpected comparison currencies: %v", lines)
			}
			for _, want := range tc.want {
				if !slices.Contains(lines, want) {
					t.Errorf("missing %q in %v", want, lines)
				}
			}
			if len(embed.Fields) != 1 || embed.Footer == nil {
				t.Error("missing update time or source")
			}
		})
	}
}
