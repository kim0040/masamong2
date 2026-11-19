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
// a code the service lacks is the only late one; the raw rates keep their shape. (A blank base is USD for
// direct callers: TestFXRawInputKeepsItsOldMeaning.)
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
			for _, bad := range []map[string]any{{"base": "U$D"}, {"base": "ZZZ"}, {"base": "달라"}, {"base": "USD", "symbols": []string{"U$D"}}} {
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

func quotesOf(t *testing.T, data map[string]any) []map[string]any {
	t.Helper()
	raw, err := json.Marshal(data["quotes"])
	if err != nil {
		t.Fatal(err)
	}
	var out []map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		t.Fatalf("quotes %s: %v", raw, err)
	}
	return out
}

// Chat, tasks, the CLI and the console get the raw rates as before, plus quotes in the unit people use,
// in the language of the request (chat: the answer language).
func TestFXQuotesNextToRawRates(t *testing.T) {
	s := fixg1Svc(t, []string{"serper"}, nil, nil)
	fxRates(t, s)
	for _, via := range fxOrigins {
		t.Run(via, func(t *testing.T) {
			r := fxRun(t, s, via, map[string]any{"base": "JPY", "symbols": []string{"KRW", "USD"}})
			if !r.OK || r.Data["base"] != "JPY" || r.Data["source"] == nil || r.Data["updated_at"] == nil {
				t.Fatalf("raw fields changed: %+v", r)
			}
			rates := r.Data["rates"].(map[string]any)
			if len(rates) != 2 || rates["KRW"] != 1400.0/150.0 {
				t.Fatalf("raw rates changed: %v", rates)
			}
			q := quotesOf(t, r.Data)
			if len(q) != 2 {
				t.Fatalf("quotes = %v", q)
			}
			if q[0]["from"] != "JPY" || q[0]["to"] != "KRW" || q[0]["unit"] != 100.0 || q[0]["value"] != 933.3 || q[0]["text"] != "100엔 ≈ 933.3원" {
				t.Errorf("first quote = %v", q[0])
			}
			if q[1]["to"] != "USD" || q[1]["text"] != "1,000엔 ≈ 6.667달러" {
				t.Errorf("second quote = %v", q[1])
			}
		})
	}
}

// With an amount the quote is that amount; a name works for the base and the symbols.
func TestFXAmountAndNames(t *testing.T) {
	s := fixg1Svc(t, []string{"serper"}, nil, nil)
	calls := fxRates(t, s)
	for _, amount := range []any{"50,000", "50000", 50000, 50000.0, json.Number("50000")} {
		r := fxRun(t, s, "cli", map[string]any{"base": "엔화", "symbols": "원", "amount": amount})
		if !r.OK {
			t.Fatalf("%v: %+v", amount, r)
		}
		if q := quotesOf(t, r.Data); len(q) != 1 || q[0]["text"] != "50,000엔 ≈ 466,700원" || q[0]["unit"] != 50000.0 {
			t.Fatalf("%v: quotes %v", amount, q)
		}
	}
	for _, bad := range []any{"abc", "-5", "0", 0, -1, "1e3", "", "5만", true, "1000000000000000000"} {
		before := calls.Load()
		r := fxRun(t, s, "cli", map[string]any{"base": "USD", "amount": bad})
		if r.Status != errcode.InvalidInput || calls.Load() != before {
			t.Errorf("amount %v: %+v, provider calls %d", bad, r, calls.Load()-before)
		}
	}
}

// The command's request (home currency, what was typed): both directions, one provider call.
func TestFXHomeRequest(t *testing.T) {
	s := fixg1Svc(t, []string{"serper"}, nil, nil)
	calls := fxRates(t, s)
	res := s.Execute(context.Background(), action.Request{Action: "finance.fx", Locale: "ko", Input: []byte(`{"home":"KRW","currency":"엔화"}`)})
	if !res.OK || res.Output["base"] != "KRW" || calls.Load() != 1 {
		t.Fatalf("%+v calls=%d", res, calls.Load())
	}
	q := quotesOf(t, res.Output)
	if len(q) != 2 || q[0]["text"] != "100엔 ≈ 933.3원" || q[1]["text"] != "1,000원 ≈ 107.1엔" {
		t.Fatalf("quotes %v", q)
	}
	if rates := res.Output["rates"].(map[string]any); len(rates) != 1 || rates["JPY"] == nil {
		t.Errorf("rates = %v", rates)
	}
	for _, bad := range []string{`{"home":"KRW","currency":""}`, `{"home":"KRW","currency":"   "}`, `{"home":"KRW","currency":"U$D"}`, `{"home":"KRW","amount":"x"}`} {
		before := calls.Load()
		res = s.Execute(context.Background(), action.Request{Action: "finance.fx", Locale: "ko", Input: []byte(bad)})
		if !IsFXInputError(res) || calls.Load() != before {
			t.Errorf("%s: %+v calls %d", bad, res, calls.Load()-before)
		}
	}
}

// The model that words the answer is told, in the result itself and in its own language, to quote the
// quotes: the tool's description is only seen where the tool is chosen.
func TestFXResultCarriesTheAnswerHint(t *testing.T) {
	s := fixg1Svc(t, []string{"serper"}, nil, nil)
	fxRates(t, s)
	for _, tag := range []string{"en", "ko", "ja", "es", "zh-Hans"} {
		t.Run(tag, func(t *testing.T) {
			p, _ := lang.Get(tag)
			voice := chat.Voice{Pack: p}
			result := s.turnTool(ChatInput{GuildID: "g", ChannelID: "c", SpeakerID: "u"}, voice, viaChat)(context.Background(), "finance.fx", map[string]any{"base": "JPY", "symbols": "KRW"})
			if !result.OK {
				t.Fatalf("tool error: %v", result)
			}
			hint, _ := result.Data["answer_hint"].(string)
			if hint == "" || !strings.Contains(hint, "quotes[].text") || hint == s.msg(action.Request{Locale: "en"}, "fx.answer_hint") && tag != "en" {
				t.Fatalf("answer_hint = %q", hint)
			}
			var all []string
			for _, m := range chat.BuildAnswerMessages(voice, chat.AnswerInput{System: chat.ComposeSystem(voice, "", "", ""), Message: "FX rate", Tools: []chat.ToolResult{result}}) {
				all = append(all, m.Content)
			}
			if !strings.Contains(strings.Join(all, "\n"), hint) {
				t.Errorf("the answer prompt lacks the hint %q", hint)
			}
		})
	}
}

// Direct callers (CLI, console, chat, tasks) keep what the action always did with their input: a missing,
// blank or null base is USD, blank symbols are skipped, a list of only blanks selects nothing. A name that
// is no currency is an error on every surface; a blank option is invalid only on a command's own request.
func TestFXRawInputKeepsItsOldMeaning(t *testing.T) {
	s := fixg1Svc(t, []string{"serper"}, nil, nil)
	calls := fxRates(t, s)
	for _, via := range fxOrigins {
		t.Run(via, func(t *testing.T) {
			for _, base := range []any{nil, "", "   ", "\t"} {
				r := fxRun(t, s, via, map[string]any{"base": base, "symbols": []string{"KRW"}})
				if !r.OK || r.Data["base"] != "USD" || r.Data["rates"].(map[string]any)["KRW"] != 1400.0 {
					t.Errorf("base %#v: %+v", base, r)
				}
			}
			r := fxRun(t, s, via, map[string]any{"base": "KRW", "symbols": []string{"USD", ""}})
			if rates, _ := r.Data["rates"].(map[string]any); !r.OK || len(rates) != 1 || rates["USD"] != 1.0/1400.0 {
				t.Errorf("a blank symbol is ignored: %+v", r)
			}
			r = fxRun(t, s, via, map[string]any{"base": "KRW", "symbols": []string{""}})
			if rates, _ := r.Data["rates"].(map[string]any); !r.OK || len(rates) != 0 || len(quotesOf(t, r.Data)) != 0 {
				t.Errorf("only blank symbols select nothing: %+v", r)
			}
			if _, has := r.Data["answer_hint"]; has {
				t.Errorf("a hint without quotes: %v", r.Data)
			}
			for _, empty := range []any{nil, []string{}, ""} {
				r = fxRun(t, s, via, map[string]any{"base": "KRW", "symbols": empty})
				if rates, _ := r.Data["rates"].(map[string]any); !r.OK || len(rates) != 5 { // the default five (the base itself is one of them)
					t.Errorf("no symbols means the defaults (%#v): %+v", empty, r)
				}
			}
			before := calls.Load()
			for _, bad := range []map[string]any{{"base": "KRW", "symbols": []string{"USD", "ZZZ"}}, {"base": "ZZZ"}, {"base": "U$D"}} {
				if r := fxRun(t, s, via, bad); r.OK || r.Status != errcode.InvalidInput {
					t.Errorf("%v: %+v, want invalid input", bad, r)
				}
			}
			if calls.Load() != before {
				t.Error("a refused name cost a provider call")
			}
		})
	}
	// a command's own blank option is not the default
	before := calls.Load()
	res := s.Execute(context.Background(), action.Request{Action: "finance.fx", Locale: "ko", Input: []byte(`{"home":"","currency":"  "}`)})
	if !IsFXInputError(res) || calls.Load() != before {
		t.Errorf("blank command option: %+v", res)
	}
}
