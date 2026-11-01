package discord

import (
	"strings"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/application"
	"masamong/internal/crash"
)

// Feedback on replies without cluttering the chat (application/feedback.go): the bot adds no
// reactions of its own. It reads the reactions people put on its replies, and offers "Rate this reply"
// in the Apps menu of a message (long press or right click), whose buttons only the person who chose
// it sees.

// rateCommandName is the message command's base name; Discord shows the localized one.
const rateCommandName = "Rate this reply"

func rateCommand() *discordgo.ApplicationCommand {
	return &discordgo.ApplicationCommand{Type: discordgo.MessageApplicationCommand, Name: rateCommandName,
		NameLocalizations: ptrLocalizations(localizations("discord.feedback.command"))}
}

func ptrLocalizations(m map[discordgo.Locale]string) *map[discordgo.Locale]string {
	if len(m) == 0 {
		return nil
	}
	return &m
}

func (g *Gateway) onReactionAdd(s *discordgo.Session, r *discordgo.MessageReactionAdd) {
	defer crash.Recover(g.log, "discord reaction")
	if r.MessageReaction == nil || r.Member != nil && r.Member.User != nil && r.Member.User.Bot || g.isSelf(r.UserID) {
		return
	}
	g.svc.ReactionFeedback(r.GuildID, r.MessageID, r.UserID, r.Emoji.Name, true)
}

func (g *Gateway) onReactionRemove(s *discordgo.Session, r *discordgo.MessageReactionRemove) {
	defer crash.Recover(g.log, "discord reaction")
	if r.MessageReaction == nil || g.isSelf(r.UserID) {
		return
	}
	g.svc.ReactionFeedback(r.GuildID, r.MessageID, r.UserID, r.Emoji.Name, false)
}

func (g *Gateway) isSelf(userID string) bool {
	p := g.botID.Load()
	return p != nil && *p == userID
}

// handleRate answers "Rate this reply" with good/bad buttons only the person sees.
func (g *Gateway) handleRate(s *discordgo.Session, i *discordgo.InteractionCreate) {
	loc := localeOf(i.Interaction)
	target := i.ApplicationCommandData().TargetID
	data := &discordgo.InteractionResponseData{Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: noMentions}
	if _, ok := g.svc.ReplyTurn(target); !ok {
		data.Content = messages.T(loc, "discord.feedback.not_reply")
	} else {
		data.Content = messages.T(loc, "discord.feedback.ask")
		data.Components = []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Style: discordgo.SuccessButton, Label: messages.T(loc, "discord.feedback.good"), Emoji: &discordgo.ComponentEmoji{Name: "👍"}, CustomID: "fb:up:" + target},
			discordgo.Button{Style: discordgo.DangerButton, Label: messages.T(loc, "discord.feedback.bad"), Emoji: &discordgo.ComponentEmoji{Name: "👎"}, CustomID: "fb:down:" + target},
		}}}
	}
	ctx, cancel := g.ackContext()
	defer cancel()
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource, Data: data}, discordgo.WithContext(ctx)); err != nil {
		g.log.Warn("discord: rate respond failed", "err", err)
	}
}

// handleRateComponent records a button or reason choice and updates the private message in place:
// 👍 thanks, 👎 asks why (optional), the reason thanks.
func (g *Gateway) handleRateComponent(s *discordgo.Session, i *discordgo.InteractionCreate, parts []string) {
	loc := localeOf(i.Interaction)
	if len(parts) < 3 {
		return
	}
	target := parts[2]
	user := actorID(i.Interaction)
	upd := &discordgo.InteractionResponseData{Components: []discordgo.MessageComponent{}, AllowedMentions: noMentions}
	switch parts[1] {
	case "up":
		g.svc.RateReply(i.GuildID, target, user, true, "")
		upd.Content = messages.T(loc, "discord.feedback.thanks")
	case "down":
		g.svc.RateReply(i.GuildID, target, user, false, "")
		upd.Content = messages.T(loc, "discord.feedback.why")
		opts := make([]discordgo.SelectMenuOption, 0, len(application.FeedbackReasons))
		for _, r := range application.FeedbackReasons {
			opts = append(opts, discordgo.SelectMenuOption{Label: messages.T(loc, "discord.feedback.reason."+r), Value: r})
		}
		upd.Components = []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.SelectMenu{CustomID: "fb:why:" + target, Placeholder: messages.T(loc, "discord.feedback.why_placeholder"), Options: opts},
		}}}
	case "why":
		reason := ""
		if v := i.MessageComponentData().Values; len(v) > 0 {
			reason = strings.TrimSpace(v[0])
		}
		g.svc.RateReply(i.GuildID, target, user, false, reason)
		upd.Content = messages.T(loc, "discord.feedback.thanks")
	default:
		return
	}
	ctx, cancel := g.ackContext()
	defer cancel()
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseUpdateMessage, Data: upd}, discordgo.WithContext(ctx)); err != nil {
		g.log.Warn("discord: rate update failed", "err", err)
	}
}
