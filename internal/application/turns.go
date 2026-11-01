package application

import (
	"context"
	"errors"
	"sync"
)

// ErrChatBusy is returned when a channel already has as many turns waiting as it may queue. The
// Discord connector answers it with a short notice, but only to someone who spoke to the bot.
var ErrChatBusy = errors.New("chat: too many replies waiting in this channel")

// maxQueuedPerChannel is how many turns may wait behind the one running in the same channel.
const maxQueuedPerChannel = 4

// turnGate lets chat turns run in parallel across channels while keeping each channel in order.
//
// A turn first takes its channel's lane, so two messages in one room are answered one after the
// other and the second sees the first exchange in its history; then it takes one of the global slots
// (limits.max_concurrent_turns), which bounds simultaneous model calls however many channels are busy.
// Waiting respects the turn's context, and a lane refuses new turns once its backlog is full.
type turnGate struct {
	mu    sync.Mutex
	limit int
	slots chan struct{}
	lanes map[string]*turnLane
}

type turnLane struct {
	token chan struct{} // capacity 1: held by the running turn
	users int           // the running turn plus the ones waiting
}

// slotsFor returns the slot pool for limit. A changed limit starts a new pool; turns already running
// release into the pool they took from.
func (g *turnGate) slotsFor(limit int) chan struct{} {
	if g.slots == nil || g.limit != limit {
		g.limit, g.slots = limit, make(chan struct{}, limit)
	}
	return g.slots
}

// enter waits for the channel lane and a global slot. The returned func releases both.
func (g *turnGate) enter(ctx context.Context, key string, limit int) (func(), error) {
	g.mu.Lock()
	slots := g.slotsFor(limit)
	if g.lanes == nil {
		g.lanes = map[string]*turnLane{}
	}
	l := g.lanes[key]
	if l == nil {
		l = &turnLane{token: make(chan struct{}, 1)}
		g.lanes[key] = l
	}
	if l.users > maxQueuedPerChannel {
		g.mu.Unlock()
		return nil, ErrChatBusy
	}
	l.users++
	g.mu.Unlock()
	leave := func() {
		g.mu.Lock()
		if l.users--; l.users == 0 {
			delete(g.lanes, key)
		}
		g.mu.Unlock()
	}
	select {
	case l.token <- struct{}{}:
	case <-ctx.Done():
		leave()
		return nil, ctx.Err()
	}
	select {
	case slots <- struct{}{}:
	case <-ctx.Done():
		<-l.token
		leave()
		return nil, ctx.Err()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			<-slots
			<-l.token
			leave()
		})
	}, nil
}
