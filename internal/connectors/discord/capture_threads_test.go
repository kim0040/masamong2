package discord

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
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

// A private thread is never registered with the application, so its parent used to live only in the
// gateway's cache. Once its parent turns capture off, a catch-up the console names for the thread must be
// refused like any thread under that parent, with no history read: the parent was stored for good when the
// gateway first saw the thread.
func TestPrivateThreadParentOutlivesTheGatewayCache(t *testing.T) {
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
	doc.Memory.BackfillDays = 7
	doc.Discord.MessageContent = true
	doc.Guilds = map[string]config.GuildSettings{"g1": {Channels: map[string]config.ChannelPolicy{
		"parent": {Respond: "command", Capture: true},
	}}}
	applyDoc(t, g, doc)
	fake.setResponse("/channels/priv", http.StatusOK, `{"id":"priv","type":12,"guild_id":"g1","parent_id":"parent"}`)
	m := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "m-priv", ChannelID: "priv", GuildID: "g1",
		Content: "a synthetic line in a private thread", Author: &discordgo.User{ID: "u1", Username: "member"}, Timestamp: time.Now()}}
	g.onServerMessage(g.sess, m, Command{}, nil, false)
	var n int
	if err := g.svc.Store.Reader().QueryRow(`SELECT COUNT(*) FROM memory_observations WHERE native_message_id='m-priv'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n == 0 {
		t.Fatal("precondition: the private thread of a storing parent stores its message")
	}
	g.channelKinds.Delete("priv") // a restart, or a day without messages: the gateway forgot the thread
	doc.Guilds["g1"].Channels["parent"] = config.ChannelPolicy{Respond: "command", Capture: false}
	applyDoc(t, g, doc)
	g.svc.SetDiscord(g)
	g.connected.Store(true)
	res := g.svc.Execute(context.Background(), action.Request{Action: "memory.backfill", Input: mustJSON(map[string]any{"guild_id": "g1", "channel_id": "priv"})})
	if res.OK {
		t.Errorf("a catch-up of a private thread under a channel with capture off was not refused: %+v", res.Output)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, c := range fake.calls {
		if c.method == http.MethodGet && strings.HasSuffix(c.path, "/channels/priv/messages") {
			t.Fatalf("history of the private thread was read after its parent turned capture off: %s %s", c.method, c.path)
		}
	}
}
