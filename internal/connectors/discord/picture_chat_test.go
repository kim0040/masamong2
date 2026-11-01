package discord

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
	"masamong/internal/chat"
)

func chatPictureResults(id string) []chat.ToolResult {
	return []chat.ToolResult{{Tool: "images.generate", OK: true, Data: map[string]any{"card": action.Card{Title: "Picture", Text: "a cat"}, "image_id": id, "bytes": 10}}}
}

// The picture is posted like a queued one (strict reply); when the bot lacks the permission to post, no
// failure line follows (it would not get through either) and the caller's reference is left alone.
func TestChatPictureWithoutPermissionIsNoFailureLine(t *testing.T) {
	fastReplyBackoff(t)
	g, _, id := pictureGateway(t, http.StatusForbidden)
	rec := &bodyRecorder{script: []int{http.StatusForbidden}}
	g.sess.Client = &http.Client{Transport: forbidden{rec}}
	ref := &discordgo.MessageReference{MessageID: "m1", ChannelID: "c1"}
	g.postToolArtifacts(context.Background(), g.sess, "g1", "c1", ref, chatPictureResults(id))
	if len(rec.bodies) != 1 || !strings.HasPrefix(rec.types[0], "multipart/form-data") {
		t.Fatalf("%d posts, want only the refused picture and no failure line", len(rec.bodies))
	}
	if !strings.Contains(rec.bodies[0], `"fail_if_not_exists":true`) {
		t.Fatalf("the picture must be a strict reply:\n%s", rec.bodies[0])
	}
	if ref.FailIfNotExists != nil {
		t.Fatal("the caller's reference was modified")
	}
}

// The message the picture answers is gone: nothing is posted, not the picture and not a line.
func TestChatPictureWhoseRequestIsGoneIsNotPosted(t *testing.T) {
	fastReplyBackoff(t)
	g, _, id := pictureGateway(t, http.StatusBadRequest)
	rec := &bodyRecorder{script: []int{http.StatusBadRequest, http.StatusOK}}
	g.sess.Client = &http.Client{Transport: gone{rec}}
	g.postToolArtifacts(context.Background(), g.sess, "g1", "c1", &discordgo.MessageReference{MessageID: "m-deleted", ChannelID: "c1"}, chatPictureResults(id))
	if len(rec.bodies) != 1 {
		t.Fatalf("%d posts, want only the one refused reply", len(rec.bodies))
	}
}

type cancelling struct {
	cancel context.CancelFunc
	rec    *bodyRecorder
}

func (c cancelling) RoundTrip(req *http.Request) (*http.Response, error) {
	resp, err := c.rec.RoundTrip(req)
	c.cancel() // the turn ends while the post is in flight
	return resp, err
}

// A turn cancelled while the picture is being posted (shutdown) leaves no failure line behind.
func TestChatPictureCancelledMidPostIsNoFailureLine(t *testing.T) {
	fastReplyBackoff(t)
	g, _, id := pictureGateway(t, http.StatusInternalServerError)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	rec := &bodyRecorder{script: []int{http.StatusInternalServerError}}
	g.sess.Client = &http.Client{Transport: cancelling{cancel, rec}}
	g.postToolArtifacts(ctx, g.sess, "g1", "c1", &discordgo.MessageReference{MessageID: "m1", ChannelID: "c1"}, chatPictureResults(id))
	if len(rec.bodies) != 1 {
		t.Fatalf("%d posts, want only the picture attempt", len(rec.bodies))
	}
}

type silent struct{ *bodyRecorder }

func (s silent) RoundTrip(req *http.Request) (*http.Response, error) {
	_, _ = s.bodyRecorder.RoundTrip(req)
	return nil, io.ErrUnexpectedEOF // the answer was lost: the picture may be posted
}

// No answer from Discord at all: the picture may be there, so no line (it would contradict it).
func TestChatPictureWithLostAnswerIsNoFailureLine(t *testing.T) {
	fastReplyBackoff(t)
	g, _, id := pictureGateway(t, http.StatusOK)
	rec := &bodyRecorder{script: []int{http.StatusOK}}
	g.sess.Client = &http.Client{Transport: silent{rec}}
	g.postToolArtifacts(context.Background(), g.sess, "g1", "c1", &discordgo.MessageReference{MessageID: "m1", ChannelID: "c1"}, chatPictureResults(id))
	if len(rec.types) == 0 {
		t.Fatal("the picture was not attempted")
	}
	for i, ty := range rec.types {
		if !strings.HasPrefix(ty, "multipart/form-data") {
			t.Fatalf("post %d is a failure line after an unknown outcome", i)
		}
	}
}
