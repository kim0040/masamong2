package discord

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
	"masamong/internal/config"
)

// What Discord gets when a picture is drawn, on the paths that post it synchronously: the /image and !image
// commands and a chat turn that drew it itself. Each sends the file alone, with no card around it.

// wirePicture is what the local image model draws.
const wirePicture = "\x89PNG wire-picture"

// imageCommandGateway is a gateway over a fake Discord and a service that draws with a local image model,
// so a command runs the whole way, from the interaction or message to the requests Discord gets.
func imageCommandGateway(t *testing.T) (*Gateway, *fakeDiscord) {
	t.Helper()
	png := "data:image/png;base64," + base64.StdEncoding.EncodeToString([]byte(wirePicture))
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"choices": []any{map[string]any{"message": map[string]any{"content": "",
			"images": []any{map[string]any{"type": "image_url", "image_url": map[string]any{"url": png}}}}}}, "usage": map[string]any{"cost": 0.03}})
	}))
	t.Cleanup(srv.Close)
	g, fake := gatewayWithFakeDiscord(t)
	g.ActionTimeout = 10 * time.Second
	g.cfg.RegisterCommands, g.cfg.MessageContent, g.cfg.CommandPrefix = true, true, "!"
	if res := g.svc.Execute(context.Background(), action.Request{Action: "access.policy", Input: mustJSON(map[string]any{"server_approval": false})}); !res.OK {
		t.Fatalf("access policy: %+v", res)
	}
	t.Setenv("FAKE_WIRE_IMAGE_KEY", "k")
	raw, err := config.Marshal(g.svc.Doc())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	doc.Providers["wire-img"] = config.Provider{Kind: config.KindOpenAI, BaseURL: srv.URL, CredentialRef: "env:FAKE_WIRE_IMAGE_KEY"}
	doc.Models["image"] = config.Model{Provider: "wire-img", Model: "google/gemini-image"}
	applyDoc(t, g, doc)
	return g, fake
}

// pictureAlone fails the test unless body is a multipart upload of picture whose JSON part has no card:
// no embed, no attachment:// reference to the file, and none of the card's words.
func pictureAlone(t *testing.T, step, body, picture string, cardWords ...string) {
	t.Helper()
	if !strings.Contains(body, `name="payload_json"`) || !strings.Contains(body, picture) {
		t.Fatalf("%s: not a multipart upload of the picture:\n%s", step, body)
	}
	if strings.Contains(body, `"embeds":[{`) || strings.Contains(body, "attachment://") {
		t.Fatalf("%s: the picture goes with a card; it goes alone:\n%s", step, body)
	}
	for _, w := range cardWords {
		if strings.Contains(body, w) {
			t.Fatalf("%s: the card's %q is in the post:\n%s", step, w, body)
		}
	}
}

// /image acknowledges at once (a deferred response) and then edits that response into the picture alone:
// a multipart PATCH of @original with the file and an empty embed list.
func TestSlashImageEditsTheDeferredResponseIntoThePictureAlone(t *testing.T) {
	g, fake := imageCommandGateway(t)
	fake.setResponse("/messages/@original", http.StatusOK,
		`{"id":"e1","channel_id":"c","attachments":[{"id":"a1","filename":"picture.png","url":"https://cdn.example/picture.png"}]}`)
	i := &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "i", AppID: "app", Token: "tok", Type: discordgo.InteractionApplicationCommand,
		ChannelID: "c", GuildID: "g", Member: &discordgo.Member{User: &discordgo.User{ID: "u", Username: "tester"}},
		Data: discordgo.ApplicationCommandInteractionData{Name: "image", Options: []*discordgo.ApplicationCommandInteractionDataOption{strOption("prompt", "a striped cat")}}}}
	g.handleCommand(g.sess, i)
	if !g.Wait(15 * time.Second) {
		t.Fatal("command work did not finish")
	}
	cb, ok := fake.find(http.MethodPost, "/interactions/i/tok/callback")
	if !ok {
		t.Fatalf("no acknowledgement: %+v", fake.calls)
	}
	var ack struct{ Type int }
	if err := json.Unmarshal([]byte(cb.body), &ack); err != nil || ack.Type != int(discordgo.InteractionResponseDeferredChannelMessageWithSource) {
		t.Fatalf("the acknowledgement is not a deferred response: %s (%v)", cb.body, err)
	}
	edit, ok := fake.find(http.MethodPatch, "/webhooks/app/tok/messages/@original")
	if !ok {
		t.Fatalf("the deferred response was never edited: %+v", fake.calls)
	}
	pictureAlone(t, "/image", edit.body, wirePicture, "a striped cat")
	if !strings.Contains(edit.body, `"embeds":[]`) {
		t.Fatalf("/image: the edit must clear the response's embeds:\n%s", edit.body)
	}
	if _, ok := fake.find(http.MethodPost, "/channels/c/messages"); ok {
		t.Fatal("/image posted a message of its own besides the edit")
	}
}

// !image replies to the command message with the picture alone, nobody mentioned.
func TestPrefixImageCommandRepliesWithThePictureAlone(t *testing.T) {
	g, fake := imageCommandGateway(t)
	m := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "m", ChannelID: "c", GuildID: "g", Content: "!image a striped cat",
		Author: &discordgo.User{ID: "u", Username: "tester"}}}
	cmd, opts, ok := ParsePrefix(shippedCatalog(), "!", m.Content)
	if !ok || cmd.Name != "image" {
		t.Fatalf("%q did not parse as the image command", m.Content)
	}
	g.runCommand(g.sess, m, cmd, opts)
	if !g.Wait(15 * time.Second) {
		t.Fatal("command work did not finish")
	}
	post, ok := fake.find(http.MethodPost, "/channels/c/messages")
	if !ok {
		t.Fatalf("!image posted nothing: %+v", fake.calls)
	}
	pictureAlone(t, "!image", post.body, wirePicture, "a striped cat")
	for _, want := range []string{`"message_reference"`, `"message_id":"m"`, `"parse":[]`} {
		if !strings.Contains(post.body, want) {
			t.Fatalf("!image: the post lacks %s:\n%s", want, post.body)
		}
	}
}

// A chat turn that drew its picture itself (no queue) posts it as a strict reply, alone.
func TestSynchronousChatPictureIsPostedAlone(t *testing.T) {
	g, rec, id := pictureGateway(t, http.StatusOK)
	g.postToolArtifacts(context.Background(), g.sess, "g1", "c1", &discordgo.MessageReference{MessageID: "m1", ChannelID: "c1"}, chatPictureResults(id))
	if len(rec.bodies) != 1 || !strings.HasPrefix(rec.types[0], "multipart/form-data") {
		t.Fatalf("%d posts, want the picture upload", len(rec.bodies))
	}
	pictureAlone(t, "chat", rec.bodies[0], "picture-bytes", `"Picture"`, "a cat")
	for _, want := range []string{`"message_id":"m1"`, `"fail_if_not_exists":true`, `"parse":[]`} {
		if !strings.Contains(rec.bodies[0], want) {
			t.Fatalf("chat: the post lacks %s:\n%s", want, rec.bodies[0])
		}
	}
}
