package discord

import (
	"context"
	"strings"

	"github.com/bwmarrin/discordgo"
)

// Personal cards of "!" commands (Command.Private: one's memory, reminders, consent, settings, registrations)
// show stored data about the caller. In a server room everyone would read them, so the card goes to the
// caller's DMs and the room gets one line; when the DM cannot be sent, the line names the slash command,
// whose answer only the caller sees. A DM channel is private already and gets the card as before.

// sendPrivateCard delivers msg (a rendered card) to the author's DMs and leaves the line in the room.
func (g *Gateway) sendPrivateCard(ctx context.Context, s *discordgo.Session, m *discordgo.MessageCreate, cmd Command, ictx InvokeContext, msg *discordgo.MessageSend) {
	msg.Reference = nil // the command's message is in the room, not in the DM
	msg.Components = dmButtons(msg.Components, m.GuildID)
	note := messages.T(ictx.Locale, "discord.private.dm_sent")
	ch, err := s.UserChannelCreate(m.Author.ID, discordgo.WithContext(ctx))
	if err == nil {
		_, err = s.ChannelMessageSendComplex(ch.ID, msg, discordgo.WithContext(ctx))
	}
	if err != nil {
		g.log.Warn("discord: private card not sent by DM", "cmd", cmd.Name, "err", dmError(err))
		note = messages.T(ictx.Locale, "discord.private.dm_closed", "command", "/"+LocalName(cmd, ictx.Locale))
	}
	if ctx.Err() != nil {
		return
	}
	line := &discordgo.MessageSend{Content: note, Reference: m.Reference(), AllowedMentions: noMentions}
	if _, err := s.ChannelMessageSendComplex(m.ChannelID, line, discordgo.WithContext(ctx)); err != nil {
		g.log.Warn("discord: private card notice failed", "cmd", cmd.Name, "err", err)
	}
}

// dmButtons carries the server into the buttons that act on it: pressed in a DM, an interaction names no
// server. Only the memory buttons (mem:forget:<scope>:<token>) need it; their token is minted for this
// person and this server, so a changed server part is refused by the Action (a fresh preview).
func dmButtons(rows []discordgo.MessageComponent, guildID string) []discordgo.MessageComponent {
	if guildID == "" {
		return rows
	}
	out := make([]discordgo.MessageComponent, 0, len(rows))
	for _, c := range rows {
		row, ok := c.(discordgo.ActionsRow)
		if !ok {
			out = append(out, c)
			continue
		}
		items := make([]discordgo.MessageComponent, 0, len(row.Components))
		for _, item := range row.Components {
			if b, ok := item.(discordgo.Button); ok && strings.HasPrefix(b.CustomID, "mem:") && strings.Count(b.CustomID, ":") == 3 {
				b.CustomID += ":" + guildID
				item = b
			}
			items = append(items, item)
		}
		row.Components = items
		out = append(out, row)
	}
	return out
}

// dmButtonGuild reads the server dmButtons added to a memory button pressed in a DM, and gives the parts
// back as the button family expects them. Elsewhere it changes nothing.
func dmButtonGuild(parts []string, ictx InvokeContext) ([]string, InvokeContext) {
	if ictx.GuildID != "" || len(parts) != 5 || parts[0] != "mem" || parts[4] == "" {
		return parts, ictx
	}
	ictx.GuildID = parts[4]
	return parts[:4], ictx
}
