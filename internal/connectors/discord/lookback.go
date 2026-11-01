package discord

import (
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/vision"
)

// People often send a picture and the words about it as two messages. The picture comes first ("photo",
// then "@마사몽 이거 뭐야"), or the words come first and the picture a few seconds later, after the
// merger already answered. recentPictures remembers each channel's last few human messages for a short
// while so that:
//   - a call that came without a picture takes the picture posted just before it: the caller's own (other
//     people's chatter in between does not matter, the caller's own words do), or anyone's picture that is
//     the very message above the call (in Discord's order, by message ID);
//   - a picture-only message from someone whose call a moment ago had no picture is the rest of that call:
//     it is answered with that call's words. Anything else the person says in the channel first, or the
//     call's deletion, drops the call.
//
// Each picture message serves at most one turn, including a turn that holds it directly. Only attachment
// references and the pending call's words are kept, in memory, for pictureLookback / pictureFollow; a
// deleted message leaves at once and an edit replaces its pictures.
const (
	pictureLookback  = 2 * time.Minute
	pictureFollow    = 45 * time.Second
	recentPerChannel = 12
	recentChannelCap = 512
	pendingCallCap   = 4096
)

type recentLine struct {
	id, author string
	at         time.Time
	pictures   []*discordgo.MessageAttachment
	used       bool
}

// pendingCall is a call answered without a picture, waiting pictureFollow for one.
type pendingCall struct {
	at   time.Time
	text string   // the call's words, said again with the picture
	ids  []string // the call's messages: deleting one drops the call
}

type recentPictures struct {
	mu      sync.Mutex
	lines   map[string][]recentLine // channel → its last human messages, in message-ID order
	calls   map[string]pendingCall  // channel/person
	sweptAt time.Time
}

func imageAttachments(atts []*discordgo.MessageAttachment) []*discordgo.MessageAttachment {
	var pics []*discordgo.MessageAttachment
	for _, a := range atts {
		if a != nil && a.URL != "" && vision.ImageType(a.ContentType, a.Filename) {
			pics = append(pics, a)
		}
	}
	return pics
}

// snowflakeLess orders Discord IDs (decimal, so a shorter one is older).
func snowflakeLess(a, b string) bool {
	if len(a) != len(b) {
		return len(a) < len(b)
	}
	return a < b
}

func (r *recentPictures) initLocked() {
	if r.lines == nil {
		r.lines = map[string][]recentLine{}
		r.calls = map[string]pendingCall{}
	}
}

// note records a person's message (bots are not recorded). A message other than a picture alone drops
// the person's pending call in that channel: they moved on.
func (r *recentPictures) note(m *discordgo.MessageCreate, now time.Time) {
	if m == nil || m.Message == nil || m.Author == nil || m.Author.Bot || m.ID == "" {
		return
	}
	pics := imageAttachments(m.Attachments)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()
	r.sweepLocked(now)
	if !pictureOnly(m.Message) {
		delete(r.calls, m.ChannelID+"/"+m.Author.ID)
	}
	ls := r.lines[m.ChannelID]
	i := len(ls)
	for i > 0 && snowflakeLess(m.ID, ls[i-1].id) {
		i--
	}
	if i > 0 && ls[i-1].id == m.ID {
		return // a replay
	}
	ls = append(ls, recentLine{})
	copy(ls[i+1:], ls[i:])
	ls[i] = recentLine{id: m.ID, author: m.Author.ID, at: now, pictures: pics}
	if len(ls) > recentPerChannel {
		ls = append(ls[:0:0], ls[len(ls)-recentPerChannel:]...)
	}
	r.lines[m.ChannelID] = ls
}

// sweepLocked forgets, at most every pictureFollow, the lines older than the lookback and the calls older
// than pictureFollow; past the caps everything goes rather than growing.
func (r *recentPictures) sweepLocked(now time.Time) {
	if now.Sub(r.sweptAt) < pictureFollow && len(r.lines) < recentChannelCap && len(r.calls) < pendingCallCap {
		return
	}
	r.sweptAt = now
	for ch, ls := range r.lines {
		kept := ls[:0]
		for _, l := range ls {
			if now.Sub(l.at) <= pictureLookback {
				kept = append(kept, l)
			}
		}
		if len(kept) == 0 {
			delete(r.lines, ch)
		} else {
			r.lines[ch] = kept
		}
	}
	for k, c := range r.calls {
		if now.Sub(c.at) > pictureFollow {
			delete(r.calls, k)
		}
	}
	if len(r.lines) >= recentChannelCap {
		r.lines = map[string][]recentLine{}
	}
	if len(r.calls) >= pendingCallCap {
		r.calls = map[string]pendingCall{}
	}
}

// earlier finds the picture posted just before the call whose first message is firstID, by caller, and
// marks it used. total counts its supported pictures, refs at most vision.MaxImages of them. Nothing is
// found when the call is not among the remembered lines.
func (r *recentPictures) earlier(channelID, firstID, caller string, now time.Time) (refs []vision.Ref, total int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ls := r.lines[channelID]
	at := -1
	for i := range ls {
		if ls[i].id == firstID {
			at = i
			break
		}
	}
	for j := at - 1; j >= 0; j-- {
		l := &ls[j]
		if now.Sub(l.at) > pictureLookback {
			break
		}
		mine := l.author == caller
		if len(l.pictures) > 0 && !l.used && (mine || j == at-1) {
			l.used = true
			for _, a := range l.pictures {
				if len(refs) < vision.MaxImages {
					refs = append(refs, vision.Ref{URL: a.URL, ContentType: a.ContentType, Filename: a.Filename, Size: a.Size, MessageID: l.id, AuthorID: l.author})
				}
			}
			return refs, len(l.pictures)
		}
		if mine {
			break // the caller said something else after their picture
		}
	}
	return nil, 0
}

// used marks messages whose pictures a turn holds or took.
func (r *recentPictures) used(channelID string, ids []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	ls := r.lines[channelID]
	for i := range ls {
		for _, id := range ids {
			if ls[i].id == id {
				ls[i].used = true
			}
		}
	}
}

// edited replaces a message's pictures with the ones it has after an edit.
func (r *recentPictures) edited(channelID, id string, atts []*discordgo.MessageAttachment) {
	pics := imageAttachments(atts)
	r.mu.Lock()
	defer r.mu.Unlock()
	ls := r.lines[channelID]
	for i := range ls {
		if ls[i].id == id {
			ls[i].pictures = pics
		}
	}
}

// forget drops deleted messages, and any pending call one of them belonged to.
func (r *recentPictures) forget(channelID string, ids []string) {
	gone := make(map[string]bool, len(ids))
	for _, id := range ids {
		gone[id] = true
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if ls, ok := r.lines[channelID]; ok {
		kept := ls[:0]
		for _, l := range ls {
			if !gone[l.id] {
				kept = append(kept, l)
			}
		}
		r.lines[channelID] = kept
	}
	for k, c := range r.calls {
		for _, id := range c.ids {
			if gone[id] {
				delete(r.calls, k)
				break
			}
		}
	}
}

// called notes a call answered now without a picture: its words and messages.
func (r *recentPictures) called(channelID, person, text string, ids []string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.initLocked()
	r.sweepLocked(now)
	r.calls[channelID+"/"+person] = pendingCall{at: now, text: text, ids: append([]string(nil), ids...)}
}

// follows returns, once, the person's call a picture-only message continues: one answered at most
// pictureFollow ago without a picture.
func (r *recentPictures) follows(channelID, person string, now time.Time) (pendingCall, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := channelID + "/" + person
	c, ok := r.calls[k]
	if !ok {
		return pendingCall{}, false
	}
	delete(r.calls, k)
	return c, now.Sub(c.at) >= 0 && now.Sub(c.at) <= pictureFollow
}
