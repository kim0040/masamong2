package discord

import "sync"

// changeHandlers counts the edit and delete handlers running now. Unlike work, a handler is let in while the
// gateway drains: Discord already delivered the change, so it must be applied or queued, never dropped, and
// Wait joins the handlers before the daemon closes the database and exits. A WaitGroup cannot count them: a
// handler may start while Wait is already waiting with the count at zero.
type changeHandlers struct {
	mu   sync.Mutex
	n    int
	idle chan struct{} // closed when n falls back to zero
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
