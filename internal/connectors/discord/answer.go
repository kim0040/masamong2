package discord

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/application"
	"masamong/internal/chat"
)

// answerTurn is one chat turn on its way to Discord: the typing indicator, the "still looking" line, and
// what of the answer reached the channel.
type answerTurn struct {
	g *Gateway
	s *discordgo.Session
	m *discordgo.MessageCreate
	// addressed: the person called the bot. A name alone is not a call until the name check says it was
	// one (the Addressed callback, which the turn calls from its own goroutine); a notice goes only
	// to someone who called.
	addressed    atomic.Bool
	visionNotice string
	typing       *typingKeepalive
	// the "still looking" line of a turn that searches long. Every way out of answer deletes it (on a
	// context of its own: the turn's is cancelled by the stop word and expires on its timeout).
	still    *interim
	turnDone chan struct{}
	// replied: something of the answer reached the channel (a part of the text or a post of a tool).
	// unsent: there was an answer to post and none of it did.
	delivered, replied, unsent bool
}

// answer runs one chat turn and replies. Mentions stay disabled; long answers are split at 2000 chars.
// Turns in different channels run in parallel; the application keeps each channel in arrival order.
func (g *Gateway) answer(base context.Context, s *discordgo.Session, m *discordgo.MessageCreate, text string, call turnCall) {
	t := &answerTurn{g: g, s: s, m: m}
	t.addressed.Store(call.addressed)
	// the turn's own time (chatTurnTimeout, the previous bot's pictures: p90 88 s) starts when it has its
	// place in the channel, not while it waits (ChatInput.TurnTimeout)
	ctx, cancel := context.WithTimeout(base, chatTurnCeiling)
	defer cancel()
	ctx, stopReply := context.WithCancelCause(ctx)
	defer stopReply(nil)
	defer g.turns.add(m.ChannelID, m.Author.ID, stopReply)()
	t.typing = newTypingKeepalive(s, m.ChannelID, g.log)
	defer t.typing.stop()
	t.still = g.newInterim(base, s, m)
	defer t.still.clear()
	// a call by name alone types only once the bot knows it was said to it (else it would type, then
	// stay quiet)
	if !call.nameOnly {
		t.typing.start(ctx)
	}
	t.turnDone = make(chan struct{})
	defer close(t.turnDone)
	// the reply is posted inside the turn, so the channel's next turn starts after it and the
	// conversation keeps only what reached Discord
	_, err := g.svc.ChatTurn(ctx, t.input(ctx, text, call))
	t.still.clear() // before any notice below: a refusal or a failure line must not stand next to "still looking"
	if errors.Is(context.Cause(ctx), errTurnStopped) {
		g.log.Debug("discord: reply stopped by the person") // quiet: no notice, no "restarting"
		return
	}
	t.report(ctx, base, err)
}

// input is the turn's request, with the callbacks that type, show progress and deliver the answer.
func (t *answerTurn) input(ctx context.Context, text string, call turnCall) application.ChatInput {
	m := t.m
	return application.ChatInput{GuildID: m.GuildID, ChannelID: m.ChannelID, MessageID: m.ID, Message: text,
		Speaker: displayName(m), SpeakerID: m.Author.ID, MaxWait: chatQueueWait, TurnTimeout: chatTurnTimeout, NameOnly: call.nameOnly, FollowUp: call.followUp, Addressed: func() { t.addressed.Store(true); t.typing.start(ctx) },
		Images: call.images, ImageOnly: call.imageOnly, ImageEarlier: call.imageEarlier, ImageTotal: call.imageTotal, ImageSkipped: call.imageSkipped,
		Fragments: call.fragments, ReadFrom: call.readFrom,
		OnVisionNotice: func(note string) { t.visionNotice = note },
		OnProgress:     t.still.show,
		Deliver:        t.deliver,
	}
}

// deliver posts the answer (the tools' posts, then the shaped text) and records what reached the channel.
func (t *answerTurn) deliver(deliverCtx context.Context, res chat.Result) string {
	g, s, m := t.g, t.s, t.m
	t.typing.stop()
	t.still.clear() // first: the line is gone before any part of the answer is posted
	if deliverCtx.Err() != nil {
		g.dropThreadOffer(res.TurnID, "not_sent")
		t.unsent = strings.TrimSpace(res.Answer) != ""
		return ""
	}
	posted := g.postToolArtifacts(deliverCtx, s, m.GuildID, m.ChannelID, m.Reference(), res.Tools)
	t.replied = posted
	if strings.TrimSpace(res.Answer) == "" {
		g.log.Warn("discord: chat turn produced no reply text", "tools", len(res.Tools))
		t.delivered = true // nothing to say is not a cut-off reply
		t.replied = true   // nor an answer that did not come: there was none to send
		return ""
	}
	shape := g.svc.ChannelPolicyFor(m.GuildID, m.ChannelID).Conversation.ReplyShape
	sent, ids, rep := g.sendShaped(deliverCtx, s, shapedReply{guildID: m.GuildID, channelID: m.ChannelID, ref: m.Reference(), text: res.Answer,
		lead: t.visionNotice, shape: shape, deep: deepSearched(res.Tools), turnDone: t.turnDone})
	g.svc.NoteReplyMessages(res.TurnID, m.GuildID, m.ChannelID, ids, rep.PartEnds...)
	g.svc.NoteDelivery(res.TurnID, m.GuildID, m.ChannelID, rep)
	t.delivered = rep.Sent == rep.Parts && rep.Parts > 0
	if sent != "" || rep.Sent > 0 {
		t.replied = true // from here on the person has been answered, in whole or in part
	}
	t.unsent = !t.replied
	if sent != "" && m.GuildID != "" {
		g.offerThread(deliverCtx, s, m, res.TurnID, ids)
	} else {
		g.dropThreadOffer(res.TurnID, "not_sent")
	}
	return sent
}

// report maps how the turn ended to what the person sees: a notice, a reaction, or nothing.
func (t *answerTurn) report(ctx, base context.Context, err error) {
	g, s, m := t.g, t.s, t.m
	var react application.ReactOnly
	var dmLimit application.DMLimited
	switch {
	case errors.As(err, &dmLimit):
		g.sendDMNotice(ctx, s, m, "discord.chat.dm_limited", dmLimit.Scope)
	case errors.Is(err, application.ErrDMClosedChat):
		g.sendDMNotice(ctx, s, m, "discord.chat.dm_closed", "closed")
	case errors.As(err, &react):
		// a playful mention answered with an emoji on the message (banter, experimental)
		g.sendBanterReaction(ctx, s, m.ChannelID, m.ID, react)
	case errors.Is(err, application.ErrNotAddressed):
		g.log.Debug("discord: reply skipped", "reason", "name mentioned, not addressed")
	case errors.Is(err, application.ErrChatDisabled):
		g.log.Debug("discord: reply skipped", "reason", err)
	case errors.Is(err, application.ErrNoVision), errors.Is(err, application.ErrImageUnreadable):
		// only a picture, and nothing could look at it: a short honest line instead of a guess
		g.sendChatNotice(ctx, s, m, chatNoticeKey(err), t.addressed.Load())
	case errors.Is(err, application.ErrChatLimited), errors.Is(err, application.ErrChatBusy),
		errors.Is(err, application.ErrChatPaused):
		g.log.Debug("discord: reply skipped", "reason", err)
		g.sendChatNotice(ctx, s, m, chatNoticeKey(err), t.addressed.Load())
	case base.Err() != nil && g.stopping.Load() && !t.delivered && (err == nil || errors.Is(err, context.Canceled)):
		// the daemon is stopping and this turn ran out of grace — while thinking, or while its reply was
		// going out: say so instead of going quiet
		nctx, ncancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer ncancel()
		g.sendChatNotice(nctx, s, m, "discord.chat.restarting", t.addressed.Load())
	case errors.Is(err, context.Canceled) && base.Err() != nil:
		// cancelled with the daemon run: nothing more to say
	case errors.Is(err, application.ErrChatBudget):
		g.log.Warn("discord: reply skipped: monthly model budget reached")
		g.sendChatNotice(ctx, s, m, chatNoticeKey(err), t.addressed.Load())
	case err == nil && t.unsent && !t.replied:
		// the answer was made and none of it reached the channel (the first send failed, or the turn's time
		// ran out before it): the person called the bot and got nothing, so the same short line as for a turn
		// that failed, on a context that does not carry the turn's own (possibly expired) deadline
		g.log.Warn("discord: chat answer not delivered")
		if key := chatFailureKey(errNothingDelivered, t.replied); key != "" && base.Err() == nil && !g.stopping.Load() {
			nctx, ncancel := g.finalContext(base)
			defer ncancel()
			g.sendChatNotice(nctx, s, m, key, t.addressed.Load())
		}
	case err != nil:
		g.log.Warn("discord: chat turn failed", "err", err)
		// the person called the bot and got nothing: one short line, on a context that does not carry the
		// turn's own (possibly expired) deadline. Not when the turn already answered, or the daemon stops.
		if key := chatFailureKey(err, t.replied); key != "" && base.Err() == nil && !g.stopping.Load() {
			nctx, ncancel := g.finalContext(base)
			defer ncancel()
			g.sendChatNotice(nctx, s, m, key, t.addressed.Load())
		}
	}
}
