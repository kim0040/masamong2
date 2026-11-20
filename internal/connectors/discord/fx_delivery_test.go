package discord

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
)

// fxDelivery runs one /환율 or !환율 against a fake Discord and a local rate service.
type fxDelivery struct {
	g     *Gateway
	fake  *fakeDiscord
	calls atomic.Int32
	paths []string
}

func newFXDelivery(t *testing.T, tool func(*Gateway)) *fxDelivery {
	t.Helper()
	d := &fxDelivery{}
	d.g, d.fake = gatewayWithFakeDiscord(t)
	d.g.ActionTimeout = 2 * time.Second
	d.g.cfg.RegisterCommands, d.g.cfg.MessageContent, d.g.cfg.CommandPrefix = true, true, "!"
	if res := d.g.svc.Execute(context.Background(), action.Request{Action: "access.policy", Input: mustJSON(map[string]any{"server_approval": false})}); !res.OK {
		t.Fatalf("access policy: %+v", res)
	}
	if tool != nil {
		tool(d.g)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		d.calls.Add(1)
		d.paths = append(d.paths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		base := strings.TrimPrefix(r.URL.Path, "/latest/")
		if base == "BTN" { // a real ISO code the service has no rates for
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"result":"error","error-type":"unsupported-code"}`)
			return
		}
		fmt.Fprintf(w, `{"result":"success","base_code":%q,"rates":{"USD":0.00072,"JPY":0.1075,"EUR":0.00066,"CNY":0.0051,"GBP":0.00056,"KRW":1}}`, base)
	}))
	t.Cleanup(server.Close)
	adapter := d.g.svc.Registry().FX
	adapter.OpenBase, adapter.KeyedBase = server.URL, server.URL
	return d
}

func (d *fxDelivery) slash(t *testing.T, opts ...*discordgo.ApplicationCommandInteractionDataOption) {
	t.Helper()
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "i", AppID: "app", Token: "tok", Type: discordgo.InteractionApplicationCommand,
		ChannelID: "c", GuildID: "g", Locale: discordgo.Korean, Member: &discordgo.Member{User: &discordgo.User{ID: "u", Username: "tester"}},
		Data: discordgo.ApplicationCommandInteractionData{Name: "fx", Options: opts}}}
	d.g.handleCommand(d.g.sess, i)
	if !d.g.Wait(5 * time.Second) {
		t.Fatal("gateway did not settle")
	}
}

func (d *fxDelivery) prefix(t *testing.T, text string) {
	t.Helper()
	m := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "m", ChannelID: "c", GuildID: "g", Content: text, Author: &discordgo.User{ID: "u", Username: "tester"}}}
	cmd, opts, ok := ParsePrefix("!", text)
	if !ok {
		t.Fatalf("%q did not parse as a command", text)
	}
	d.g.runCommand(d.g.sess, m, cmd, opts)
	if !d.g.Wait(5 * time.Second) {
		t.Fatal("gateway did not settle")
	}
}

func strOption(name, value string) *discordgo.ApplicationCommandInteractionDataOption {
	return &discordgo.ApplicationCommandInteractionDataOption{Name: name, Type: discordgo.ApplicationCommandOptionString, Value: value}
}

func (d *fxDelivery) deferredFlags(t *testing.T) int {
	t.Helper()
	cb, ok := d.fake.find(http.MethodPost, "/callback")
	if !ok {
		t.Fatal("missing slash callback")
	}
	var p struct {
		Type int
		Data struct{ Flags int }
	}
	if err := json.Unmarshal([]byte(cb.body), &p); err != nil || p.Type != 5 {
		t.Fatalf("callback %q: %v", cb.body, err)
	}
	return p.Data.Flags
}

const ephemeral = int(discordgo.MessageFlagsEphemeral)

// The region a card's home currency is read from, in order: the region the person's saved language
// names itself, else the region Discord's tag names (the interaction's for a slash command, the
// server's for a message), else the likely region of the saved language, else USD.
func TestFXHomeFollowsTheFirstRegionThatNamesOne(t *testing.T) {
	saved := func(t *testing.T, d *fxDelivery, locale string) {
		t.Helper()
		res := d.g.svc.Execute(context.Background(), action.Request{Action: "preferences.set",
			Input:   mustJSON(map[string]any{"locale": locale}),
			Invoker: &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "u"}})
		if !res.OK {
			t.Fatalf("saved locale: %+v", res.Error)
		}
	}
	t.Run("saved English, a slash command in pt-BR: Brazil's home, the card in English", func(t *testing.T) {
		d := newFXDelivery(t, nil)
		saved(t, d, "en")
		i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "i", AppID: "app", Token: "tok",
			Type: discordgo.InteractionApplicationCommand, ChannelID: "c", GuildID: "g",
			Locale: discordgo.Locale("pt-BR"), Member: &discordgo.Member{User: &discordgo.User{ID: "u", Username: "tester"}},
			Data: discordgo.ApplicationCommandInteractionData{Name: "fx"}}}
		d.g.handleCommand(d.g.sess, i)
		if !d.g.Wait(5 * time.Second) {
			t.Fatal("gateway did not settle")
		}
		if len(d.paths) != 1 || d.paths[0] != "/latest/BRL" {
			t.Fatalf("provider paths %v, want /latest/BRL", d.paths)
		}
		edit, ok := d.fake.find(http.MethodPatch, "/messages/@original")
		if !ok || !strings.Contains(edit.body, "Exchange rates") {
			t.Errorf("the card is not in the saved language: %v", edit.body)
		}
	})
	for _, tc := range []struct {
		saved, want string
	}{
		{"en", "/latest/GBP"},    // no region of its own: the guild's
		{"en-US", "/latest/USD"}, // its own region wins over the guild's
		{"es", "/latest/GBP"},    // the guild's region beats the language's likely one
	} {
		t.Run("guild en-GB, saved "+tc.saved, func(t *testing.T) {
			d := newFXDelivery(t, nil)
			saved(t, d, tc.saved)
			if err := d.g.sess.State.GuildAdd(&discordgo.Guild{ID: "g", PreferredLocale: "en-GB"}); err != nil {
				t.Fatal(err)
			}
			d.prefix(t, "!fx")
			if len(d.paths) != 1 || d.paths[0] != tc.want {
				t.Errorf("provider paths %v, want %s", d.paths, tc.want)
			}
		})
	}
}

// A currency that cannot be read, or that the rate service lacks, is told to the member alone, with how
// to write one. Nothing is rewritten into another command, and a malformed one costs no provider call.
func TestFXSlashInputErrorsAreAnsweredPrivately(t *testing.T) {
	for _, tc := range []struct {
		name, input string
		wantCalls   int32
	}{
		{"malformed", "U$D", 0},
		{"blank is not the default", "   ", 0},
		{"not an ISO code", "ZZZ", 0},
		{"unknown word", "달라", 0},
		{"valid code the service lacks", "BTN", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := newFXDelivery(t, nil)
			d.slash(t, strOption("currency", tc.input))
			if got := d.calls.Load(); got != tc.wantCalls {
				t.Errorf("provider calls = %d (%v), want %d", got, d.paths, tc.wantCalls)
			}
			if flags := d.deferredFlags(t); flags != 0 {
				t.Errorf("deferred flags = %d, want the public default (a good answer stays public)", flags)
			}
			if _, ok := d.fake.find(http.MethodDelete, "/messages/@original"); !ok {
				t.Error("the public deferred response was not deleted")
			}
			if _, ok := d.fake.find(http.MethodPatch, "/messages/@original"); ok {
				t.Error("the error was written into the public response")
			}
			follow, ok := d.fake.find(http.MethodPost, "/webhooks/app/tok")
			if !ok {
				t.Fatalf("no follow-up: %+v", d.fake.calls)
			}
			var body struct {
				Flags  int
				Embeds []struct{ Title, Description string }
			}
			if err := json.Unmarshal([]byte(follow.body), &body); err != nil {
				t.Fatal(err)
			}
			if body.Flags != ephemeral {
				t.Errorf("follow-up flags = %d, want ephemeral %d", body.Flags, ephemeral)
			}
			if strings.Contains(follow.body, "/poll") {
				t.Error("the help list was sent: the request must stay a finance.fx failure")
			}
			if len(body.Embeds) != 1 || !strings.Contains(body.Embeds[0].Description, "통화") {
				t.Errorf("follow-up should say how to write a currency, got %s", follow.body)
			}
		})
	}
}

// A good answer is still public.
func TestFXSlashSuccessStaysPublic(t *testing.T) {
	d := newFXDelivery(t, nil)
	d.slash(t)
	if flags := d.deferredFlags(t); flags != 0 {
		t.Errorf("flags = %d, want public", flags)
	}
	if _, ok := d.fake.find(http.MethodDelete, "/messages/@original"); ok {
		t.Error("a good answer was deleted")
	}
	edit, ok := d.fake.find(http.MethodPatch, "/messages/@original")
	if !ok || !strings.Contains(edit.body, "≈") {
		t.Fatalf("no rate card: %+v", d.fake.calls)
	}
}

// The tool's own channel switch decides, whatever the member typed.
func TestFXSlashInvalidInputStillGoesThroughTheToolGate(t *testing.T) {
	d := newFXDelivery(t, func(g *Gateway) {
		if res := g.svc.Execute(context.Background(), action.Request{Action: "tools.configure", Input: mustJSON(map[string]any{"id": "finance.fx", "enabled": "off", "guild_id": "g", "channel_id": "c"})}); !res.OK {
			t.Fatalf("configure: %+v", res)
		}
	})
	d.slash(t, strOption("currency", "U$D"))
	edit, ok := d.fake.find(http.MethodPatch, "/messages/@original")
	if !ok {
		t.Fatalf("no reply: %+v", d.fake.calls)
	}
	if strings.Contains(edit.body, "통화를 알아보지") || strings.Contains(edit.body, "/poll") {
		t.Errorf("the finance.fx switch was skipped: %s", edit.body)
	}
	if d.calls.Load() != 0 {
		t.Error("a provider was called")
	}
}

// An input error ends the command on the prefix surface too: no model reply, whatever help's state.
func TestFXPrefixInputErrorIsTerminal(t *testing.T) {
	for _, helpOff := range []bool{false, true} {
		t.Run(fmt.Sprintf("help off=%v", helpOff), func(t *testing.T) {
			d := newFXDelivery(t, func(g *Gateway) {
				g.svc = gatewayBanterService(t)
				g.cfg.RegisterCommands, g.cfg.MessageContent, g.cfg.CommandPrefix = true, true, "!"
				if helpOff {
					if res := g.svc.Execute(context.Background(), action.Request{Action: "tools.configure", Input: mustJSON(map[string]any{"id": "help.version", "enabled": "off"})}); !res.OK {
						t.Fatalf("configure: %+v", res)
					}
				}
			})
			d.prefix(t, "!환율 U$D")
			reply, ok := d.fake.find(http.MethodPost, "/channels/c/messages")
			if !ok {
				t.Fatalf("no reply: %+v", d.fake.calls)
			}
			if strings.Contains(reply.body, `"content":"reply"`) {
				t.Fatalf("a model answered a validation error: %s", reply.body)
			}
			if !strings.Contains(reply.body, "Write a code such as USD") {
				t.Errorf("reply should say how to write a currency: %s", reply.body)
			}
			if _, typing := d.fake.find(http.MethodPost, "/channels/c/typing"); typing {
				t.Error("the command carried on as a conversation")
			}
			if d.calls.Load() != 0 {
				t.Error("a provider was called")
			}
		})
	}
}

// refuseDelete answers the deletion of the original response with Discord's 403 and passes the rest on.
type refuseDelete struct{ fake *fakeDiscord }

func (r refuseDelete) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method == http.MethodDelete && strings.HasSuffix(req.URL.Path, "/messages/@original") {
		_, _ = r.fake.RoundTrip(req) // recorded
		return &http.Response{StatusCode: http.StatusForbidden, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"code":50013,"message":"denied"}`)), Request: req}, nil
	}
	return r.fake.RoundTrip(req)
}

// When Discord refuses to delete the public deferred response, the input error is not sent at all: a patch
// would be public, and Discord may attach the first follow-up to the public deferred response whatever its
// flags say.
func TestFXSlashInputErrorStaysPrivateWhenTheDeleteFails(t *testing.T) {
	for _, input := range []string{"U$D", "   ", "ZZZ", "BTN"} {
		t.Run(input, func(t *testing.T) {
			d := newFXDelivery(t, nil)
			d.g.sess.Client.Transport = refuseDelete{d.fake}
			d.slash(t, strOption("currency", input))
			if _, ok := d.fake.find(http.MethodDelete, "/messages/@original"); !ok {
				t.Fatal("no deletion attempted")
			}
			if edit, ok := d.fake.find(http.MethodPatch, "/messages/@original"); ok {
				t.Fatalf("the error was patched into the public response: %s", edit.body)
			}
			if follow, ok := d.fake.find(http.MethodPost, "/webhooks/app/tok"); ok {
				t.Fatalf("a follow-up was sent after the failed delete: %s", follow.body)
			}
		})
	}
}

// A typo is refused before the per-user allowance is counted: the next valid request still has it.
func TestFXInvalidInputDoesNotUseTheToolAllowance(t *testing.T) {
	d := newFXDelivery(t, func(g *Gateway) { // before the rate service is wired: configuring rebuilds the providers
		if res := g.svc.Execute(context.Background(), action.Request{Action: "tools.configure", Input: mustJSON(map[string]any{"id": "finance.fx", "per_user_per_day": 1})}); !res.OK {
			t.Fatalf("configure: %v", res.Error)
		}
	})
	req := func(in map[string]any) action.Request {
		in["home"] = "KRW"
		return action.Request{Action: "finance.fx", Locale: "ko", Input: mustJSON(in),
			Invoker: &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "u"}}
	}
	for _, bad := range []map[string]any{{"currency": "U$D"}, {"currency": "   "}, {"amount": "-3"}} {
		if res := d.g.svc.Execute(context.Background(), req(bad)); res.OK || res.Error.MessageKey == "" || res.Error.Code == "TOOL_LIMITED" || d.calls.Load() != 0 {
			t.Fatalf("%v: %+v provider=%d", bad, res.Error, d.calls.Load())
		}
	}
	if res := d.g.svc.Execute(context.Background(), req(map[string]any{"currency": "JPY"})); !res.OK || d.calls.Load() != 1 {
		t.Fatalf("valid after typos: %+v provider=%d", res.Error, d.calls.Load())
	}
	if res := d.g.svc.Execute(context.Background(), req(map[string]any{"currency": "EUR"})); res.OK || res.Error.Code != "TOOL_LIMITED" {
		t.Fatalf("the one allowed call was not counted: %+v", res)
	}
}

// The caller's access is answered first, whatever was typed.
func TestFXBlockedUserIsRefusedBeforeInputIsRead(t *testing.T) {
	d := newFXDelivery(t, nil)
	if res := d.g.svc.Execute(context.Background(), action.Request{Action: "access.policy", Input: mustJSON(map[string]any{"guild_id": "223456789012345678", "block": "123456789012345678"})}); !res.OK {
		t.Fatalf("block: %v", res.Error)
	}
	res := d.g.svc.Execute(context.Background(), action.Request{Action: "finance.fx", Locale: "ko", Input: mustJSON(map[string]any{"home": "KRW", "currency": "U$D"}),
		Invoker: &action.Invoker{Via: "command", GuildID: "223456789012345678", ChannelID: "c", UserID: "123456789012345678"}})
	if res.OK || res.Error.MessageKey != "access.blocked" {
		t.Fatalf("a blocked member was told about their typo: %+v", res.Error)
	}
}

// The prefix words and the action read an amount in one syntax, the locale's: a comma that is a decimal
// in Spanish is a thousands group in English, and anything ambiguous is refused, never read as another sum.
func TestFXPrefixAmountsAreReadInTheLocalesSyntax(t *testing.T) {
	for _, tc := range []struct {
		locale, text string
		want         string // a fragment of the card, or of the refusal
	}{
		{"es", "!cambio 1,5 USD", "1,5 USD ≈"},
		{"es", "!cambio 1.234,56 yen", "1.234,56 JPY ≈"},
		{"es", "!cambio 1.5 yen", "El importe debe ser"},
		{"en", "!fx 1,234.5 yen", "1,234.5 JPY ≈"},
		{"en", "!fx 1,5 yen", "The amount must be"},
		{"en", "!fx 5,00,0 yen", "The amount must be"},
		{"en", "!fx .5 yen", "The amount must be"}, // the lexer and the action agree: an amount, and a malformed one
		{"en", "!fx 50, USD", "The amount must be"},
		{"en", "!fx 0.5 yen", "0.5 JPY ≈"},
		{"ko", "!환율 50,000 엔화", "50,000엔 ≈"},
	} {
		t.Run(tc.locale+" "+tc.text, func(t *testing.T) {
			d := newFXDelivery(t, nil)
			cmd, o, ok := ParsePrefix("!", tc.text)
			if !ok {
				t.Fatal("did not parse")
			}
			if strings.Contains(strings.Join([]string{o["currency"]}, ""), "5") && o["amount"] == "" {
				t.Fatalf("a number was taken for a currency: %v", o)
			}
			r := d.g.svc.Execute(context.Background(), requestFor(cmd, o, InvokeContext{Locale: tc.locale, Typed: true}, "fx-prefix"))
			got := ""
			if r.OK {
				got = Render(r, cmd, tc.locale).Embed.Description
			} else {
				got = r.Error.Message
				if r.Error.MessageKey != "fx.invalid_amount" {
					t.Errorf("error %s, want fx.invalid_amount", r.Error.MessageKey)
				}
				if d.calls.Load() != 0 {
					t.Error("a provider was called for a refused amount")
				}
			}
			if !strings.Contains(got, tc.want) {
				t.Errorf("options %v: %q, want %q", o, got, tc.want)
			}
		})
	}
}

// The amount refusal says what an amount may be, in the locale's own way of writing one.
func TestFXInvalidAmountMessageStatesTheCap(t *testing.T) {
	d := newFXDelivery(t, nil)
	for locale, want := range map[string][]string{
		"en": {"1,000,000,000,000,000", "1,234.5"}, "es": {"1.000.000.000.000.000", "1.234,5"}, "ko": {"1,000,000,000,000,000", "1,234.5"},
		"ja": {"1,000,000,000,000,000", "1,234.5"}, "zh-Hans": {"1,000,000,000,000,000", "1,234.5"},
	} {
		res := d.g.svc.Execute(context.Background(), action.Request{Action: "finance.fx", Locale: locale, Input: mustJSON(map[string]any{"home": "USD", "amount": 2e15})})
		if res.OK || res.Error.MessageKey != "fx.invalid_amount" {
			t.Fatalf("%s: %+v", locale, res)
		}
		for _, w := range want {
			if !strings.Contains(res.Error.Message, w) {
				t.Errorf("%s: %q lacks %q", locale, res.Error.Message, w)
			}
		}
	}
}

// One language for the whole request: a saved language preference decides the home currency (its likely region), the way the
// quotes are written and the card's own words, whatever language Discord reported for the interaction.
func TestFXOneLocalePerRequest(t *testing.T) {
	setLocale := func(d *fxDelivery) {
		res := d.g.svc.Execute(context.Background(), action.Request{Action: "preferences.set", Locale: "ko", Input: mustJSON(map[string]any{"locale": "es"}),
			Invoker: &action.Invoker{Via: "command", GuildID: "g", ChannelID: "c", UserID: "u"}})
		if !res.OK {
			t.Fatalf("set locale: %v", res.Error)
		}
	}
	check := func(t *testing.T, d *fxDelivery, body string) {
		t.Helper()
		var payload struct {
			Embeds []struct{ Title, Description string }
		}
		if err := json.Unmarshal([]byte(body), &payload); err != nil || len(payload.Embeds) != 1 {
			t.Fatalf("malformed answer: %v %s", err, body)
		}
		got := payload.Embeds[0]
		// Spanish: the home currency of Spain first (euro), codes, decimal comma, Spanish title; nothing Korean
		if !strings.Contains(got.Title, "Tipos de cambio · JPY") || !strings.HasPrefix(got.Description, "1 JPY ≈ ") || !strings.Contains(strings.Split(got.Description, "\n")[0], " EUR") || !strings.Contains(got.Description, ",") {
			t.Errorf("card is not entirely Spanish: %+v", got)
		}
		if strings.Contains(got.Title, "환율") || strings.Contains(got.Description, "원") || strings.Contains(got.Description, "엔") {
			t.Errorf("a Korean piece in a Spanish card: %+v", got)
		}
		if d.paths[0] != "/latest/JPY" {
			t.Errorf("rates asked for %v, want the currency asked about", d.paths)
		}
	}
	t.Run("slash", func(t *testing.T) {
		d := newFXDelivery(t, nil)
		setLocale(d)
		d.slash(t, strOption("currency", "JPY")) // the interaction says Korean
		edit, ok := d.fake.find(http.MethodPatch, "/messages/@original")
		if !ok {
			t.Fatal("no answer")
		}
		check(t, d, edit.body)
	})
	t.Run("prefix", func(t *testing.T) {
		d := newFXDelivery(t, nil)
		setLocale(d)
		d.prefix(t, "!fx JPY")
		sent, ok := d.fake.find(http.MethodPost, "/messages")
		if !ok {
			t.Fatalf("no reply: %+v", d.fake.calls)
		}
		check(t, d, sent.body)
	})
	t.Run("an input error is in the same language", func(t *testing.T) {
		d := newFXDelivery(t, nil)
		setLocale(d)
		d.slash(t, strOption("currency", "U$D"))
		follow, ok := d.fake.find(http.MethodPost, "/webhooks/app/tok")
		if !ok || !strings.Contains(follow.body, "No pude saber") || strings.Contains(follow.body, "통화") {
			t.Fatalf("follow-up: %+v", follow)
		}
	})
}
