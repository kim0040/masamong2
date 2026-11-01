package discord

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/errclass"
)

const (
	typingInterval     = 8 * time.Second
	typingFailureLimit = 3
)

type typingSession interface {
	ChannelTyping(channelID string, options ...discordgo.RequestOption) error
}

// typingKeepalive owns the typing indicator for one reply. done seals it so delayed start calls cannot
// restart the ticker after delivery, cancellation, or a permanent REST failure. The lock guards state
// only; it is never held across the REST call.
type typingKeepalive struct {
	mu        sync.Mutex
	session   typingSession
	channelID string
	log       *slog.Logger
	interval  time.Duration
	doneCh    chan struct{}
	started   bool
	done      bool
	failures  int
	cancel    context.CancelFunc // ends the typing call in flight, if any
}

func newTypingKeepalive(session typingSession, channelID string, log *slog.Logger) *typingKeepalive {
	return newTypingKeepaliveInterval(session, channelID, log, typingInterval)
}

func newTypingKeepaliveInterval(session typingSession, channelID string, log *slog.Logger, interval time.Duration) *typingKeepalive {
	if log == nil {
		log = slog.Default()
	}
	if interval <= 0 {
		interval = typingInterval
	}
	return &typingKeepalive{session: session, channelID: channelID, log: log, interval: interval, doneCh: make(chan struct{})}
}

func (k *typingKeepalive) start(ctx context.Context) {
	k.mu.Lock()
	if k.done || k.started {
		k.mu.Unlock()
		return
	}
	k.started = true
	k.mu.Unlock()
	if k.send(ctx) {
		go k.run(ctx)
	}
}

func (k *typingKeepalive) run(ctx context.Context) {
	ticker := time.NewTicker(k.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			k.stop()
			return
		case <-k.doneCh:
			return
		case <-ticker.C:
			if !k.send(ctx) {
				return
			}
		}
	}
}

// send makes one typing call and reports whether the keepalive is still live. The REST call runs outside
// the lock, so stop never waits for it: state is checked under the lock before the call and again after
// it (a stop that arrived meanwhile wins and the result is dropped). stop also cancels the call in flight.
func (k *typingKeepalive) send(ctx context.Context) bool {
	k.mu.Lock()
	if k.done || ctx.Err() != nil {
		k.sealLocked()
		k.mu.Unlock()
		return false
	}
	callCtx, cancel := context.WithCancel(ctx)
	k.cancel = cancel
	k.mu.Unlock()
	err := k.session.ChannelTyping(k.channelID, discordgo.WithContext(callCtx))
	cancel()

	k.mu.Lock()
	defer k.mu.Unlock()
	k.cancel = nil
	if k.done {
		return false
	}
	if err == nil {
		k.failures = 0
		return true
	}
	if ctx.Err() != nil {
		k.sealLocked()
		return false
	}
	k.failures++
	class := classifyREST(err)
	k.log.Debug("discord: typing indicator failed", "consecutive_failures", k.failures, "class", string(class))
	if class == errclass.Permission || k.failures >= typingFailureLimit {
		k.sealLocked()
		return false
	}
	return true
}

func (k *typingKeepalive) stop() {
	k.mu.Lock()
	k.sealLocked()
	cancel := k.cancel
	k.mu.Unlock()
	if cancel != nil {
		cancel() // a call in flight ends now instead of landing after the reply
	}
}

func (k *typingKeepalive) sealLocked() {
	if k.done {
		return
	}
	k.done = true
	close(k.doneCh)
}
