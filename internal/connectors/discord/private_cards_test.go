package discord

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/action"
)

func privateCardGateway(t *testing.T) (*Gateway, *fakeDiscord) {
	t.Helper()
	g, fake := gatewayWithFakeDiscord(t)
	g.ActionTimeout = 10 * time.Second
	g.cfg.CommandPrefix = "!"
	g.cfg.MessageContent = true
	return g, fake
}

func roomMessage(id, text string) *discordgo.MessageCreate {
	return &discordgo.MessageCreate{Message: &discordgo.Message{ID: id, ChannelID: "room", GuildID: "g1", Content: text,
		Author: &discordgo.User{ID: "u1", Username: "member"}, Timestamp: time.Now()}}
}

type sentMessage struct {
	Content string                    `json:"content"`
	Embeds  []*discordgo.MessageEmbed `json:"embeds"`
}

func postsTo(t *testing.T, fake *fakeDiscord, channelID string) []sentMessage {
	t.Helper()
	fake.mu.Lock()
	defer fake.mu.Unlock()
	var out []sentMessage
	for _, c := range fake.calls {
		if c.method != http.MethodPost || !strings.HasSuffix(c.path, "/channels/"+channelID+"/messages") {
			continue
		}
		var m sentMessage
		if err := json.Unmarshal([]byte(c.body), &m); err != nil {
			// multipart (a file): the JSON is in the payload_json part
			if i := strings.Index(c.body, "{"); i >= 0 {
				_ = json.NewDecoder(strings.NewReader(c.body[i:])).Decode(&m)
			}
		}
		out = append(out, m)
	}
	return out
}

// Every personal command typed with "!" in a server room sends its card to the caller's DMs: the room gets
// one line and no card.
func TestPrivatePrefixCardsGoToDMNotTheRoom(t *testing.T) {
	var private []Command
	for _, c := range Commands() {
		if c.Private {
			private = append(private, c)
		}
	}
	want := map[string]bool{"mymemory": true, "forgetme": true, "reminders": true, "privacy": true, "mysettings": true,
		"fortune-register": true, "notices-register": true}
	if len(private) != len(want) {
		t.Fatalf("private commands = %d, want %d", len(private), len(want))
	}
	for _, cmd := range private {
		if !want[cmd.Name] {
			t.Fatalf("unexpected private command %s", cmd.Name)
		}
		t.Run(cmd.Name, func(t *testing.T) {
			g, fake := privateCardGateway(t)
			m := roomMessage("m-"+cmd.Name, "!"+cmd.Aliases[0])
			g.runCommand(g.sess, m, cmd, nil)
			g.inflight.Wait()
			if _, ok := fake.find(http.MethodPost, "/users/@me/channels"); !ok {
				t.Fatalf("no DM channel opened: %+v", fake.calls)
			}
			dm := postsTo(t, fake, "1") // the fake answers every create with id 1
			if len(dm) != 1 || len(dm[0].Embeds) == 0 {
				t.Fatalf("DM posts = %+v, want the card", dm)
			}
			room := postsTo(t, fake, "room")
			loc := g.svc.LocaleFor("u1", g.locale("g1"))
			if len(room) != 1 || len(room[0].Embeds) != 0 || room[0].Content != shippedCatalog().T(loc, "discord.private.dm_sent") {
				t.Fatalf("room posts = %+v, want only the DM line", room)
			}
		})
	}
}

// When the DM cannot be sent the room gets the slash command to use instead, still without the card.
func TestPrivatePrefixCardWithClosedDMsNamesTheSlashCommand(t *testing.T) {
	g, fake := privateCardGateway(t)
	fake.setResponse("/channels/1/messages", http.StatusForbidden, `{"code":50007,"message":"Cannot send messages to this user"}`)
	cmd, _ := ByName(shippedCatalog(), "reminders")
	g.runCommand(g.sess, roomMessage("m1", "!"+cmd.Aliases[0]), cmd, nil)
	room := postsTo(t, fake, "room")
	loc := g.svc.LocaleFor("u1", g.locale("g1"))
	want := shippedCatalog().T(loc, "discord.private.dm_closed", "command", "/"+LocalName(shippedCatalog(), cmd, loc))
	if len(room) != 1 || len(room[0].Embeds) != 0 || room[0].Content != want || !strings.Contains(want, "/"+LocalName(shippedCatalog(), cmd, loc)) {
		t.Fatalf("room posts = %+v, want %q", room, want)
	}
}

// A personal command in a DM channel, and a command that is not personal in a room, answer where they were
// typed, as before.
func TestNonPrivateOrDMCardsStayWhereTyped(t *testing.T) {
	g, fake := privateCardGateway(t)
	help, _ := ByName(shippedCatalog(), "help")
	g.runCommand(g.sess, roomMessage("m1", "!help"), help, nil)
	if room := postsTo(t, fake, "room"); len(room) != 1 || len(room[0].Embeds) == 0 {
		t.Fatalf("help in a room: %+v", room)
	}
	if _, ok := fake.find(http.MethodPost, "/users/@me/channels"); ok {
		t.Fatal("a command that is not personal opened a DM")
	}
	mine, _ := ByName(shippedCatalog(), "mymemory")
	dm := &discordgo.MessageCreate{Message: &discordgo.Message{ID: "m2", ChannelID: "dmchan", Content: "!x", Author: &discordgo.User{ID: "u1"}}}
	g.runCommand(g.sess, dm, mine, nil)
	g.inflight.Wait()
	if got := postsTo(t, fake, "dmchan"); len(got) != 1 || len(got[0].Embeds) == 0 {
		t.Fatalf("mymemory in a DM: %+v", got)
	}
}

// The forget button of a card sent to a DM carries the server; the Action binds its token to the person
// and the server, so the button works there and a changed server part is refused with a fresh preview.
func TestForgetButtonInDMKeepsItsServer(t *testing.T) {
	g, _ := privateCardGateway(t)
	preview := g.svc.Execute(context.Background(), action.Request{Action: "memory.mine.forget", Input: mustJSON(map[string]any{"scope": "all"}),
		Invoker: &action.Invoker{Via: "command", GuildID: "g1", ChannelID: "room", UserID: "u1"}})
	token, _ := preview.Output["token"].(string)
	if !preview.OK || token == "" {
		t.Fatalf("preview: %+v", preview)
	}
	rows := []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{CustomID: "mem:forget:a:" + token, Label: "x"},
		discordgo.Button{CustomID: "remind:cancel:r1", Label: "y"},
	}}}
	out := dmButtons(rows, "g1")[0].(discordgo.ActionsRow).Components
	id := out[0].(discordgo.Button).CustomID
	if id != "mem:forget:a:"+token+":g1" || out[1].(discordgo.Button).CustomID != "remind:cancel:r1" {
		t.Fatalf("buttons for the DM: %q %q", id, out[1].(discordgo.Button).CustomID)
	}
	press := func(customID string) action.Result {
		parts, ictx := dmButtonGuild(strings.Split(customID, ":"), InvokeContext{ChannelID: "dmchan", UserID: "u1"})
		req, ok := featureButton(parts, ictx, "dc:press")
		if !ok {
			t.Fatalf("button %q not understood", customID)
		}
		return g.svc.Execute(context.Background(), req)
	}
	if res := press(strings.TrimSuffix(id, "g1") + "g2"); !res.OK || res.Output["applied"] != false || res.Output["stale"] != true {
		t.Fatalf("a changed server part was acted on: %+v", res.Output)
	}
	if res := press(id); !res.OK || res.Output["applied"] != true {
		t.Fatalf("the DM button did not forget: %+v %+v", res.Output, res.Error)
	}
	// a memory button pressed in a server, or a DM button of another family, is left as it is
	if parts, ictx := dmButtonGuild([]string{"mem", "forget", "a", token, "g9"}, InvokeContext{GuildID: "g1"}); len(parts) != 5 || ictx.GuildID != "g1" {
		t.Fatalf("server press changed: %v %q", parts, ictx.GuildID)
	}
	if parts, ictx := dmButtonGuild([]string{"remind", "cancel", "r1", "x", "g9"}, InvokeContext{}); len(parts) != 5 || ictx.GuildID != "" {
		t.Fatalf("another family changed: %v %q", parts, ictx.GuildID)
	}
}

// In a server waiting for approval a member's !내기억 and !잊어줘 still answer (as the slash commands do),
// by DM; other commands stay quiet.
func TestPendingServerAnswersOwnMemoryCommands(t *testing.T) {
	g, fake := privateCardGateway(t)
	g.log = slog.New(slog.NewTextHandler(io.Discard, nil))
	if r := g.svc.Execute(context.Background(), action.Request{Action: "access.policy", Input: json.RawMessage(`{"server_approval":true}`)}); !r.OK {
		t.Fatalf("approval: %+v", r.Error)
	}
	if g.svc.GuildAllowed("g1") {
		t.Fatal("fixture: g1 is approved")
	}
	for _, name := range []string{"mymemory", "forgetme"} {
		cmd, _ := ByName(shippedCatalog(), name)
		g.onMessage(g.sess, roomMessage("p-"+name, "!"+cmd.Aliases[0]))
	}
	g.inflight.Wait()
	if dm := postsTo(t, fake, "1"); len(dm) != 2 {
		t.Fatalf("DM cards in a pending server = %d, want 2: %+v", len(dm), fake.calls)
	}
	before := len(postsTo(t, fake, "room"))
	weather, _ := ByName(shippedCatalog(), "weather")
	g.onMessage(g.sess, roomMessage("p-weather", "!"+weather.Aliases[0]))
	g.inflight.Wait()
	if after := len(postsTo(t, fake, "room")); after != before {
		t.Fatal("a pending server answered a command that is not one's own memory")
	}
}
