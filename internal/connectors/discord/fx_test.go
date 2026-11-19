package discord

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"masamong/internal/config"
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
		act, input := cmd.Build(map[string]string{"base": tc.base}, InvokeContext{Locale: tc.locale})
		if act != "finance.fx" || input["base"] != tc.want {
			t.Errorf("locale %q, base %q: got %s %v, want finance.fx %s", tc.locale, tc.base, act, input, tc.want)
		}
	}
}

func TestFXInvalidCurrencyShowsAvailableCommands(t *testing.T) {
	svc := svcForTest(t)
	g := &Gateway{svc: svc, cfg: config.Discord{RegisterCommands: true}}
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusBadRequest)
		switch r.URL.Path {
		case "/latest/ZZZ":
			fmt.Fprint(w, `{"result":"error","error-type":"unsupported-code"}`)
		default:
			fmt.Fprint(w, `{"result":"error","error-type":"invalid-key"}`)
		}
	}))
	defer server.Close()
	adapter := svc.Registry().FX
	adapter.OpenBase, adapter.KeyedBase = server.URL, server.URL
	invoke := func(name, base string) (string, string) {
		t.Helper()
		cmd, _ := ByName(name)
		ictx := InvokeContext{Locale: "ko", UserID: "tester"}
		res := g.executeCommand(context.Background(), requestFor(cmd, map[string]string{"base": base}, ictx, ""))
		embed := g.renderInvoked(res, cmd, ictx).Embed
		return embed.Title, embed.Description
	}
	wantTitle, wantList := invoke("help", "")
	if !strings.Contains(wantList, "/poll") {
		t.Fatalf("missing commands in help: %s", wantList)
	}
	for _, base := range []string{"달러", "원", "U$D", "USD KRW", "zzz"} {
		title, list := invoke("fx", base)
		if title != wantTitle || list != wantList {
			t.Errorf("input %q: got %q %q, want the current help list", base, title, list)
		}
	}
	if calls.Load() != 1 {
		t.Errorf("expected only the well-formed ZZZ to reach the provider, got %d calls", calls.Load())
	}
	if title, _ := invoke("fx", "USD"); title == wantTitle {
		t.Error("a provider credential error was mistaken for an input error")
	}
}
