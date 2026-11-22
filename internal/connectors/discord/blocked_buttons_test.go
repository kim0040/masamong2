package discord

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
)

// A person blocked in a server cannot use the reply's thread buttons or its "more" button there: the press
// gets the refusal only they see, and nothing else happens.
func TestBlockedPersonsThreadAndMoreButtonsAreRefused(t *testing.T) {
	g, fake := privateCardGateway(t)
	const guild, blocked = "223456789012345678", "123456789012345678"
	if res := g.svc.Execute(context.Background(), action.Request{Action: "access.policy", Input: mustJSON(map[string]any{"guild_id": guild, "block": blocked})}); !res.OK {
		t.Fatalf("block: %+v", res.Error)
	}
	if !g.svc.BlockedIn(guild, blocked) {
		t.Fatal("fixture: the person is not blocked")
	}
	press := func(customID string) *discordgo.InteractionCreate {
		return &discordgo.InteractionCreate{Interaction: &discordgo.Interaction{ID: "i-" + customID, AppID: "app", Token: "tok",
			Type: discordgo.InteractionMessageComponent, ChannelID: "room", GuildID: guild,
			Member:  &discordgo.Member{User: &discordgo.User{ID: blocked}},
			Message: &discordgo.Message{ID: "reply", ChannelID: "room", Content: "an answer"},
			Data:    discordgo.MessageComponentInteractionData{CustomID: customID, ComponentType: discordgo.ButtonComponent}}}
	}
	want := messages.T(g.svc.LocaleFor(blocked, g.locale(guild)), "access.blocked")
	for _, id := range []string{"thr:open:turn1:" + blocked, "thr:stay:turn1:" + blocked, "thr:stop:thread1", "more:reply"} {
		before := len(fake.calls)
		g.handleComponent(g.sess, press(id))
		g.inflight.Wait()
		fake.mu.Lock()
		calls := append([]recordedCall(nil), fake.calls[before:]...)
		fake.mu.Unlock()
		if len(calls) != 1 || !strings.HasSuffix(calls[0].path, "/callback") {
			t.Fatalf("%s: calls %+v, want one refusal", id, calls)
		}
		var resp discordgo.InteractionResponse
		if err := json.Unmarshal([]byte(calls[0].body), &resp); err != nil || resp.Data == nil {
			t.Fatalf("%s: response %q", id, calls[0].body)
		}
		if resp.Type != discordgo.InteractionResponseChannelMessageWithSource || resp.Data.Content != want || resp.Data.Flags&discordgo.MessageFlagsEphemeral == 0 {
			t.Fatalf("%s: response %+v %+v, want the private refusal %q", id, resp, resp.Data, want)
		}
	}
	// a person not blocked gets the button's own answer (here: the "more" text has expired)
	other := press("more:reply")
	other.ID = "i-other"
	other.Member = &discordgo.Member{User: &discordgo.User{ID: "323456789012345678"}}
	g.handleComponent(g.sess, other)
	if c, ok := fake.find(http.MethodPost, "/interactions/i-other/tok/callback"); !ok || strings.Contains(c.body, want) {
		t.Fatalf("a member who is not blocked was refused: %+v", fake.calls)
	}
}
