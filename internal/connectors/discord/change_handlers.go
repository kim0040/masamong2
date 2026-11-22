package discord

import (
	"sync"

	"github.com/bwmarrin/discordgo"
)

// registerHandlers adds the gateway's event handlers to the session. DiscordGo calls each one on its event
// loop (SyncEvents), and each starts its handler in a goroutine of its own, as DiscordGo did itself. An edit
// or delete is counted there, before its goroutine is scheduled: Wait cannot find the count at zero while a
// change Discord delivered has not reached its handler yet.
func (g *Gateway) registerHandlers(sess *discordgo.Session) {
	sess.SyncEvents = true // none of the funcs below blocks the event loop: each only starts a goroutine
	sess.AddHandler(spawn(g.onGatewayEvent))
	sess.AddHandler(spawn(g.onReady))
	sess.AddHandler(spawn(g.onDisconnect))
	sess.AddHandler(spawn(g.onResumed))
	sess.AddHandler(spawn(g.onInteraction))
	sess.AddHandler(spawn(g.onMessage))
	sess.AddHandler(counted(&g.changes, g.onMessageUpdate))
	sess.AddHandler(counted(&g.changes, g.onMessageDelete))
	sess.AddHandler(counted(&g.changes, g.onMessageDeleteBulk))
	sess.AddHandler(spawn(g.onThreadCreate))
	sess.AddHandler(spawn(g.onThreadUpdate))
	sess.AddHandler(spawn(g.onThreadDelete))
	sess.AddHandler(spawn(g.onPollVoteAdd))
	sess.AddHandler(spawn(g.onPollVoteRemove))
	sess.AddHandler(spawn(g.onReactionAdd))
	sess.AddHandler(spawn(g.onReactionRemove))
}

// spawn is what DiscordGo calls for an event: it runs the handler in a goroutine of its own.
func spawn[E any](handler func(*discordgo.Session, E)) func(*discordgo.Session, E) {
	return func(s *discordgo.Session, ev E) { go handler(s, ev) }
}

// counted is spawn for an edit or delete handler: the change is counted in before the goroutine starts and
// counted out when the handler returns.
func counted[E any](c *changeHandlers, handler func(*discordgo.Session, E)) func(*discordgo.Session, E) {
	return func(s *discordgo.Session, ev E) {
		leave := c.enter()
		go func() {
			defer leave()
			handler(s, ev)
		}()
	}
}

// changeHandlers counts the edit and delete handlers running now. Unlike work, a handler is let in while the
// gateway drains: Discord already delivered the change, so it must be applied or queued, never dropped, and
// Wait joins the handlers before the daemon closes the database and exits. A WaitGroup cannot count them: a
// handler may start while Wait is already waiting with the count at zero.
type changeHandlers struct {
	mu      sync.Mutex
	n       int
	idle    chan struct{} // closed when n falls back to zero
	stopped sync.Once     // stopDelivery
}

// enter counts a handler in; the returned func counts it out.
func (c *changeHandlers) enter() func() {
	c.mu.Lock()
	if c.n == 0 {
		c.idle = make(chan struct{})
	}
	c.n++
	c.mu.Unlock()
	return func() {
		c.mu.Lock()
		defer c.mu.Unlock()
		if c.n--; c.n == 0 {
			close(c.idle)
		}
	}
}

// done is closed once no handler runs (already closed when none does).
func (c *changeHandlers) done() <-chan struct{} {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.n == 0 {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return c.idle
}

// stopDelivery closes the session once (Wait and shutdown both call it), so DiscordGo hands out no event
// after it. An event it was still handing out has been counted when stopDelivery returns: DiscordGo holds
// its handler lock while it calls the handlers, and adding a handler waits for that lock.
func (c *changeHandlers) stopDelivery(sess *discordgo.Session) {
	c.stopped.Do(func() {
		if sess == nil {
			return
		}
		_ = sess.Close()
		sess.AddHandler(func(*discordgo.Session, *discordgo.Connect) {})() // added and removed: only the wait matters
	})
}
