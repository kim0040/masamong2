package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
	"masamong/internal/application"
	"masamong/internal/chat"
)

// A picture drawn in the chat turn itself is held to Discord's upload limit like a queued one: over it,
// the file is not sent (Discord would refuse it) and the channel gets the image.failed line instead.
func TestChatPictureOverTheUploadLimitIsAFailureLineNotAnUpload(t *testing.T) {
	g, rec, id := pictureGateway(t, http.StatusOK)
	card := action.Card{Title: "Picture", Text: "a cat"}
	post := func(size int) {
		g.postToolArtifacts(context.Background(), g.sess, "g1", "c1", &discordgo.MessageReference{MessageID: "m1", ChannelID: "c1"},
			[]chat.ToolResult{{Tool: "images.generate", OK: true, Data: map[string]any{"card": card, "image_id": id, "bytes": size}}})
	}
	post(application.ImageUploadLimit) // exactly the limit still goes out
	if len(rec.bodies) != 1 || !strings.HasPrefix(rec.types[0], "multipart/form-data") || !strings.Contains(rec.bodies[0], "picture-bytes") {
		t.Fatalf("a picture within the limit was not uploaded: %d posts", len(rec.bodies))
	}
	post(application.ImageUploadLimit + 1)
	if len(rec.bodies) != 2 {
		t.Fatalf("%d posts, want the upload and one failure line", len(rec.bodies))
	}
	line := rec.bodies[1]
	if strings.HasPrefix(rec.types[1], "multipart/form-data") || strings.Contains(line, "picture-bytes") || strings.Contains(line, "attachment://") {
		t.Fatalf("an oversized picture was uploaded:\n%s", line)
	}
	want, _ := json.Marshal(g.svc.Msg(g.locale("g1"), "image.failed"))
	if !strings.Contains(line, string(want)) || !strings.Contains(line, `"message_id":"m1"`) || !strings.Contains(line, `"parse":[]`) {
		t.Fatalf("failure line: %s", line)
	}
}

func failureLine(t *testing.T, g *Gateway) string {
	t.Helper()
	want, _ := json.Marshal(g.svc.Msg(g.locale("g1"), "image.failed"))
	return string(want)
}

// A picture the chat turn drew but cannot be sent (the file is gone, or Discord refused the post) is
// the same image.failed line as a queued one: a reply to the request, nobody mentioned.
func TestChatPictureThatCannotBeSentIsAFailureLine(t *testing.T) {
	card := action.Card{Title: "Picture", Text: "a cat"}
	ref := &discordgo.MessageReference{MessageID: "m1", ChannelID: "c1"}
	check := func(step string, rec *bodyRecorder, wantPosts int) {
		t.Helper()
		if len(rec.bodies) != wantPosts {
			t.Fatalf("%s: %d posts, want %d", step, len(rec.bodies), wantPosts)
		}
		line := rec.bodies[wantPosts-1]
		if strings.HasPrefix(rec.types[wantPosts-1], "multipart/form-data") || !strings.Contains(line, `"message_id":"m1"`) || !strings.Contains(line, `"parse":[]`) {
			t.Fatalf("%s: last post is not the failure line reply:\n%s", step, line)
		}
	}

	g, rec, _ := pictureGateway(t, http.StatusOK)
	g.postToolArtifacts(context.Background(), g.sess, "g1", "c1", ref,
		[]chat.ToolResult{{Tool: "images.generate", OK: true, Data: map[string]any{"card": card, "image_id": "img_ffffffffffffffff", "bytes": 10}}})
	check("file missing", rec, 1)
	if !strings.Contains(rec.bodies[0], failureLine(t, g)) {
		t.Fatalf("file missing: not the image.failed text:\n%s", rec.bodies[0])
	}

	// the first post (the picture) is refused, the second (the line) goes through
	g, rec, id := pictureGateway(t, http.StatusBadRequest, http.StatusOK)
	g.postToolArtifacts(context.Background(), g.sess, "g1", "c1", ref,
		[]chat.ToolResult{{Tool: "images.generate", OK: true, Data: map[string]any{"card": card, "image_id": id, "bytes": 10}}})
	check("picture post refused", rec, 2)
	if !strings.HasPrefix(rec.types[0], "multipart/form-data") || !strings.Contains(rec.bodies[1], failureLine(t, g)) {
		t.Fatalf("picture post refused: want the upload attempt then the image.failed line:\n%v", rec.types)
	}
}

// The "!" command that drew a picture over the upload limit answers with the image.failed line (a reply,
// nobody mentioned) instead of a file Discord would refuse; within the limit the card goes out with it.
func TestPrefixImageCommandOverTheUploadLimitIsAFailureLine(t *testing.T) {
	g, rec, id := pictureGateway(t, http.StatusOK)
	cmd, ok := ByName("image")
	if !ok {
		t.Fatal("no image command")
	}
	m := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "m1", ChannelID: "c1", GuildID: "g1"}}
	ictx := InvokeContext{ChannelID: "c1", GuildID: "g1", Locale: g.locale("g1")}
	result := func(size int) action.Result {
		return action.Result{OK: true, Action: "images.generate", Output: map[string]any{"status": "ok", "image_id": id, "bytes": size}}
	}
	g.sendCommandCard(context.Background(), g.sess, m, cmd, ictx, result(application.ImageUploadLimit))
	if len(rec.bodies) != 1 || !strings.HasPrefix(rec.types[0], "multipart/form-data") || !strings.Contains(rec.bodies[0], "picture-bytes") {
		t.Fatalf("a picture within the limit was not uploaded: %d posts", len(rec.bodies))
	}
	g.sendCommandCard(context.Background(), g.sess, m, cmd, ictx, result(application.ImageUploadLimit+1))
	if len(rec.bodies) != 2 {
		t.Fatalf("%d posts, want the upload and one failure line", len(rec.bodies))
	}
	line := rec.bodies[1]
	if strings.HasPrefix(rec.types[1], "multipart/form-data") || strings.Contains(line, "picture-bytes") || strings.Contains(line, "attachment://") ||
		!strings.Contains(line, failureLine(t, g)) || !strings.Contains(line, `"message_id":"m1"`) || !strings.Contains(line, `"parse":[]`) {
		t.Fatalf("failure line: %s", line)
	}
}

// The /image slash command edits its response with the image.failed text, no file and no card, for a
// picture over the upload limit.
func TestSlashImageResponseOverTheUploadLimitHasNoFile(t *testing.T) {
	g, _, id := pictureGateway(t, http.StatusOK)
	cmd, _ := ByName("image")
	ictx := InvokeContext{ChannelID: "c1", GuildID: "g1", Locale: g.locale("g1")}
	result := func(size int) action.Result {
		return action.Result{OK: true, Action: "images.generate", Output: map[string]any{"status": "ok", "image_id": id, "bytes": size}}
	}
	if edit := g.responseEdit(result(application.ImageUploadLimit), cmd, ictx); len(edit.Files) != 1 || edit.Content != nil {
		t.Fatalf("a picture within the limit is attached: %+v", edit)
	}
	edit := g.responseEdit(result(application.ImageUploadLimit+1), cmd, ictx)
	if len(edit.Files) != 0 || edit.Content == nil || *edit.Content != g.svc.Msg(g.locale("g1"), "image.failed") || edit.Embeds == nil || len(*edit.Embeds) != 0 {
		t.Fatalf("an oversized picture must answer with the failure text only: %+v", edit)
	}
}
