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

// When Discord refuses to delete the public deferred response, the input error still goes out privately
// and is never patched into the public message.
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
			follow, ok := d.fake.find(http.MethodPost, "/webhooks/app/tok")
			if !ok {
				t.Fatalf("no private follow-up: %+v", d.fake.calls)
			}
			var body struct {
				Flags  int
				Embeds []struct{ Description string }
			}
			if err := json.Unmarshal([]byte(follow.body), &body); err != nil || body.Flags != ephemeral || len(body.Embeds) != 1 {
				t.Fatalf("follow-up %s: %v", follow.body, err)
			}
		})
	}
}
