package application_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"masamong/internal/application"
)

// The operator pastes only the bot token: discord.check reads the bot id from Discord (Get Current
// Application) and stores it, so the invite link works without copying ids from the Developer Portal,
// and it says when config wants message content the portal does not grant.
func TestDiscordCheckFillsTheBotIDFromTheToken(t *testing.T) {
	var auth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		if r.URL.Path != "/applications/@me" || auth != "Bot tok-1" {
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"message": "401: Unauthorized", "code": 0}`))
			return
		}
		// the documented example object, trimmed; flags 1<<19 = GATEWAY_MESSAGE_CONTENT_LIMITED
		_, _ = w.Write([]byte(`{"id":"172150183260323840","name":"Baba O-Riley","flags":524288,"bot_public":true,"bot":{"id":"172150183260323840","username":"masamong"}}`))
	}))
	defer srv.Close()
	svc := setupSvc(t)
	application.SetDiscordAPIForTest(svc, srv.URL)
	if r := exec(svc, "discord.check", nil); r.OK || r.Error.Args["reason"] != "no_token" {
		t.Fatalf("no token yet: %+v", r)
	}
	t.Setenv("MASAMONG_DISCORD_TOKEN", "tok-1")
	res := out(t, svc, "discord.check", map[string]any{"apply": true})
	if res["bot_id"] != "172150183260323840" || res["bot_name"] != "masamong" || res["message_content_portal"] != true || res["stored"] != true {
		t.Fatalf("check: %v", res)
	}
	inst := out(t, svc, "config.document", nil)["document"].(map[string]any)["instance"].(map[string]any)
	if inst["expected_bot_id"] != "172150183260323840" {
		t.Fatalf("bot id stored: %v", inst)
	}
	if inv := out(t, svc, "channels.invite", nil); inv["url"] == nil {
		t.Fatalf("invite from the stored id: %v", inv)
	}
	t.Setenv("MASAMONG_DISCORD_TOKEN", "wrong")
	if r := exec(svc, "discord.check", nil); r.OK || r.Error.Args["reason"] != "token_rejected" {
		t.Fatalf("a rejected token: %+v", r)
	}
}

// The console asks Discord about the token only when one can be read, so channels.list says so.
func TestChannelsListSaysWhetherADiscordTokenIsSaved(t *testing.T) {
	svc := setupSvc(t)
	t.Setenv("MASAMONG_DISCORD_TOKEN", "")
	if got := out(t, svc, "channels.list", nil)["discord_token"]; got != false {
		t.Fatalf("no token: discord_token = %v", got)
	}
	t.Setenv("MASAMONG_DISCORD_TOKEN", "tok-1")
	if got := out(t, svc, "channels.list", nil)["discord_token"]; got != true {
		t.Fatalf("token set: discord_token = %v", got)
	}
}
