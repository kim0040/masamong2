package discord

import (
	"container/list"
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/vision"
)

// Inbound message handling that is not about commands: what a message says when it is a forward or a
// sticker, replayed events, and a person's "stop".

const (
	seenTTL = 5 * time.Minute
	seenCap = 2000
	// stickerNameMax and forwardMax bound what a message's metadata adds to the text a turn sees.
	stickerNameMax = 40
	forwardMax     = 3
)

// forwardMark starts the text of a forwarded message, next to the transcript's ↩ for a reply.
const forwardMark = "↪ forwarded: "

// bodyOf is the text of a message as the bot reads it: its content, then "[sticker: name]" for each
// sticker (a sticker alone is not an empty message), then each forwarded message's text behind
// forwardMark. A forward's own content is empty; what was forwarded sits in its snapshots.
func bodyOf(m *discordgo.Message) string {
	if m == nil {
		return ""
	}
	parts := []string{}
	if c := strings.TrimSpace(m.Content); c != "" {
		parts = append(parts, c)
	}
	parts = append(parts, stickerTags(m)...)
	for i, snap := range m.MessageSnapshots {
		if i >= forwardMax {
			break
		}
		if snap.Message == nil {
			continue
		}
		var inner []string
		if c := strings.TrimSpace(snap.Message.Content); c != "" {
			inner = append(inner, c)
		}
		inner = append(inner, stickerTags(snap.Message)...)
		if len(inner) > 0 {
			parts = append(parts, forwardMark+strings.Join(inner, " "))
		}
	}
	return strings.Join(parts, " ")
}

// stickerTags names a message's stickers; a name is someone else's text, so it is flattened and short.
func stickerTags(m *discordgo.Message) []string {
	var tags []string
	for _, st := range m.StickerItems {
		if st == nil {
			continue
		}
		name := strings.Join(strings.Fields(strings.NewReplacer("[", " ", "]", " ").Replace(st.Name)), " ")
		if r := []rune(name); len(r) > stickerNameMax {
			name = string(r[:stickerNameMax])
		}
		if name == "" {
			name = "?"
		}
		tags = append(tags, "[sticker: "+name+"]")
	}
	return tags
}

type imageSelection struct {
	Refs    []vision.Ref
	Total   int
	Skipped map[string]int
}

// selectImageRefs lists the pictures the request called the bot with. All messages in the request are
// considered before any replied-to message, so a referenced picture cannot crowd out a picture attached
// to a later piece of the request. Only PNG, JPEG, WebP and GIF count; nothing is downloaded here.
func selectImageRefs(msgs []*discordgo.Message) imageSelection {
	out := imageSelection{Skipped: map[string]int{}}
	seen := map[string]bool{}
	add := func(messageID, authorID string, atts []*discordgo.MessageAttachment) {
		for _, a := range atts {
			if a == nil || a.URL == "" || seen[a.URL] || !vision.ImageType(a.ContentType, a.Filename) {
				continue
			}
			seen[a.URL] = true
			out.Total++
			if len(out.Refs) >= vision.MaxImages {
				out.Skipped[vision.ReasonOverLimit]++
				continue
			}
			out.Refs = append(out.Refs, vision.Ref{URL: a.URL, ContentType: a.ContentType, Filename: a.Filename, Size: a.Size, MessageID: messageID, AuthorID: authorID})
		}
	}
	for _, m := range msgs {
		if m == nil {
			continue
		}
		add(m.ID, authorOf(m), m.Attachments)
	}
	for _, m := range msgs {
		if m == nil {
			continue
		}
		for i, snap := range m.MessageSnapshots {
			if i >= forwardMax {
				break
			}
			if snap.Message != nil {
				// a forward's copy belongs to the forwarding message; its author, when Discord says, is
				// the original one (whose consent decides), else the forwarding person
				author := authorOf(snap.Message)
				if author == "" {
					author = authorOf(m)
				}
				add(m.ID, author, snap.Message.Attachments)
			}
		}
	}
	for _, m := range msgs {
		if m == nil {
			continue
		}
		if r := m.ReferencedMessage; r != nil && (m.MessageReference == nil || m.MessageReference.Type != discordgo.MessageReferenceTypeForward) {
			add(r.ID, authorOf(r), r.Attachments)
		}
	}
	return out
}

// imageRefs is the selected-reference view used by callers that do not need omission counts.
func imageRefs(msgs []*discordgo.Message) []vision.Ref { return selectImageRefs(msgs).Refs }

func authorOf(m *discordgo.Message) string {
	if m == nil || m.Author == nil {
		return ""
	}
	return m.Author.ID
}

// onlyCalls reports messages that say nothing but the call (a mention, the bot's name, or nothing at
// all, as in a reply to the bot that is only a picture): with pictures, the pictures are the request.
func onlyCalls(msgs []*discordgo.Message, botID string, names []string) bool {
	for _, m := range msgs {
		body := strings.TrimSpace(bodyOf(m))
		if body != "" && !bareCall(body, botID, names) {
			return false
		}
	}
	return true
}

// carriesMore reports a message whose meaning is not only its content: an attachment, a reply, a
// forward or a sticker.
func carriesMore(m *discordgo.Message) bool {
	return len(m.Attachments) > 0 || m.ReferencedMessage != nil || len(m.MessageSnapshots) > 0 || len(m.StickerItems) > 0
}

// seenSet remembers message IDs for seenTTL, at most seenCap of them (the oldest go first). Discord can
// replay MESSAGE_CREATE after a RESUME; the second delivery must not reply or capture again.
type seenSet struct {
	mu    sync.Mutex
	ids   map[string]*list.Element // value: seenEntry
	order list.List                // oldest at the front
}

type seenEntry struct {
	id string
	at time.Time
}

// first reports whether id is new, and remembers it.
func (s *seenSet) first(id string, now time.Time) bool {
	if id == "" {
		return true
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.ids == nil {
		s.ids = map[string]*list.Element{}
	}
	for e := s.order.Front(); e != nil; e = s.order.Front() {
		old := e.Value.(seenEntry)
		if now.Sub(old.at) <= seenTTL && s.order.Len() < seenCap {
			break
		}
		s.order.Remove(e)
		delete(s.ids, old.id)
	}
	if e, ok := s.ids[id]; ok {
		if now.Sub(e.Value.(seenEntry).at) <= seenTTL {
			return false
		}
		s.order.Remove(e)
	}
	s.ids[id] = s.order.PushBack(seenEntry{id: id, at: now})
	return true
}

// errTurnStopped is the cause of a turn cancelled by its person's stop word: not a failure, no notice.
var errTurnStopped = errors.New("stopped by the person")

// turnRegistry keeps the cancel funcs of the replies in flight, by channel and person.
type turnRegistry struct {
	mu   sync.Mutex
	next uint64
	by   map[[2]string]map[uint64]context.CancelCauseFunc
}

// add registers a turn and returns the func that removes it once the turn is over.
func (r *turnRegistry) add(channelID, userID string, cancel context.CancelCauseFunc) func() {
	key := [2]string{channelID, userID}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.by == nil {
		r.by = map[[2]string]map[uint64]context.CancelCauseFunc{}
	}
	if r.by[key] == nil {
		r.by[key] = map[uint64]context.CancelCauseFunc{}
	}
	r.next++
	id := r.next
	r.by[key][id] = cancel
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.by[key], id)
		if len(r.by[key]) == 0 {
			delete(r.by, key)
		}
	}
}

// active reports whether a reply for this person in this channel is in flight.
func (r *turnRegistry) active(channelID, userID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.by[[2]string{channelID, userID}]) > 0
}

// stop cancels every reply in flight for this person in this channel and reports whether there was any.
func (r *turnRegistry) stop(channelID, userID string) bool {
	key := [2]string{channelID, userID}
	r.mu.Lock()
	cancels := r.by[key]
	delete(r.by, key)
	r.mu.Unlock()
	for _, c := range cancels {
		c(errTurnStopped)
	}
	return len(cancels) > 0
}

// withoutBotMention drops the bot's own mention, so "@마사몽 그만" is the stop word alone.
func withoutBotMention(content, botID string) string {
	if botID == "" {
		return content
	}
	return strings.NewReplacer("<@"+botID+">", " ", "<@!"+botID+">", " ").Replace(content)
}

// stopTurn handles a stop word: the reply in flight for this person here is cancelled and the message
// acknowledged with ✋. It reports whether anything was cancelled; if not, the message is ordinary talk.
func (g *Gateway) stopTurn(m *discordgo.MessageCreate) bool {
	if m == nil || m.Author == nil {
		return false
	}
	pending := g.merge.waiting(m)
	active := g.turns.active(m.ChannelID, m.Author.ID)
	if !pending && !active {
		return false
	}
	id := ""
	if p := g.botID.Load(); p != nil {
		id = *p
	}
	if !g.svc.TurnStopCue(m.GuildID, m.ChannelID, withoutBotMention(m.Content, id)) {
		return false
	}
	stoppedPending := g.merge.cancel(m)
	stoppedActive := g.turns.stop(m.ChannelID, m.Author.ID)
	if !stoppedPending && !stoppedActive {
		return false
	}
	g.work(func(ctx context.Context) {
		ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		react := g.reactForTest
		if react == nil {
			react = g.React
		}
		_ = react(ctx, m.ChannelID, m.ID, "✋")
	})
	return true
}
