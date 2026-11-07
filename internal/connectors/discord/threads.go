package discord

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/application"
	"masamong/internal/config"
	"masamong/internal/crash"
)

// Threads on the Discord side (the rules live in application/threads.go): tell the application which
// channels are threads of which parent, put the thread offer under a reply, make the thread when the
// caller accepts, and answer untagged messages in the bot's own threads.

// channelKind is what the gateway has learned about a channel: whether it is a thread. Entries are made for
// every channel and thread the bot reads in, so they expire (sweepChannelKinds) instead of growing with
// every thread ever seen.
type channelKind struct {
	thread bool
	seen   atomic.Int64 // unix nanoseconds of the last message that looked it up
}

const (
	// channelKindIdle is shorter than the application's threadKeepHumanOpen: a thread the application let go of
	// has been forgotten here first, so its next message reaches NoteThread and reads the row back.
	channelKindIdle       = 12 * time.Hour
	channelKindCap        = 4096
	channelKindSweepEvery = time.Hour
)

func (g *Gateway) storeChannelKind(id string, thread bool) {
	k := &channelKind{thread: thread}
	k.seen.Store(time.Now().UnixNano())
	g.channelKinds.Store(id, k)
}

// channelKnown reports whether the channel's kind is known, and marks it used now.
func (g *Gateway) channelKnown(id string) bool {
	v, ok := g.channelKinds.Load(id)
	if !ok {
		return false
	}
	if k, isKind := v.(*channelKind); isKind {
		k.seen.Store(time.Now().UnixNano())
	}
	return true
}

// sweepChannelKinds forgets the channels not heard from for channelKindIdle, then the longest unheard beyond
// channelKindCap. Nothing is lost: the kind is asked of the gateway's own cache again.
func (g *Gateway) sweepChannelKinds(now time.Time) {
	type entry struct {
		id   string
		seen int64
	}
	var kept []entry
	g.channelKinds.Range(func(key, value any) bool {
		k, ok := value.(*channelKind)
		if !ok || now.Sub(time.Unix(0, k.seen.Load())) > channelKindIdle {
			g.channelKinds.Delete(key)
			return true
		}
		kept = append(kept, entry{key.(string), k.seen.Load()})
		return true
	})
	if len(kept) <= channelKindCap {
		return
	}
	sort.Slice(kept, func(i, j int) bool { return kept[i].seen < kept[j].seen })
	for _, e := range kept[:len(kept)-channelKindCap] {
		g.channelKinds.Delete(e.id)
	}
}

// noteThread tells the application when a channel is a thread. It reads the gateway's channel cache and,
// for a thread the cache has not seen (one made while the bot was away), asks Discord once.
func (g *Gateway) noteThread(s *discordgo.Session, guildID, channelID string) {
	if guildID == "" || channelID == "" {
		return
	}
	// the gateway has no timer of its own for this; the message path sweeps at most once an hour
	if last := g.kindSweptAt.Load(); time.Since(time.Unix(0, last)) > channelKindSweepEvery && g.kindSweptAt.CompareAndSwap(last, time.Now().UnixNano()) {
		g.sweepChannelKinds(time.Now())
	}
	if g.channelKnown(channelID) {
		return
	}
	var ch *discordgo.Channel
	if s.State != nil {
		ch, _ = s.State.Channel(channelID)
	}
	if ch == nil {
		ctx, cancel := context.WithTimeout(g.runCtx(), 3*time.Second)
		defer cancel()
		ch, _ = s.Channel(channelID, discordgo.WithContext(ctx))
	}
	if ch == nil {
		return // asked again with the next message
	}
	g.storeChannelKind(channelID, ch.IsThread())
	// a private thread is not the parent's room: only the people in it may read what is said there,
	// so it neither inherits the parent's capture nor joins its memory rooms (it is a channel of its own)
	if ch.IsThread() && ch.Type != discordgo.ChannelTypeGuildPrivateThread {
		g.svc.NoteThread(guildID, channelID, ch.ParentID, ch.OwnerID != "" && ch.OwnerID == g.botUserID())
	}
}

func (g *Gateway) onThreadCreate(s *discordgo.Session, t *discordgo.ThreadCreate) {
	defer crash.Recover(g.log, "discord thread create")
	if t.Channel == nil || !t.IsThread() {
		return
	}
	g.storeChannelKind(t.ID, true)
	if t.Type == discordgo.ChannelTypeGuildPrivateThread {
		return // see noteThread
	}
	g.svc.NoteThread(t.GuildID, t.ID, t.ParentID, t.OwnerID != "" && t.OwnerID == g.botUserID())
}

// onThreadUpdate and onThreadDelete keep the application's thread table true to Discord: an archived
// thread is closed there (so its memory can go), an unarchived human thread is open again, a deleted one
// is gone.
func (g *Gateway) onThreadUpdate(s *discordgo.Session, t *discordgo.ThreadUpdate) {
	defer crash.Recover(g.log, "discord thread update")
	if t.Channel == nil || !t.IsThread() || t.ThreadMetadata == nil {
		return
	}
	g.svc.ThreadArchived(t.ID, t.ThreadMetadata.Archived)
}

func (g *Gateway) onThreadDelete(s *discordgo.Session, t *discordgo.ThreadDelete) {
	defer crash.Recover(g.log, "discord thread delete")
	if t.Channel == nil || t.ID == "" {
		return
	}
	g.channelKinds.Delete(t.ID)
	g.svc.ThreadDeleted(t.ID)
}

func (g *Gateway) botUserID() string {
	if p := g.botID.Load(); p != nil {
		return *p
	}
	return ""
}

// threadAnswer decides a message in a thread the bot made: it answers like a chat with one person —
// without a tag — the people who spoke to it there (or everyone, thread_follow: all), but still leaves
// alone what is said to someone else and messages with next to nothing in them. ok is false outside the
// bot's open threads.
func (g *Gateway) threadAnswer(m *discordgo.MessageCreate, pol config.ChannelPolicy, sure bool) (answer, ok bool) {
	if m.GuildID == "" || pol.Conversation.Thread == "off" || (pol.Respond != "mention" && pol.Respond != "always") {
		return false, false
	}
	v, known := g.svc.ThreadFor(m.ChannelID, m.Author.ID)
	if !known || !v.BotOwned || !v.Open {
		return false, false
	}
	if sure {
		g.svc.NoteThreadParticipant(m.ChannelID, m.Author.ID)
		return true, true
	}
	if g.toSomeone(m) || g.svc.CallsSomeoneElse(m.GuildID, m.ChannelID, m.Author.ID, m.Content) || thinMessage(m.Content) {
		return false, true
	}
	return pol.Conversation.ThreadFollow == "all" || v.Participant || v.Alone, true
}

// thinMessage: nothing to answer — no letters or digits, two or fewer of them ("ok", "ㅇㅋ"), or one
// character repeated ("ㅋㅋㅋㅋ", "www").
func thinMessage(text string) bool {
	var letters []rune
	for _, r := range strings.TrimSpace(text) {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			letters = append(letters, r)
		}
	}
	if len(letters) <= 2 {
		return true
	}
	for _, r := range letters[1:] {
		if r != letters[0] {
			return false
		}
	}
	return true
}

// canThread reports whether the bot may make a thread in a channel and talk in it.
func (g *Gateway) canThread(s *discordgo.Session, channelID string) bool {
	if s == nil || s.State == nil {
		return false
	}
	perms, err := s.State.UserChannelPermissions(g.botUserID(), channelID)
	if err != nil {
		return false
	}
	need := int64(discordgo.PermissionCreatePublicThreads | discordgo.PermissionSendMessagesInThreads)
	return perms&need == need
}

// offerThread puts the turn's thread offer under the reply that was just sent (or, with
// thread: auto_task, makes the thread right away).
func (g *Gateway) offerThread(ctx context.Context, s *discordgo.Session, m *discordgo.MessageCreate, turnID string, replyIDs []string) {
	offer, ok := g.svc.TakeThreadOffer(turnID)
	if !ok {
		return
	}
	if len(replyIDs) == 0 {
		g.svc.ThreadOfferFailed(offer, "not_sent")
		return
	}
	last := replyIDs[len(replyIDs)-1]
	// the decision was made at the start of the turn: the room may have been switched off since, and
	// neither a button nor an automatic thread may appear there
	if g.svc.ThreadsOff(m.GuildID, m.ChannelID) {
		g.svc.ThreadOfferFailed(offer, "thread_off")
		return
	}
	if !g.canThread(s, m.ChannelID) {
		g.svc.ThreadOfferFailed(offer, "no_permission")
		return
	}
	if offer.Auto {
		g.openThread(ctx, s, m.GuildID, m.ChannelID, last, offer.Caller, displayName(m), m.Content, turnID, "")
		return
	}
	// the offer is a line and two buttons on the reply itself: a separate message would clutter the
	// channel the thread is meant to tidy
	msg, err := s.ChannelMessage(m.ChannelID, last, discordgo.WithContext(ctx))
	if err != nil {
		g.svc.ThreadOfferFailed(offer, "edit_failed")
		return
	}
	edit := discordgo.NewMessageEdit(m.ChannelID, last)
	content := msg.Content + "\n-# " + offer.Text
	if utf8.RuneCountInString(content) > 2000 {
		content = msg.Content
	}
	edit.Content = &content
	edit.AllowedMentions = noMentions
	// a "more" button already on the reply stays (shape.go)
	rows := append([]discordgo.MessageComponent(nil), msg.Components...)
	rows = append(rows, discordgo.ActionsRow{Components: []discordgo.MessageComponent{
		discordgo.Button{Label: offer.Open, Style: discordgo.PrimaryButton, CustomID: "thr:open:" + turnID + ":" + offer.Caller, Emoji: &discordgo.ComponentEmoji{Name: "🧵"}},
		discordgo.Button{Label: offer.Stay, Style: discordgo.SecondaryButton, CustomID: "thr:stay:" + turnID + ":" + offer.Caller},
	}})
	edit.Components = &rows
	if _, err := s.ChannelMessageEditComplex(edit, discordgo.WithContext(ctx)); err != nil {
		g.log.Debug("discord: thread offer not attached", "err", err)
		g.svc.ThreadOfferFailed(offer, "edit_failed")
	}
}

// dropThreadOffer gives back the offer of a turn whose reply did not go out.
func (g *Gateway) dropThreadOffer(turnID, reason string) {
	if offer, ok := g.svc.TakeThreadOffer(turnID); ok {
		g.svc.ThreadOfferFailed(offer, reason)
	}
}

// openThread makes a thread on the bot's reply, seeds it and posts the first message with a stop button.
// joiner, when set, is someone else who pressed the open button: they are in the conversation too.
func (g *Gateway) openThread(ctx context.Context, s *discordgo.Session, guildID, channelID, messageID, callerID, callerName, topic, turnID, joiner string) bool {
	name := threadName(callerName, topic)
	th, err := s.MessageThreadStartComplex(channelID, messageID, &discordgo.ThreadStart{Name: name, AutoArchiveDuration: 1440, Type: discordgo.ChannelTypeGuildPublicThread},
		discordgo.WithContext(ctx))
	if err != nil {
		g.log.Warn("discord: thread not created", "err", err)
		g.svc.ThreadOfferDropped(guildID, channelID, "create_failed")
		return false
	}
	g.storeChannelKind(th.ID, true)
	first := g.svc.ThreadOpened(ctx, guildID, channelID, th.ID, callerID, turnID)
	if joiner != "" && joiner != callerID {
		g.svc.NoteThreadParticipant(th.ID, joiner)
	}
	stop := g.svc.ThreadStopLabel(guildID, channelID)
	_, err = s.ChannelMessageSendComplex(th.ID, &discordgo.MessageSend{Content: first,
		AllowedMentions: &discordgo.MessageAllowedMentions{Users: []string{callerID}},
		Components: []discordgo.MessageComponent{discordgo.ActionsRow{Components: []discordgo.MessageComponent{
			discordgo.Button{Label: stop, Style: discordgo.SecondaryButton, CustomID: "thr:stop:" + th.ID},
		}}}}, discordgo.WithContext(ctx))
	if err != nil {
		g.log.Warn("discord: thread first message failed", "err", err)
	}
	return true
}

// threadName is the thread's title: who and what, within Discord's 100 characters.
func threadName(speaker, topic string) string {
	topic = strings.Join(strings.Fields(topic), " ")
	topic = strings.TrimSpace(stripMentions(topic))
	if r := []rune(topic); len(r) > 40 {
		topic = string(r[:40]) + "…"
	}
	name := speaker
	switch {
	case topic != "" && strings.TrimSpace(speaker) == "":
		name = topic
	case topic != "":
		name += " · " + topic
	}
	if r := []rune(name); len(r) > 100 {
		name = string(r[:100])
	}
	if strings.TrimSpace(name) == "" {
		name = "🧵"
	}
	return name
}

// stripMentions removes <@id>, <@!id>, <@&id> and <#id> tokens.
func stripMentions(s string) string {
	var b strings.Builder
	for {
		i := strings.Index(s, "<")
		if i < 0 {
			b.WriteString(s)
			return b.String()
		}
		j := strings.Index(s[i:], ">")
		if j < 0 || !(strings.HasPrefix(s[i:], "<@") || strings.HasPrefix(s[i:], "<#")) {
			b.WriteString(s[:i+1])
			s = s[i+1:]
			continue
		}
		b.WriteString(s[:i])
		s = s[i+j+1:]
	}
}

// handleThreadComponent answers the thread buttons: thr:open|stay:<turn>:<caller> and thr:stop:<thread>.
func (g *Gateway) handleThreadComponent(s *discordgo.Session, i *discordgo.InteractionCreate, parts []string) {
	actor := actorID(i.Interaction)
	ctx, cancel := context.WithTimeout(g.runCtx(), 2500*time.Millisecond)
	defer cancel()
	ephemeral := func(text string) {
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseChannelMessageWithSource,
			Data: &discordgo.InteractionResponseData{Content: text, Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: noMentions}},
			discordgo.WithContext(ctx))
	}
	switch {
	case len(parts) == 4 && (parts[1] == "open" || parts[1] == "stay"):
		turnID, caller := parts[2], parts[3]
		// anyone in the channel may open the thread and join it; only the person asked may turn it down
		if parts[1] == "stay" && actor != caller {
			ephemeral(g.svc.ThreadNotYours(i.GuildID, i.ChannelID))
			return
		}
		// the buttons go away first, so a second press cannot make a second thread
		content := strings.TrimSpace(i.Message.Content)
		if k := strings.LastIndex(content, "\n-# "); k >= 0 {
			content = content[:k]
		}
		rest := componentsWithout(i.Message.Components, "thr:") // a "more" button on the reply stays
		if err := s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{Content: content, Components: rest, AllowedMentions: noMentions}}, discordgo.WithContext(ctx)); err != nil {
			g.log.Debug("discord: thread button ack failed", "err", err)
			return
		}
		if parts[1] == "stay" {
			g.svc.DeclineThread(i.GuildID, i.ChannelID, caller)
			return
		}
		// a button outlives the policy it was made under: when the room's threads have been switched off
		// since, no thread is made. The buttons are already gone; only the one who pressed is told.
		if g.svc.ThreadsOff(i.GuildID, i.ChannelID) {
			if _, err := s.FollowupMessageCreate(i.Interaction, false, &discordgo.WebhookParams{Content: g.svc.ThreadOffNotice(i.GuildID, i.ChannelID),
				Flags: discordgo.MessageFlagsEphemeral, AllowedMentions: noMentions}, discordgo.WithContext(ctx)); err != nil {
				g.log.Debug("discord: thread off notice failed", "err", err)
			}
			return
		}
		name := interactionName(i.Interaction)
		if actor != caller {
			name = "" // named after the person asked (below) or only by its topic, never after whoever pressed
		}
		topic := ""
		if i.Message.MessageReference != nil {
			if ref, err := s.ChannelMessage(i.ChannelID, i.Message.MessageReference.MessageID); err == nil {
				topic = ref.Content
				if actor != caller && ref.Author != nil && ref.Author.ID == caller {
					name = messageAuthorName(ref) // the thread is named after the person asked, not whoever pressed
				}
			}
		}
		joiner := ""
		if actor != caller {
			joiner = actor
		}
		g.work(func(base context.Context) {
			wctx, wcancel := context.WithTimeout(base, g.ActionTimeout)
			defer wcancel()
			g.openThread(wctx, s, i.GuildID, i.ChannelID, i.Message.ID, caller, name, topic, turnID, joiner)
		})
	case len(parts) == 3 && parts[1] == "stop":
		// like the stop words: only a person the bot talks with there ends it (and posts its recap)
		if v, ok := g.svc.ThreadFor(parts[2], actor); ok && !v.Participant {
			ephemeral(g.svc.ThreadNotYours(i.GuildID, i.ChannelID))
			return
		}
		_ = s.InteractionRespond(i.Interaction, &discordgo.InteractionResponse{Type: discordgo.InteractionResponseUpdateMessage,
			Data: &discordgo.InteractionResponseData{Content: i.Message.Content, Components: []discordgo.MessageComponent{}, AllowedMentions: noMentions}},
			discordgo.WithContext(ctx))
		threadID := parts[2]
		g.work(func(base context.Context) {
			wctx, wcancel := context.WithTimeout(base, g.ActionTimeout)
			defer wcancel()
			_ = g.svc.CloseThread(wctx, threadID, "stop")
		})
	}
}

// componentsWithout is a message's component rows without the buttons whose custom id starts with
// prefix (rows left empty go too): pressing one button keeps the others on the message.
func componentsWithout(rows []discordgo.MessageComponent, prefix string) []discordgo.MessageComponent {
	out := []discordgo.MessageComponent{}
	for _, c := range rows {
		var row *discordgo.ActionsRow
		switch r := c.(type) {
		case *discordgo.ActionsRow:
			row = r
		case discordgo.ActionsRow:
			row = &r
		default:
			out = append(out, c)
			continue
		}
		var kept []discordgo.MessageComponent
		for _, b := range row.Components {
			id := ""
			switch v := b.(type) {
			case *discordgo.Button:
				id = v.CustomID
			case discordgo.Button:
				id = v.CustomID
			}
			if id != "" && strings.HasPrefix(id, prefix) {
				continue
			}
			kept = append(kept, b)
		}
		if len(kept) > 0 {
			out = append(out, discordgo.ActionsRow{Components: kept})
		}
	}
	return out
}

// ArchiveThread archives a thread the bot made (application.ThreadArchiver).
func (g *Gateway) ArchiveThread(ctx context.Context, threadID string) error {
	archived := true
	_, err := g.sess.ChannelEditComplex(threadID, &discordgo.ChannelEdit{Archived: &archived}, discordgo.WithContext(ctx))
	return err
}

var _ application.ThreadArchiver = (*Gateway)(nil)

// pollIDOf is the poll id in a polls.create result.
func pollIDOf(out any) string {
	if m, ok := out.(map[string]any); ok {
		if id, ok := m["id"].(string); ok {
			return id
		}
	}
	return ""
}

func (g *Gateway) onPollVoteAdd(s *discordgo.Session, v *discordgo.MessagePollVoteAdd) {
	defer crash.Recover(g.log, "discord poll vote")
	g.svc.PollVote(v.MessageID, v.UserID, v.AnswerID, true)
}

func (g *Gateway) onPollVoteRemove(s *discordgo.Session, v *discordgo.MessagePollVoteRemove) {
	defer crash.Recover(g.log, "discord poll unvote")
	g.svc.PollVote(v.MessageID, v.UserID, v.AnswerID, false)
}

// React puts a reaction on a person's message (application.ChannelReactor: the bot stepping back from
// chiming in says so with an emoji instead of another message).
func (g *Gateway) React(ctx context.Context, channelID, messageID, emoji string) error {
	if g.sess == nil {
		return errors.New("discord not connected")
	}
	return g.sess.MessageReactionAdd(channelID, messageID, emoji, discordgo.WithContext(ctx))
}

var _ application.ChannelReactor = (*Gateway)(nil)
