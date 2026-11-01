package application

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"masamong/internal/crash"
	"masamong/internal/hostprofile"
	"masamong/internal/memory/engine"
)

// pendingRecount is how old the last successful count of messages waiting for a vector may get before it is
// counted again, even when nothing visible changed. It is checked when the console lists or the doctor runs.
const pendingRecount = 10 * time.Minute

// pendingRecountGap is the least time between the end of one count and the start of the next, also after
// a failed one. While people talk, the live message count (part of the key) changes every few seconds, an
// open console lists again on every memory event, and anyone may run the doctor (Discord /status); without
// a gap one 5-second count would follow another and keep a small machine's only core busy. A changed space
// or recipe (an operator's config change) is counted without waiting for it.
const pendingRecountGap = time.Minute

var errPendingUncounted = errors.New("pending messages not counted for this space yet")

// pendingMessageCount keeps the last count of messages waiting for a message vector. Counting walks every
// message of every closed leaf and reads its text (short messages never get a vector, so they always come
// back and must be checked again): about 5 s at 100,000 messages on one core, more under load. The console
// and the doctor share it: one count runs at a time, always in the background under the service's
// lifetime, and whether to start one is decided in one critical section (admit). The first console
// request for a space and the doctor wait for a count with their own request context; later console
// requests get the last count at once while a stale one is redone.
type pendingMessageCount struct {
	mu      sync.Mutex
	ident   string // space|recipe of the stored count
	key     string // ident plus what makes it stale (see pendingKey)
	n       int
	known   bool
	counted time.Time   // when the stored count ended (pendingRecount runs from here)
	ended   time.Time   // when the last count, stored or failed, ended (the gap runs from here)
	run     *pendingRun // the running count; nil while none runs
	waiting *time.Timer // the reminder armed for a stale count within the gap
	closed  bool
	gap     time.Duration // tests; 0 uses the host profile, then pendingRecountGap
}

// pendingRun is one count; its result fields are written once, before done is closed.
type pendingRun struct {
	done  chan struct{}
	ident string
	n     int
	err   error
}

func pendingIdent(eng *engine.Engine, space string) string {
	return space + "|" + eng.Settings.Recipe.Version()
}

// pendingKey changes when the count may have: another space or recipe, a reloaded index (new leaves),
// another number of stored vectors (saved, or removed by an edit or a delete) or of live messages (a
// waiting message deleted). live < 0 reads it (a covering-index scan, about 25 ms at 100,000 messages).
// It is taken before a count starts, so a change made while counting makes the next request count again.
func pendingKey(ctx context.Context, eng *engine.Engine, space string, stored, live int) string {
	if live < 0 {
		_ = eng.Store.DB.Reader().QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_observations WHERE deleted_at=''`).Scan(&live)
	}
	return fmt.Sprintf("%s|%d|%d|%d", pendingIdent(eng, space), eng.LoadedAt().UnixNano(), stored, live)
}

// admit decides under c.mu, for a request about ident, whether to start a count (started; the caller
// must then call startPendingCount) and which count, if any, the request waits for. A count starts when
// none runs and the stored one is missing, for another ident, or stale with the gap passed. The doctor
// (wait) waits for a count it started or one that runs; the console waits only while nothing for its
// ident is stored. A stale count within the gap arms one reminder that asks an open console to list
// again when the gap ends.
func (s *Service) admit(ident, key string, wait bool) (run *pendingRun, started bool) {
	c := &s.pendingMsgs
	c.mu.Lock()
	defer c.mu.Unlock()
	have := c.known && c.ident == ident
	if c.run != nil {
		if wait || !have {
			return c.run, false
		}
		return nil, false
	}
	gap := c.gap
	if gap == 0 {
		// WHY: a test sets c.gap to pin the wait. Zero still means the process default,
		// which is now the host profile (one minute on medium, the value this constant
		// has always been) so a large server can recount without waiting a full minute.
		gap = hostprofile.Current().PendingRecountGap
		if gap <= 0 {
			gap = pendingRecountGap
		}
	}
	left := gap - time.Since(c.ended)
	if c.ended.IsZero() || (c.known && c.ident != ident) {
		left = 0
	}
	stale := !have || c.key != key || time.Since(c.counted) > pendingRecount
	switch {
	case !stale || c.closed:
		return nil, false
	case left <= 0:
		c.run = &pendingRun{done: make(chan struct{}), ident: ident}
		return c.run, true
	case have && c.waiting == nil:
		c.waiting = time.AfterFunc(left, func() {
			c.mu.Lock()
			c.waiting = nil
			c.mu.Unlock()
			if _, done, ok := s.beginWork(nil); ok { // not after the service closed
				defer done()
				s.emit("memory", "pending_stale", nil) // an open console lists again, which counts
			}
		})
	}
	return nil, false
}

// startPendingCount runs the count admit registered in the background, under the service's lifetime:
// a request that gives up waiting does not cancel the count others wait for. The run always ends, also
// when the count panics or the service is closing.
func (s *Service) startPendingCount(eng *engine.Engine, space, key string, run *pendingRun) {
	ctx, done, ok := s.beginWork(nil)
	if !ok {
		s.finishPendingCount(key, run, 0, errors.New("service closing"))
		return
	}
	go func() {
		defer done()
		finished := false
		defer func() {
			if !finished {
				s.finishPendingCount(key, run, 0, errors.New("pending message count panicked"))
			}
		}()
		defer crash.Recover(s.Log, "pending message count")
		n, err := pendingCountHook(ctx, eng, space)
		s.finishPendingCount(key, run, n, err)
		finished = true
	}()
}

// pendingCountHook is the count itself; tests replace it.
var pendingCountHook = func(ctx context.Context, eng *engine.Engine, space string) (int, error) {
	return eng.Store.CountPendingMessagesContext(ctx, nil, space, eng.Settings.Recipe.Version())
}

// finishPendingCount ends run with its result and stores a success. A changed number is announced so an
// open console lists again.
func (s *Service) finishPendingCount(key string, run *pendingRun, n int, err error) {
	c := &s.pendingMsgs
	c.mu.Lock()
	run.n, run.err = n, err
	close(run.done)
	if c.run == run {
		c.run = nil
	}
	now := time.Now()
	c.ended = now
	changed := false
	if err == nil {
		changed = !c.known || c.ident != run.ident || c.n != n
		c.ident, c.key, c.n, c.known, c.counted = run.ident, key, n, true, now
	}
	c.mu.Unlock()
	if changed {
		s.emit("memory", "pending_counted", nil)
	}
}

// pendingFor returns, for ident, the result of run once it ends (ctx is the request's) or, with no run,
// the stored count.
func (s *Service) pendingFor(ctx context.Context, ident string, run *pendingRun) (int, error) {
	if run != nil {
		select {
		case <-run.done:
		case <-ctx.Done():
			return 0, ctx.Err()
		}
		if run.err != nil {
			return 0, run.err
		}
		if run.ident != ident {
			return 0, errPendingUncounted // it counted another space; the next request counts this one
		}
		return run.n, nil
	}
	c := &s.pendingMsgs
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.known || c.ident != ident {
		return 0, errPendingUncounted
	}
	return c.n, nil
}

// pendingMessages returns the console's count of messages waiting for a vector; ok is false when there is
// none to show for this space yet (never counted, the count failed, or the request ended). live is the
// live message count the caller already read, or -1.
func (s *Service) pendingMessages(ctx context.Context, eng *engine.Engine, space string, stored, live int) (int, bool) {
	ident := pendingIdent(eng, space)
	key := pendingKey(ctx, eng, space, stored, live)
	run, started := s.admit(ident, key, false)
	if started {
		s.startPendingCount(eng, space, key, run)
		if s.pendingKnown(ident) {
			run = nil // a recount: show the last count meanwhile
		}
	}
	n, err := s.pendingFor(ctx, ident, run)
	return n, err == nil
}

// countPendingNow is the doctor's count: the stored one while it is fresh (or stale within the gap),
// otherwise a new one (or the one already running), which it waits for and stores for the console too.
// pendingFirstWait is how long the doctor waits for a space's first count.
const pendingFirstWait = 3 * time.Second

// errPendingCounting: the first count is still running after pendingFirstWait.
var errPendingCounting = errors.New("pending message count still running")

func (s *Service) countPendingNow(ctx context.Context, eng *engine.Engine, space string, stored int) (int, error) {
	ident := pendingIdent(eng, space)
	if s.pendingKnown(ident) {
		// a count is already known: report it and let a stale one be recounted in the background. A
		// recount over half a million messages takes 10-20 seconds, and the dashboard and the status
		// page wait for the doctor, so the doctor waits only for the first count of a space
		key := pendingKey(ctx, eng, space, stored, -1)
		if run, started := s.admit(ident, key, false); started {
			s.startPendingCount(eng, space, key, run)
		}
		if n, err := s.pendingFor(ctx, ident, nil); !errors.Is(err, errPendingUncounted) {
			return n, err
		}
		// the stored count went away meanwhile (another space was counted): wait below like a first count
	}
	// a first count waits a few seconds at most; a longer one finishes in the background and the
	// doctor says it is counting (errPendingCounting) instead of holding the status page
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, pendingFirstWait)
		defer cancel()
	}
	for try := 0; ; try++ {
		key := pendingKey(ctx, eng, space, stored, -1)
		run, started := s.admit(ident, key, true)
		if started {
			s.startPendingCount(eng, space, key, run)
		}
		n, err := s.pendingFor(ctx, ident, run)
		if errors.Is(err, context.DeadlineExceeded) {
			return 0, errPendingCounting
		}
		if !errors.Is(err, errPendingUncounted) || run == nil || try > 0 {
			return n, err
		}
		// it waited for a count of another space (the config changed meanwhile): count this one, once
	}
}

func (s *Service) pendingKnown(ident string) bool {
	c := &s.pendingMsgs
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.known && c.ident == ident
}

// close stops the reminder and admits no more counts; Close calls it.
func (c *pendingMessageCount) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.waiting != nil {
		c.waiting.Stop()
		c.waiting = nil
	}
}
