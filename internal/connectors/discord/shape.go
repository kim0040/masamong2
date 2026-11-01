package discord

import (
	"context"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/application"
	"masamong/internal/chat"
	"masamong/internal/errclass"
)

type deliveryReport = application.DeliveryReport

// Reply shape (docs/plan-memory-conversation.md §5.6). A friend does not send one block of text: two or
// three short thoughts go out as separate messages a moment apart (bubbles), and a reply far longer than a
// chat line shows its first part with a "more" button instead of a wall (trim). Channel setting
// conversation.reply_shape: bubbles (default) | trim | plain.
const (
	bubbleMax      = 3   // messages per reply
	bubbleMaxRunes = 280 // a bubble reply is short; anything longer is sent as one message
	trimAt         = 700 // a reply this long is trimmed
	trimHead       = 420 // the first part keeps whole paragraphs up to about this many characters
	moreKeep       = time.Hour
)

var (
	bubbleGapMin = 800 * time.Millisecond
	bubbleGapMax = 2500 * time.Millisecond
)

// bubbles splits a short reply into its paragraphs (lines), at most bubbleMax; ok is false when the reply
// should go out as written: one line, long, or with code, lists or quotes whose lines belong together.
func bubbles(text string) ([]string, bool) {
	text = strings.TrimSpace(text)
	if text == "" || len([]rune(text)) > bubbleMaxRunes || strings.Contains(text, "```") {
		return nil, false
	}
	var parts []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if listy(line) {
			return nil, false
		}
		parts = append(parts, line)
	}
	if len(parts) < 2 {
		return nil, false
	}
	if len(parts) > bubbleMax {
		parts = append(parts[:bubbleMax-1], strings.Join(parts[bubbleMax-1:], "\n"))
	}
	return parts, true
}

// listy: a line that is part of a list, quote or table (its neighbours read as one block).
func listy(line string) bool {
	for _, p := range []string{"- ", "* ", "• ", "> ", "|", "#"} {
		if strings.HasPrefix(line, p) {
			return true
		}
	}
	i := 0
	for i < len(line) && line[i] >= '0' && line[i] <= '9' {
		i++
	}
	return i > 0 && i < len(line) && (line[i] == '.' || line[i] == ')')
}

// trimReply cuts a long reply after its first paragraphs; rest is "" when it is short enough or has no
// paragraph break to cut at (the reply is then sent whole, split by Discord's limit as before).
func trimReply(text string) (head, rest string) {
	text = strings.TrimSpace(text)
	if len([]rune(text)) < trimAt || strings.Contains(text, "```") {
		return text, ""
	}
	paras := strings.Split(text, "\n\n")
	if len(paras) < 2 {
		paras = strings.Split(text, "\n")
	}
	n, cut := 0, 0
	for i, p := range paras {
		r := len([]rune(p))
		if i > 0 && n+r > trimHead {
			break
		}
		n += r
		cut = i + 1
	}
	if cut >= len(paras) || cut == 0 {
		return text, ""
	}
	sep := "\n\n"
	if !strings.Contains(text, "\n\n") {
		sep = "\n"
	}
	return strings.TrimSpace(strings.Join(paras[:cut], sep)), strings.TrimSpace(strings.Join(paras[cut:], sep))
}

// moreStore keeps the rest of trimmed replies for their button, for an hour.
type moreStore struct {
	mu   sync.Mutex
	rest map[string]moreEntry // reply message id
}

type moreEntry struct {
	text    string
	channel string
	at      time.Time
}

func (s *moreStore) put(messageID, channelID, text string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.rest == nil {
		s.rest = map[string]moreEntry{}
	}
	now := time.Now()
	for k, e := range s.rest {
		if now.Sub(e.at) > moreKeep {
			delete(s.rest, k)
		}
	}
	s.rest[messageID] = moreEntry{text: text, channel: channelID, at: now}
}

func (s *moreStore) peek(messageID string) (moreEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.rest[messageID]
	return e, ok && time.Since(e.at) <= moreKeep
}

func (s *moreStore) take(messageID string) (moreEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.rest[messageID]
	if ok {
		delete(s.rest, messageID)
	}
	return e, ok && time.Since(e.at) <= moreKeep
}

// sendShaped sends a chat reply in the channel's reply shape. It returns what reached the channel (the
// head only, for a trimmed reply), the message IDs, and the delivery report. lead, when set, is a line
// said before the answer (the partial-picture notice): it is the first line or bubble, and a trimmed
// reply keeps it in the part shown, so no path hides it.
func (g *Gateway) sendShaped(ctx context.Context, s *discordgo.Session, guildID, channelID string, ref *discordgo.MessageReference, text, lead, shape string) (string, []string, deliveryReport) {
	lead = strings.Join(strings.Fields(lead), " ")
	if shape == "plain" {
		return g.sendReply(ctx, s, guildID, channelID, ref, withLead(lead, text))
	}
	if shape == "bubbles" {
		if parts, ok := bubbles(withLead(lead, text)); ok {
			return g.sendBubbles(ctx, s, guildID, channelID, ref, parts)
		}
	}
	head, rest := trimReply(text)
	head = withLead(lead, head)
	said, ids, rep := g.sendReply(ctx, s, guildID, channelID, ref, head)
	if rest != "" && len(ids) > 0 && rep.Sent == rep.Parts {
		last := ids[len(ids)-1]
		g.more.put(last, channelID, rest)
		edit := discordgo.NewMessageEdit(channelID, last)
		edit.Components = &[]discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: g.svc.MoreLabel(guildID, channelID), Style: discordgo.SecondaryButton, CustomID: "more:" + last},
		}}}
		if _, err := s.ChannelMessageEditComplex(edit, discordgo.WithContext(ctx)); err != nil {
			// the button could not be attached: send the rest as it is, so nothing is lost
			g.more.take(last)
			restSaid, restIDs, _ := g.sendReply(ctx, s, guildID, channelID, nil, rest)
			return said + "\n\n" + restSaid, append(ids, restIDs...), rep
		}
	}
	return said, ids, rep
}

// withLead puts the lead line (if any) in front of text as a paragraph of its own.
func withLead(lead, text string) string {
	if lead == "" {
		return text
	}
	if strings.TrimSpace(text) == "" {
		return lead
	}
	return lead + "\n\n" + text
}

// sendBubbles sends the parts as separate messages with a typing pause between them.
func (g *Gateway) sendBubbles(ctx context.Context, s *discordgo.Session, guildID, channelID string, ref *discordgo.MessageReference, parts []string) (string, []string, deliveryReport) {
	var said []string
	var ids []string
	rep := deliveryReport{Parts: len(parts)} // each bubble is shorter than Discord's limit: one part
	for i, p := range parts {
		if i > 0 {
			_ = s.ChannelTyping(channelID, discordgo.WithContext(ctx))
			gap := bubbleGapMin + time.Duration(rand.Int64N(int64(bubbleGapMax-bubbleGapMin)+1))
			if !sleepCtx(ctx, gap) {
				rep.Class = errclass.Canceled
				break
			}
			ref = nil // only the first bubble replies to the message
		}
		got, pid, r := g.sendReply(ctx, s, guildID, channelID, ref, p)
		rep.Sent += r.Sent
		rep.Retries += r.Retries
		rep.ReferenceDropped = rep.ReferenceDropped || r.ReferenceDropped
		if r.Sent < r.Parts {
			rep.Class = r.Class
			break
		}
		said = append(said, got)
		ids = append(ids, pid...)
	}
	rep.MessageIDs = ids
	return strings.Join(said, "\n"), ids, rep
}

// handleMore posts the rest of a trimmed reply and takes the button away.
func (g *Gateway) handleMore(s *discordgo.Session, i *discordgo.InteractionCreate, messageID string) {
	ctx, cancel := context.WithTimeout(g.runCtx(), 2500*time.Millisecond)
	defer cancel()
	e, ok := g.more.peek(messageID)
	if !ok {
		// kept for an hour in memory only (or the bot restarted): say so instead of doing nothing
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: messages.T(g.locale(i.GuildID), "discord.more.expired"), Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: noMentions}},
			discordgo.WithContext(ctx))
		return
	}
	content := ""
	var rows []discordgo.MessageComponent
	if i.Message != nil {
		content, rows = i.Message.Content, i.Message.Components
	}
	// the thread offer's buttons, if any, stay
	data := &discordgo.InteractionResponseData{Content: content, Components: componentsWithout(rows, "more:"), AllowedMentions: noMentions}
	if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseUpdateMessage, Data: data},
		discordgo.WithContext(ctx)); err != nil {
		return // the rest stays for another press
	}
	if _, ok := g.more.take(messageID); !ok {
		return // a second press got there first
	}
	g.work(func(base context.Context) {
		wctx, wcancel := context.WithTimeout(base, g.ActionTimeout)
		defer wcancel()
		g.sendReply(wctx, s, i.GuildID, e.channel, &discordgo.MessageReference{MessageID: messageID, ChannelID: e.channel, FailIfNotExists: ptrBool(false)}, e.text)
	})
}

// noticeLocale is the language of a notice to a member: the language they wrote in when the message says
// enough to tell (LIVE-07), else the server's.
func (g *Gateway) noticeLocale(m *discordgo.MessageCreate) string {
	text := strings.TrimSpace(stripMentions(m.Content))
	if len([]rune(text)) >= 4 {
		if tag := chat.DetectLanguage(text); tag != "" {
			return tag
		}
	}
	return g.locale(m.GuildID)
}
