package discord

import (
	"context"
	"time"

	"github.com/bwmarrin/discordgo"
)

type dmMembership struct {
	allowed bool
	until   time.Time
}

// SharedGuildMember uses cached gateway state before a bounded REST lookup. It needs no member intent.
func (g *Gateway) SharedGuildMember(ctx context.Context, userID string) bool {
	if g.sess == nil || g.sess.State == nil {
		return false
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	now := time.Now()
	last := g.dmSweptAt.Load()
	if now.UnixNano()-last >= int64(time.Minute) && g.dmSweptAt.CompareAndSwap(last, now.UnixNano()) {
		// Bound stale entries even when accounts stop messaging the bot.
		g.dmMembers.Range(func(key, value any) bool {
			if !now.Before(value.(dmMembership).until) {
				g.dmMembers.Delete(key)
			}
			return true
		})
	}
	guilds := g.Guilds()
	for _, guild := range guilds {
		if !g.svc.GuildAllowed(guild.ID) {
			continue
		}
		if member, err := g.sess.State.Member(guild.ID, userID); err == nil && member != nil {
			return true
		}
		key := guild.ID + ":" + userID
		if cached, ok := g.dmMembers.Load(key); ok {
			hit := cached.(dmMembership)
			if now.Before(hit.until) {
				if hit.allowed {
					return true
				}
				continue
			}
			g.dmMembers.Delete(key)
		}
		member, err := g.sess.GuildMember(guild.ID, userID, discordgo.WithContext(ctx))
		allowed := err == nil && member != nil && member.User != nil && member.User.ID == userID
		ttl := time.Minute
		if allowed {
			ttl = 10 * time.Minute
		}
		g.dmMembers.Store(key, dmMembership{allowed: allowed, until: now.Add(ttl)})
		if allowed {
			return true
		}
		if ctx.Err() != nil {
			break
		}
	}
	return false
}

func (g *Gateway) sendDMNotice(ctx context.Context, s *discordgo.Session, m *discordgo.MessageCreate, key, kind string) {
	if ctx.Err() != nil || !g.svc.ClaimDMNotice(m.Author.ID, kind) {
		return
	}
	msg := &discordgo.MessageSend{Content: messages.T(g.locale(""), key), Reference: m.Reference(), AllowedMentions: noMentions}
	if _, err := s.ChannelMessageSendComplex(m.ChannelID, msg, discordgo.WithContext(ctx)); err != nil {
		g.log.Warn("discord: DM notice failed", "err", err)
	}
}
