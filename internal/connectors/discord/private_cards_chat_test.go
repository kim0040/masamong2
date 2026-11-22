package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"masamong/internal/action"
	"masamong/internal/config"
)

// A personal "!" command that fails in a room where the bot chats does not carry on as a conversation: the
// model's reply would be posted in the room, about the person's own data. The failure goes to the person's
// DMs as the card, and the room gets the usual one line.
func TestPrivatePrefixFailureNeverBecomesARoomReply(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"role": "assistant", "content": "a synthetic public reply"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 10, "completion_tokens": 5},
		})
	}))
	t.Cleanup(srv.Close)
	g, fake := privateCardGateway(t)
	if res := g.svc.Execute(context.Background(), action.Request{Action: "access.policy", Input: mustJSON(map[string]any{"server_approval": false})}); !res.OK {
		t.Fatal("could not disable server approval for the fixture")
	}
	t.Setenv("FAKE_PRIVATE_CARD_MODEL_KEY", "test")
	raw, err := config.Marshal(g.svc.Doc())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	doc.Providers["private-card-test"] = config.Provider{Kind: config.KindOpenAI, BaseURL: srv.URL, CredentialRef: "env:FAKE_PRIVATE_CARD_MODEL_KEY"}
	doc.Models["planner"] = config.Model{Provider: "private-card-test", Model: "shared/model"}
	doc.Models["response"] = config.Model{Provider: "private-card-test", Model: "shared/model"}
	doc.Memory.Retrieve = "off"
	doc.Guilds = map[string]config.GuildSettings{"g1": {Channels: map[string]config.ChannelPolicy{"room": {Respond: "mention"}}}}
	applyDoc(t, g, doc)
	if res := g.svc.Execute(context.Background(), action.Request{Action: "tools.configure", Input: mustJSON(map[string]any{"id": "reminders.update", "enabled": "off"})}); !res.OK {
		t.Fatalf("could not switch the reminders tool off: %+v", res.Error)
	}
	cmd, _ := ByName("reminders")
	g.runCommand(g.sess, roomMessage("m1", "!"+cmd.Aliases[0]), cmd, nil)
	g.inflight.Wait()
	if n := calls.Load(); n != 0 {
		t.Fatalf("the failed personal command asked the model %d times", n)
	}
	loc := g.svc.LocaleFor("u1", g.locale("g1"))
	if room := postsTo(t, fake, "room"); len(room) != 1 || len(room[0].Embeds) != 0 || room[0].Content != messages.T(loc, "discord.private.dm_sent") {
		t.Fatalf("room posts = %+v, want only the DM line", room)
	}
	if dm := postsTo(t, fake, "1"); len(dm) != 1 || len(dm[0].Embeds) == 0 {
		t.Fatalf("DM posts = %+v, want the error card", dm)
	}
}
