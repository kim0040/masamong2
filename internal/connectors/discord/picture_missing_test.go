package discord

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
)

// A generated picture whose file is missing is the same failure as an oversized one on the "!" command:
// the image.failed line as a reply, not a card that claims success without a picture.
func TestPrefixImageCommandWithMissingFileIsAFailureLine(t *testing.T) {
	g, rec, _ := pictureGateway(t, http.StatusOK)
	cmd, _ := ByName("image")
	m := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "m1", ChannelID: "c1", GuildID: "g1"}}
	ictx := InvokeContext{ChannelID: "c1", GuildID: "g1", Locale: g.locale("g1")}
	res := action.Result{OK: true, Action: "images.generate", Output: map[string]any{"status": "ok", "image_id": "img_ffffffffffffffff", "bytes": 10}}
	g.sendCommandCard(context.Background(), g.sess, m, cmd, ictx, res)
	if len(rec.bodies) != 1 {
		t.Fatalf("%d posts, want one failure line", len(rec.bodies))
	}
	line := rec.bodies[0]
	if strings.HasPrefix(rec.types[0], "multipart/form-data") || strings.Contains(line, `"embeds":[`) || strings.Contains(line, "attachment://") ||
		!strings.Contains(line, failureLine(t, g)) || !strings.Contains(line, `"message_id":"m1"`) || !strings.Contains(line, `"parse":[]`) {
		t.Fatalf("a missing picture file must post the failure line only: %s", line)
	}
}

// The /image slash command edits its response with the image.failed text when the picture's file is
// missing: no card, no file.
func TestSlashImageResponseWithMissingFileIsTheFailureText(t *testing.T) {
	g, _, _ := pictureGateway(t, http.StatusOK)
	cmd, _ := ByName("image")
	ictx := InvokeContext{ChannelID: "c1", GuildID: "g1", Locale: g.locale("g1")}
	res := action.Result{OK: true, Action: "images.generate", Output: map[string]any{"status": "ok", "image_id": "img_ffffffffffffffff", "bytes": 10}}
	edit := g.responseEdit(res, cmd, ictx)
	if len(edit.Files) != 0 || edit.Content == nil || *edit.Content != g.svc.Msg(g.locale("g1"), "image.failed") || edit.Embeds == nil || len(*edit.Embeds) != 0 {
		t.Fatalf("a missing picture file must answer with the failure text only: %+v", edit)
	}
}
