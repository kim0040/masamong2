package discord

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/vision"
)

// Pictures are collected only for a turn said to the bot: a room that answers everything does not read
// the pictures of messages nobody addressed to it. The picture's author travels with it (the consent of
// someone else's picture is checked by the turn).
func TestPicturesAreCollectedOnlyForTurnsSaidToTheBot(t *testing.T) {
	svc := svcForTest(t)
	g := &Gateway{svc: svc, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	bot := "bot"
	g.botID.Store(&bot)
	svc.ThreadOpened(context.Background(), "g1", "c1", "th1", "owner", "")
	msg := func(id, channel, content string, mod func(m *discordgo.Message)) []*discordgo.MessageCreate {
		m := &discordgo.Message{ID: id, GuildID: "g1", ChannelID: channel, Content: content, Author: &discordgo.User{ID: "owner"},
			Attachments: []*discordgo.MessageAttachment{att("a-"+id, "image/png", "x.png")}}
		if mod != nil {
			mod(m)
		}
		return []*discordgo.MessageCreate{{Message: m}}
	}
	g.followIDs.Store("follow", true)
	for _, c := range []struct {
		name string
		msgs []*discordgo.MessageCreate
		want bool
	}{
		{"talk nobody addressed to the bot", msg("plain", "c1", "이거 봐라 ㅋㅋ", nil), false},
		{"a reply to someone else", msg("other", "c1", "이거?", func(m *discordgo.Message) {
			m.ReferencedMessage = &discordgo.Message{ID: "r", Author: &discordgo.User{ID: "friend"}}
		}), false},
		{"a mention", msg("mention", "c1", "<@bot> 이거 뭐야", func(m *discordgo.Message) { m.Mentions = []*discordgo.User{{ID: bot}} }), true},
		{"the bot's name", msg("name", "c1", "masamong 이거 뭐야", nil), true},
		{"a reply to the bot", msg("reply", "c1", "이건?", func(m *discordgo.Message) {
			m.ReferencedMessage = &discordgo.Message{ID: "b", Author: &discordgo.User{ID: bot}}
		}), true},
		{"a follow-up the window chose", msg("follow", "c1", "그럼 이건?", nil), true},
		{"a line in the bot's own thread", msg("thread", "th1", "이것도 봐줘", nil), true},
	} {
		call := g.heldCall(c.msgs)
		if got := len(call.images) > 0; got != c.want {
			t.Errorf("%s: pictures collected %v, want %v", c.name, got, c.want)
		}
		if c.want && call.images[0].AuthorID != "owner" {
			t.Errorf("%s: picture author %q", c.name, call.images[0].AuthorID)
		}
	}
}

func TestPictureAuthorIsTheOneWhoPostedIt(t *testing.T) {
	replied := &discordgo.Message{ID: "r1", Author: &discordgo.User{ID: "poster"}, Attachments: []*discordgo.MessageAttachment{att("a1", "image/png", "a.png")}}
	fwd := &discordgo.Message{ID: "m2", Author: &discordgo.User{ID: "caller"}, MessageReference: &discordgo.MessageReference{Type: discordgo.MessageReferenceTypeForward},
		MessageSnapshots: []discordgo.MessageSnapshot{{Message: &discordgo.Message{Attachments: []*discordgo.MessageAttachment{att("a2", "image/png", "b.png")}}}}}
	m := &discordgo.Message{ID: "m1", Author: &discordgo.User{ID: "caller"}, ReferencedMessage: replied, MessageReference: &discordgo.MessageReference{MessageID: "r1"}}
	refs := imageRefs([]*discordgo.Message{m, fwd})
	if len(refs) != 2 || refs[0].AuthorID != "caller" || refs[0].MessageID != "m2" {
		t.Fatalf("request-forwarded picture should come first: %+v", refs)
	}
	// Discord does not say who wrote a forwarded message: the forwarding person stands for it
	if refs[1].AuthorID != "poster" || refs[1].MessageID != "r1" {
		t.Fatalf("replied-to picture should come after the request: %+v", refs[1])
	}
}

func TestHeldCallCarriesImageOmissionCounts(t *testing.T) {
	svc := svcForTest(t)
	g := &Gateway{svc: svc}
	bot := "bot"
	g.botID.Store(&bot)
	m := &discordgo.Message{ID: "request", GuildID: "g", ChannelID: "c", Content: "<@bot> describe these", Author: &discordgo.User{ID: "caller"},
		Mentions: []*discordgo.User{{ID: bot}}, Attachments: []*discordgo.MessageAttachment{
			att("1", "image/png", "1.png"), att("2", "image/png", "2.png"), att("3", "image/png", "3.png"), att("4", "image/png", "4.png"),
		}}
	call := g.heldCall([]*discordgo.MessageCreate{{Message: m}})
	if len(call.images) != 3 || call.imageTotal != 4 || call.imageSkipped[vision.ReasonOverLimit] != 1 {
		t.Fatalf("held image selection: selected=%d total=%d skipped=%v", len(call.images), call.imageTotal, call.imageSkipped)
	}
}
