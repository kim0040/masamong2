package application

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/errcode"
	"masamong/internal/lang"
)

// fxRates serves ExchangeRate-API's latest/{base} from fixed dollar prices, and counts the requests.
func fxRates(t *testing.T, s *Service) *atomic.Int32 {
	t.Helper()
	var calls atomic.Int32
	perDollar := map[string]float64{"USD": 1, "KRW": 1400, "JPY": 150, "EUR": 0.9, "CNY": 7, "GBP": 0.8, "VND": 26000}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		base := strings.TrimPrefix(r.URL.Path, "/latest/")
		w.Header().Set("Content-Type", "application/json")
		if base == "BTN" {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"result":"error","error-type":"unsupported-code"}`)
			return
		}
		rates := map[string]float64{}
		for code, v := range perDollar {
			rates[code] = v / perDollar[base]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"result": "success", "base_code": base, "rates": rates, "time_last_update_unix": 1757894401})
	}))
	t.Cleanup(server.Close)
	adapter := s.Registry().FX
	adapter.OpenBase, adapter.KeyedBase = server.URL, server.URL
	return &calls
}

// fxRun runs finance.fx the way each origin does: the chat and task tool runners, the CLI and the console.
func fxRun(t *testing.T, s *Service, via string, input map[string]any) chat.ToolResult {
	t.Helper()
	ctx := context.Background()
	p, _ := lang.Get("ko")
	if via == viaChat || via == viaTask {
		return s.turnTool(ChatInput{GuildID: "g", ChannelID: "c", SpeakerID: "tester"}, chat.Voice{Pack: p}, via)(ctx, "finance.fx", input)
	}
	raw, _ := json.Marshal(input)
	res := s.Execute(ctx, action.Request{Action: "finance.fx", Locale: "ko", RequestID: via, Input: raw})
	if !res.OK {
		return chat.ToolResult{Status: res.Error.Code, Error: res.Error.Message}
	}
	return chat.ToolResult{OK: true, Data: res.Output}
}

var fxOrigins = []string{viaChat, viaTask, "cli", "web"}

// A bad currency is an input error on every origin, found before the quota or the provider is touched;
// a code the service lacks is the only late one; the raw rates keep their shape.
func TestFXActionSurfaces(t *testing.T) {
	s := fixg1Svc(t, []string{"serper"}, nil, nil)
	calls := fxRates(t, s)
	for _, via := range fxOrigins {
		t.Run(via, func(t *testing.T) {
			r := fxRun(t, s, via, map[string]any{"base": "KRW"})
			if !r.OK || r.Data["base"] != "KRW" {
				t.Fatalf("KRW failed %+v", r)
			}
			rates := r.Data["rates"].(map[string]any)
			if rates["USD"] != nil || rates["JPY"] != 150.0/1400.0 {
				t.Fatalf("raw default rates changed: %v", rates)
			}
			for _, bad := range []map[string]any{{"base": "U$D"}, {"base": "ZZZ"}, {"base": "   "}, {"base": ""}, {"base": "달라"}, {"base": "USD", "symbols": []string{"U$D"}}} {
				before := calls.Load()
				r = fxRun(t, s, via, bad)
				if r.Status != errcode.InvalidInput || calls.Load() != before {
					t.Fatalf("%v: result=%+v provider calls=%d", bad, r, calls.Load()-before)
				}
			}
			before := calls.Load()
			r = fxRun(t, s, via, map[string]any{"base": "BTN"})
			if r.Status != errcode.InvalidInput || calls.Load() != before+1 {
				t.Fatalf("an ISO code the service lacks: %+v calls=%d", r, calls.Load()-before)
			}
			if via == "cli" || via == "web" {
				if r = fxRun(t, s, via, nil); !r.OK || r.Data["base"] != "USD" {
					t.Fatalf("omitted base: %+v", r)
				}
			}
		})
	}
}

// What the person typed is read like a code: a name in any language, any case.
func TestFXActionReadsCurrencyNames(t *testing.T) {
	s := fixg1Svc(t, []string{"serper"}, nil, nil)
	fxRates(t, s)
	for _, name := range []string{"엔화", "Japanese Yen", " jpy ", "円"} {
		r := fxRun(t, s, "cli", map[string]any{"base": name, "symbols": "원"})
		if !r.OK || r.Data["base"] != "JPY" {
			t.Fatalf("%q: %+v", name, r)
		}
		if _, ok := r.Data["rates"].(map[string]any)["KRW"]; !ok {
			t.Fatalf("%q: symbol 원 was not read as KRW: %v", name, r.Data["rates"])
		}
	}
}

// An input error is an answer: nothing carries on from it into a conversation.
func TestFXInputErrorIsNotUnanswered(t *testing.T) {
	s := fixg1Svc(t, []string{"serper"}, nil, nil)
	fxRates(t, s)
	for _, in := range []string{"U$D", "BTN"} {
		raw, _ := json.Marshal(map[string]any{"base": in})
		res := s.Execute(context.Background(), action.Request{Action: "finance.fx", Locale: "ko", Input: raw})
		if res.OK || !IsFXInputError(res) || Unanswered(res) {
			t.Errorf("%s: want a terminal input error, got %+v unanswered=%v", in, res, Unanswered(res))
		}
	}
	other := action.Fail(action.Request{Action: "finance.fx"}, errcode.InvalidInput, "generic.error", "x", nil)
	if !Unanswered(other) {
		t.Error("an unrelated failure must still carry on")
	}
}
