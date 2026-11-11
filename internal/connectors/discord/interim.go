package discord

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/chat"
	"masamong/internal/crash"
)

// The "still looking" line. A turn that searches long (a deep search, or a second round of searches, or an
// ordinary search that has run for a while) says so in one short reply to the member's message, and the
// line is deleted just before the answer is sent. It is never edited into the answer: the bot's own
// messages are captured, so an edited line would leave "still looking" in the stored conversation.
//
// For the same reason the line must never reach memory or the delete reconciliation. Its create event is
// recognised by (channel, exact text) registered before the send, and its delete event by its message ID
// (interimRegistry), so neither is captured, logged or queued, and a captured-then-deleted line cannot make
// the next turn wait on a pending source change.
const (
	// interimSlowDelay is how long an ordinary search turn runs before the line is posted (mode "slow").
	interimSlowDelay = 8 * time.Second
	// interimTimeout bounds the post and the delete, each on its own context: the turn's context is
	// cancelled by the stop word and expired by the timeout, and the line still has to go.
	interimTimeout = 3 * time.Second
	// interimIDTTL is how long the registry remembers a line, for its delete event to be recognised.
	interimIDTTL = 10 * time.Minute
	// the number of variants of each line in the catalog (discord.chat.interim.N / interim_more.N)
	interimVariants, interimMoreVariants = 3, 2
)

type interimState int

const (
	interimIdle interimState = iota
	interimPosting
	interimPosted
	interimCleared
)

// interim is one turn's "still looking" line. The zero value is not usable; newInterim makes one. All
// methods are safe on a nil receiver and from any goroutine: Progress (the chat goroutine), the slow-mode
// timer and Deliver/clear race by design.
type interim struct {
	g         *Gateway
	s         *discordgo.Session
	base      context.Context
	channelID string
	ref       *discordgo.MessageReference
	locale    string
	mode      string // off | deep | slow

	// afterFunc and pick are replaced in tests (a clock that can be advanced, a fixed variant).
	afterFunc func(d time.Duration, f func()) (stop func() bool)
	pick      func(n int) int

	posts sync.WaitGroup // posts started by show and not yet landed (tests wait on it)

	mu        sync.Mutex
	state     interimState
	msgID     string
	stopTimer func() bool
	landed    chan struct{} // closed when the post has landed and, if clear came meanwhile, been deleted
}

// newInterim prepares the line of the turn that answers m. base is the daemon run: the post and the delete
// get their own short contexts, so neither depends on the turn's own.
func (g *Gateway) newInterim(base context.Context, s *discordgo.Session, m *discordgo.MessageCreate) *interim {
	i := &interim{g: g, s: s, base: base, channelID: m.ChannelID, ref: m.Reference(), locale: g.noticeLocale(m),
		afterFunc: func(d time.Duration, f func()) func() bool { return time.AfterFunc(d, f).Stop },
		pick:      rand.IntN}
	if g.svc != nil {
		i.mode = g.svc.Doc().Chat.ProgressMode()
	}
	if g.interimHook != nil {
		g.interimHook(i)
	}
	return i
}

// show is the turn's OnProgress. It must not block: the post runs on the caller's goroutine only for deep
// stages, and is bounded by interimTimeout.
func (i *interim) show(p chat.Progress) {
	if i == nil || i.mode == "off" || i.mode == "" {
		return
	}
	defer crash.Recover(i.g.log, "discord interim")
	switch p.Stage {
	case chat.ProgressDeep:
		i.postAsync(interimVariants, "discord.chat.interim")
	case chat.ProgressMore:
		i.postAsync(interimMoreVariants, "discord.chat.interim_more")
	case chat.ProgressSearch:
		if i.mode == "slow" {
			i.startTimer()
		}
	}
}

// postAsync posts on its own goroutine: the chat goroutine that reported the stage goes on searching.
func (i *interim) postAsync(variants int, key string) {
	i.posts.Add(1)
	go func() {
		defer i.posts.Done()
		defer crash.Recover(i.g.log, "discord interim post")
		i.post(variants, key)
	}()
}

// startTimer posts the line if the turn is still unanswered after interimSlowDelay.
func (i *interim) startTimer() {
	i.mu.Lock()
	defer i.mu.Unlock()
	if i.state != interimIdle || i.stopTimer != nil {
		return
	}
	i.stopTimer = i.afterFunc(interimSlowDelay, func() {
		defer crash.Recover(i.g.log, "discord interim timer")
		i.post(interimMoreVariants, "discord.chat.interim_more")
	})
}

// post sends the line once. A line already posting, posted or cleared is not posted again.
func (i *interim) post(variants int, key string) {
	i.mu.Lock()
	if i.state != interimIdle {
		i.mu.Unlock()
		return
	}
	i.state = interimPosting
	i.landed = make(chan struct{})
	if i.stopTimer != nil {
		i.stopTimer()
		i.stopTimer = nil
	}
	landed := i.landed
	i.mu.Unlock()

	text := messages.T(i.locale, fmt.Sprintf("%s.%d", key, 1+i.pick(variants)))
	reg := &i.g.interims
	reg.expect(i.channelID, text) // before the send: the create event can arrive before the REST call returns
	ctx, cancel := context.WithTimeout(i.base, interimTimeout)
	sent, err := i.s.ChannelMessageSendComplex(i.channelID, &discordgo.MessageSend{Content: text, Reference: i.ref,
		AllowedMentions: noMentions, Flags: discordgo.MessageFlagsSuppressNotifications}, discordgo.WithContext(ctx))
	cancel()

	i.mu.Lock()
	if err != nil || sent == nil || sent.ID == "" {
		reg.unexpect(i.channelID, text)
		i.state = interimCleared // a line that did not go out is not tried again; the answer is unaffected
		close(landed)
		i.mu.Unlock()
		if err != nil {
			i.g.log.Debug("discord: interim line not sent", "err", err)
		}
		return
	}
	reg.bind(i.channelID, text, sent.ID)
	late := i.state == interimCleared // the answer came while the line was being sent
	if !late {
		i.state, i.msgID = interimPosted, sent.ID
	}
	i.mu.Unlock()
	if late {
		i.remove(sent.ID)
	}
	close(landed)
}

// clear deletes the line if it was posted, and keeps any later post from happening. It is idempotent, and
// waits for a post in flight so the line is gone before the answer goes out.
func (i *interim) clear() {
	if i == nil {
		return
	}
	i.mu.Lock()
	prev, id, landed := i.state, i.msgID, i.landed
	i.state = interimCleared
	if i.stopTimer != nil {
		i.stopTimer()
		i.stopTimer = nil
	}
	i.mu.Unlock()
	switch prev {
	case interimPosted:
		i.remove(id)
	case interimPosting:
		select { // the poster deletes its own line when it lands after this
		case <-landed:
		case <-time.After(2 * interimTimeout):
		}
	}
}

// remove deletes the line on a context of its own: the daemon stopping must not leave it behind.
func (i *interim) remove(id string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(i.base), interimTimeout)
	defer cancel()
	if err := i.s.ChannelMessageDelete(i.channelID, id, discordgo.WithContext(ctx)); err != nil {
		i.g.log.Debug("discord: interim line not deleted", "err", err)
	}
}

// interimRegistry tells the gateway's event handlers which bot messages are "still looking" lines.
// The zero value is ready.
type interimRegistry struct {
	mu      sync.Mutex
	pending map[string]interimPending // channel + text → lines sent and not yet seen
	ids     map[string]time.Time      // message ID → when it was bound
}

type interimPending struct {
	n  int
	at time.Time
}

func interimKey(channelID, text string) string { return channelID + "\x00" + text }

func (r *interimRegistry) sweepLocked(now time.Time) {
	for k, p := range r.pending {
		if now.Sub(p.at) > interimIDTTL {
			delete(r.pending, k)
		}
	}
	for id, at := range r.ids {
		if now.Sub(at) > interimIDTTL {
			delete(r.ids, id)
		}
	}
}

// expect registers a line about to be sent.
func (r *interimRegistry) expect(channelID, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.sweepLocked(now)
	if r.pending == nil {
		r.pending = map[string]interimPending{}
	}
	k := interimKey(channelID, text)
	p := r.pending[k]
	r.pending[k] = interimPending{n: p.n + 1, at: now}
}

// unexpect takes back an expectation whose send failed.
func (r *interimRegistry) unexpect(channelID, text string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.takeLocked(interimKey(channelID, text))
}

func (r *interimRegistry) takeLocked(k string) bool {
	p, ok := r.pending[k]
	if !ok || p.n <= 0 {
		return false
	}
	if p.n == 1 {
		delete(r.pending, k)
	} else {
		r.pending[k] = interimPending{n: p.n - 1, at: p.at}
	}
	return true
}

// bind records the message ID the send returned. If the create event came first it already took the
// expectation and the ID is known.
func (r *interimRegistry) bind(channelID, text, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, known := r.ids[id]; known {
		return
	}
	r.takeLocked(interimKey(channelID, text))
	r.addIDLocked(id)
}

func (r *interimRegistry) addIDLocked(id string) {
	if r.ids == nil {
		r.ids = map[string]time.Time{}
	}
	r.ids[id] = time.Now()
}

// ownCreate reports whether a bot message just created is a "still looking" line, and remembers its ID.
func (r *interimRegistry) ownCreate(channelID, id, text string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(time.Now())
	if _, ok := r.ids[id]; ok {
		return true
	}
	if r.takeLocked(interimKey(channelID, text)) {
		r.addIDLocked(id)
		return true
	}
	return false
}

// deleted reports whether a deleted message was a "still looking" line (and forgets it): such a delete is
// neither logged nor reconciled.
func (r *interimRegistry) deleted(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.ids[id]; !ok {
		return false
	}
	delete(r.ids, id)
	return true
}

// withoutInterims returns ids without the "still looking" lines (a bulk delete that took them along).
func (r *interimRegistry) withoutInterims(ids []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.ids) == 0 {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := r.ids[id]; ok {
			delete(r.ids, id)
			continue
		}
		out = append(out, id)
	}
	return out
}
