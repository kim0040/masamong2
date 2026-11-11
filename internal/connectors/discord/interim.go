package discord

import (
	"context"
	"errors"
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
// the next turn wait on a pending source change. A deleted ID stays a tombstone for a short while, because
// the create or a second delete can arrive after the first delete. A send that returns no answer from
// Discord may still have been created: that expectation stays until a matching create arrives or it expires,
// and the create is deleted at once instead of being observed.
const (
	// interimSlowDelay is how long an ordinary search turn runs before the line is posted (mode "slow").
	interimSlowDelay = 12 * time.Second
	// interimPostTimeout bounds the post. Discord can accept the message and still answer late.
	interimPostTimeout = 6 * time.Second
	// interimTimeout bounds the delete, on its own context: the turn's context is cancelled by the stop
	// word and expired by the timeout, and the line still has to go.
	interimTimeout = 3 * time.Second
	// interimDeleteRetry is the pause before the one retry of a delete that did not succeed.
	interimDeleteRetry = time.Second
	// interimIDTTL is how long the registry remembers a live line, for its delete event to be recognised.
	interimIDTTL = 10 * time.Minute
	// interimTombstoneTTL is how long a deleted line's ID is remembered, so a create or another delete
	// that arrives after the first is still ours.
	interimTombstoneTTL = 2 * time.Minute
	// interimAmbiguousTTL is how long a (channel, text) expectation is kept after a send whose outcome
	// is unknown: Discord may have created the line.
	interimAmbiguousTTL = 2 * time.Minute
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
	// retryWait replaces the pause before a failed delete is tried again.
	afterFunc func(d time.Duration, f func()) (stop func() bool)
	pick      func(n int) int
	retryWait func(time.Duration)

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
// stages, and is bounded by interimPostTimeout.
func (i *interim) show(p chat.Progress) {
	if i == nil || i.mode == "off" || i.mode == "" {
		return
	}
	defer crash.Recover(i.g.log, "discord interim")
	switch p.Stage {
	case chat.ProgressDeep:
		i.postAsync(interimVariants, "discord.chat.interim")
	case chat.ProgressMore:
		// slow mode never uses the second-round lines: an ordinary search that ran long says the same
		// thing a deep search says at once.
		if i.mode == "slow" {
			i.postAsync(interimVariants, "discord.chat.interim")
			break
		}
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
		i.post(interimVariants, "discord.chat.interim")
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
	token := reg.expect(i.channelID, text) // before the send: the create event can arrive before the REST call returns
	ctx, cancel := context.WithTimeout(i.base, interimPostTimeout)
	sent, err := i.s.ChannelMessageSendComplex(i.channelID, &discordgo.MessageSend{Content: text, Reference: i.ref,
		AllowedMentions: noMentions, Flags: discordgo.MessageFlagsSuppressNotifications}, discordgo.WithContext(ctx))
	cancel()

	// Discord answered with an error, or nothing came back with an ID: the line was not created.
	// A timeout or a dropped connection is not that. The message may exist, so the expectation stays.
	if err != nil && !interimSendAmbiguous(err) || err == nil && (sent == nil || sent.ID == "") {
		reg.cancel(token)
		i.mu.Lock()
		i.state = interimCleared // a line that did not go out is not tried again; the answer is unaffected
		close(landed)
		i.mu.Unlock()
		if err != nil {
			i.g.log.Debug("discord: interim line not sent", "err", err)
		}
		return
	}
	if err != nil {
		if id := reg.lose(token); id != "" {
			i.remove(id) // the create arrived while the REST call was still out
		}
		i.mu.Lock()
		i.state = interimCleared
		close(landed)
		i.mu.Unlock()
		i.g.log.Debug("discord: interim line not sent", "err", err)
		return
	}
	if extra := reg.settle(token, sent.ID); extra != "" {
		i.remove(extra) // a create parked on this send belonged to a line whose REST answer was lost
	}
	i.mu.Lock()
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

// interimSendAmbiguous reports whether a failed post may still have created the message. Discord
// answering with an HTTP error means it did not; a timeout or a transport error leaves it unknown.
func interimSendAmbiguous(err error) bool {
	if err == nil {
		return false
	}
	var rest *discordgo.RESTError
	if errors.As(err, &rest) && rest.Response != nil {
		return false
	}
	return true
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
		select { // the poster deletes its own line when it lands after this, including one delete retry
		case <-landed:
		case <-time.After(interimPostTimeout + 2*interimTimeout + interimDeleteRetry + time.Second):
		}
	}
}

// remove deletes the line on a context of its own: the daemon stopping must not leave it behind.
// A delete that fails is tried once more, after a short pause.
func (i *interim) remove(id string) {
	deleteInterimMessage(i.g, i.base, i.s, i.channelID, id, i.retryWait)
}

// deleteInterimLine deletes a "still looking" line whose turn is already over (a send whose answer
// never came back, then the create event). It does not block the gateway's event loop.
func (g *Gateway) deleteInterimLine(s *discordgo.Session, channelID, id string) {
	go func() {
		defer crash.Recover(g.log, "discord interim drop")
		deleteInterimMessage(g, g.ctx, s, channelID, id, nil)
	}()
}

// deleteInterimMessage deletes one line and, on failure, tries once more after interimDeleteRetry.
// pause replaces the wait in tests.
func deleteInterimMessage(g *Gateway, base context.Context, s *discordgo.Session, channelID, id string, pause func(time.Duration)) {
	if g == nil || s == nil || channelID == "" || id == "" {
		return
	}
	if base == nil {
		base = context.Background()
	}
	once := func() error {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(base), interimTimeout)
		defer cancel()
		return s.ChannelMessageDelete(channelID, id, discordgo.WithContext(ctx))
	}
	err := once()
	if err == nil {
		return
	}
	if g.log != nil {
		g.log.Debug("discord: interim line not deleted", "err", err)
	}
	if pause != nil {
		pause(interimDeleteRetry)
	} else {
		time.Sleep(interimDeleteRetry)
	}
	if err := once(); err != nil && g.log != nil {
		g.log.Debug("discord: interim line not deleted", "err", err)
	}
}

// interimRegistry tells the gateway's event handlers which bot messages are "still looking" lines.
// The zero value is ready. Each expect is its own slot, so a send that fails can be told apart from
// another send of the same text that is still in flight.
type interimRegistry struct {
	mu         sync.Mutex
	next       uint64
	pending    map[string][]interimSlot // channel + text → sends not yet finished
	ids        map[string]time.Time     // live message ID → when it was bound
	tombstones map[string]time.Time     // deleted message ID → when it was deleted
}

// interimSlot is one line about to be sent, or sent with no REST answer yet.
type interimSlot struct {
	token     uint64
	at        time.Time
	id        string // set when the create event arrives before the REST call returns
	ambiguous bool   // the REST call failed without an answer; a later create of this slot is deleted
	restDone  bool
}

func interimKey(channelID, text string) string { return channelID + "\x00" + text }

func (r *interimRegistry) sweepLocked(now time.Time) {
	for k, slots := range r.pending {
		kept := make([]interimSlot, 0, len(slots))
		for _, s := range slots {
			ttl := interimIDTTL
			if s.ambiguous {
				ttl = interimAmbiguousTTL
			}
			if now.Sub(s.at) <= ttl {
				kept = append(kept, s)
			}
		}
		if len(kept) == 0 {
			delete(r.pending, k)
		} else {
			r.pending[k] = kept
		}
	}
	for id, at := range r.ids {
		if now.Sub(at) > interimIDTTL {
			delete(r.ids, id)
		}
	}
	for id, at := range r.tombstones {
		if now.Sub(at) > interimTombstoneTTL {
			delete(r.tombstones, id)
		}
	}
}

// expect registers a line about to be sent and returns the token that settles it.
func (r *interimRegistry) expect(channelID, text string) uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.sweepLocked(now)
	if r.pending == nil {
		r.pending = map[string][]interimSlot{}
	}
	r.next++
	k := interimKey(channelID, text)
	r.pending[k] = append(r.pending[k], interimSlot{token: r.next, at: now})
	return r.next
}

// cancel drops an expectation whose send Discord refused: the line was not created.
func (r *interimRegistry) cancel(token uint64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k, idx, ok := r.slotLocked(token)
	if !ok {
		return
	}
	r.removeSlotLocked(k, idx)
}

// lose records a send that returned no answer. If the create already arrived, its ID is returned so
// the caller can delete it. Otherwise the expectation stays for interimAmbiguousTTL and a later create
// of it is dropped.
func (r *interimRegistry) lose(token uint64) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	k, idx, ok := r.slotLocked(token)
	if !ok {
		return ""
	}
	slot := r.pending[k][idx]
	if slot.id != "" {
		r.removeSlotLocked(k, idx)
		return slot.id
	}
	slot.ambiguous = true
	slot.restDone = true
	slot.at = time.Now()
	r.pending[k][idx] = slot
	return ""
}

// settle records the message ID a successful send returned. A create that was parked on this slot but
// is a different message (another line of the same text) is returned so the caller can delete it.
func (r *interimRegistry) settle(token uint64, id string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var drop string
	k, idx, ok := r.slotLocked(token)
	if ok {
		slot := r.pending[k][idx]
		if slot.id != "" && slot.id != id {
			drop = slot.id
		}
		r.removeSlotLocked(k, idx)
	}
	if _, tomb := r.tombstones[id]; !tomb {
		r.addIDLocked(id)
	}
	return drop
}

// bind records the message ID the send returned. If the create event came first it already took the
// expectation and the ID is known. Tests and a caller without the expect token use it; post uses settle.
func (r *interimRegistry) bind(channelID, text, id string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k := interimKey(channelID, text)
	if _, known := r.ids[id]; known {
		r.removeSlotIDLocked(k, id)
		return
	}
	if _, tomb := r.tombstones[id]; tomb {
		r.removeSlotIDLocked(k, id)
		return
	}
	if idx := r.oldestWaitingLocked(k); idx >= 0 {
		slot := r.pending[k][idx]
		r.removeSlotLocked(k, idx)
		if slot.id != "" && slot.id != id {
			r.addIDLocked(slot.id)
		}
	}
	r.addIDLocked(id)
}

func (r *interimRegistry) addIDLocked(id string) {
	if id == "" {
		return
	}
	if r.ids == nil {
		r.ids = map[string]time.Time{}
	}
	r.ids[id] = time.Now()
}

func (r *interimRegistry) tombstoneLocked(id string, now time.Time) {
	if id == "" {
		return
	}
	delete(r.ids, id)
	if r.tombstones == nil {
		r.tombstones = map[string]time.Time{}
	}
	if _, ok := r.tombstones[id]; !ok {
		r.tombstones[id] = now
	}
}

func (r *interimRegistry) slotLocked(token uint64) (string, int, bool) {
	for k, slots := range r.pending {
		for i := range slots {
			if slots[i].token == token {
				return k, i, true
			}
		}
	}
	return "", 0, false
}

func (r *interimRegistry) oldestWaitingLocked(k string) int {
	for i := range r.pending[k] {
		if !r.pending[k][i].restDone {
			return i
		}
	}
	return -1
}

func (r *interimRegistry) removeSlotLocked(k string, idx int) {
	slots := r.pending[k]
	slots = append(slots[:idx], slots[idx+1:]...)
	if len(slots) == 0 {
		delete(r.pending, k)
		return
	}
	r.pending[k] = slots
}

func (r *interimRegistry) removeSlotIDLocked(k, id string) {
	for i := range r.pending[k] {
		if r.pending[k][i].id == id {
			r.removeSlotLocked(k, i)
			return
		}
	}
}

// ownCreate reports whether a bot message just created is a "still looking" line, and whether it
// should be deleted now. drop is set when the send already failed without an answer, or would be,
// so the line must not stay in the channel and must not be observed.
func (r *interimRegistry) ownCreate(channelID, id, text string) (ours, drop bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sweepLocked(time.Now())
	if _, ok := r.tombstones[id]; ok {
		return true, false // already deleted; the create is late and is not deleted again
	}
	if _, ok := r.ids[id]; ok {
		return true, false
	}
	k := interimKey(channelID, text)
	for i := range r.pending[k] {
		slot := r.pending[k][i]
		if slot.id != "" {
			continue
		}
		if !slot.restDone {
			slot.id = id
			r.pending[k][i] = slot
			r.addIDLocked(id)
			return true, false
		}
		if slot.ambiguous {
			r.removeSlotLocked(k, i)
			r.addIDLocked(id)
			return true, true
		}
	}
	return false, false
}

// deleted reports whether a deleted message was a "still looking" line: such a delete is neither
// logged nor reconciled. The ID stays a tombstone, so a duplicate delete or a create that arrives
// afterwards is still ours.
func (r *interimRegistry) deleted(id string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.sweepLocked(now)
	if _, ok := r.ids[id]; ok {
		r.tombstoneLocked(id, now)
		return true
	}
	if _, ok := r.tombstones[id]; ok {
		return true
	}
	return false
}

// withoutInterims returns ids without the "still looking" lines (a bulk delete that took them along),
// including lines already deleted whose ID is still a tombstone.
func (r *interimRegistry) withoutInterims(ids []string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	r.sweepLocked(now)
	if len(r.ids) == 0 && len(r.tombstones) == 0 {
		return ids
	}
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if _, ok := r.ids[id]; ok {
			r.tombstoneLocked(id, now)
			continue
		}
		if _, ok := r.tombstones[id]; ok {
			continue
		}
		out = append(out, id)
	}
	return out
}
