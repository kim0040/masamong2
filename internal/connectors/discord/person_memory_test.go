package discord

import (
	"testing"

	"github.com/bwmarrin/discordgo"
)

func TestPersonMemoryNeedsPublicVisibility(t *testing.T) {
	g := &Gateway{sess: &discordgo.Session{State: discordgo.NewState()}}
	const view = discordgo.PermissionViewChannel | discordgo.PermissionReadMessageHistory
	guild := &discordgo.Guild{ID: "guild", Roles: []*discordgo.Role{{ID: "guild", Permissions: view}},
		Channels: []*discordgo.Channel{
			{ID: "public", Type: discordgo.ChannelTypeGuildNews, PermissionOverwrites: []*discordgo.PermissionOverwrite{}},
			{ID: "missing", Type: discordgo.ChannelTypeGuildText},
			{ID: "staff", Type: discordgo.ChannelTypeGuildText, PermissionOverwrites: []*discordgo.PermissionOverwrite{{ID: "guild", Deny: discordgo.PermissionViewChannel}}},
			{ID: "restricted", Type: discordgo.ChannelTypeGuildText, PermissionOverwrites: []*discordgo.PermissionOverwrite{{ID: "member", Type: discordgo.PermissionOverwriteTypeMember, Deny: discordgo.PermissionReadMessageHistory}}},
		},
		Threads: []*discordgo.Channel{{ID: "private", Type: discordgo.ChannelTypeGuildPrivateThread}},
	}
	if err := g.sess.State.GuildAdd(guild); err != nil {
		t.Fatal(err)
	}
	if got := g.PersonMemoryChannels("guild"); len(got) != 0 {
		t.Fatal("disconnected visibility", got)
	}
	g.connected.Store(true)
	got := g.PersonMemoryChannels("guild")
	if len(got) != 4 || !got["public"] {
		t.Fatal(got)
	}
	for _, id := range []string{"staff", "restricted", "private"} {
		if public, known := got[id]; !known || public {
			t.Fatal(id, got)
		}
	}
	if _, known := got["missing"]; known {
		t.Fatal("missing overwrites treated as known")
	}
}
