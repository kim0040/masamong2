package discord

import (
	"net/http"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/config"
)

// Turning memory off is explicit and wins: a channel set capture: false keeps every thread under it from
// storing messages. That holds for a thread the application knows, one whose kind Discord could not tell
// (the lookup failed), a private thread (never registered with its parent) and a thread whose own entry says
// capture: true. An ordinary channel with no entry still stores by default.
func TestCaptureOffParentBlocksAllThreadCapture(t *testing.T) {
	g, fake := privateCardGateway(t)
	raw, err := config.Marshal(g.svc.Doc())
	if err != nil {
		t.Fatal(err)
	}
	doc, err := config.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	doc.Memory.Capture = true
	doc.Guilds = map[string]config.GuildSettings{"g1": {Channels: map[string]config.ChannelPolicy{
		"parent": {Respond: "command", Capture: false},
		"kid":    {Respond: "command", Capture: true}, // a thread of parent with an entry of its own
	}}}
	applyDoc(t, g, doc)
	g.svc.NoteThread("g1", "pub", "parent", false)
	g.svc.NoteThread("g1", "kid", "parent", false)
	fake.setResponse("/channels/pub", http.StatusOK, `{"id":"pub","type":11,"guild_id":"g1","parent_id":"parent"}`)
	fake.setResponse("/channels/kid", http.StatusOK, `{"id":"kid","type":11,"guild_id":"g1","parent_id":"parent"}`)
	fake.setResponse("/channels/priv", http.StatusOK, `{"id":"priv","type":12,"guild_id":"g1","parent_id":"parent"}`)
	fake.setResponse("/channels/cold", http.StatusNotFound, `{"code":10003,"message":"Unknown Channel"}`)
	for _, ch := range []string{"pub", "kid", "priv", "cold", "open"} {
		m := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "m-" + ch, ChannelID: ch, GuildID: "g1",
			Content: "a synthetic line in " + ch, Author: &discordgo.User{ID: "u1", Username: "member"}, Timestamp: time.Now()}}
		g.onServerMessage(g.sess, m, Command{}, nil, false)
	}
	stored := func(id string) bool {
		t.Helper()
		var n int
		if err := g.svc.Store.Reader().QueryRow(`SELECT COUNT(*) FROM memory_observations WHERE native_message_id=?`, id).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n > 0
	}
	if !stored("m-open") {
		t.Fatal("an ordinary channel with no entry stores by default; the checks below prove nothing")
	}
	for _, ch := range []string{"pub", "kid", "priv", "cold"} {
		if stored("m-" + ch) {
			t.Errorf("thread %s of a channel with capture off stored its message", ch)
		}
	}
}
