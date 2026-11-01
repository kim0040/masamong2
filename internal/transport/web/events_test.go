//go:build !noweb

package web

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/application"
)

// watcherCount is the live SSE subscriptions. The map is unexported on Service. Len is safe only
// while every handler that mutates it is parked before unsub, or after that handler has returned.
func watcherCount(svc *application.Service) int {
	v := reflect.ValueOf(svc).Elem().FieldByName("watchers")
	if !v.IsValid() || v.IsNil() {
		return 0
	}
	return v.Len()
}

// stallWriter blocks in Write until the handler's per-write deadline, then fails the way a socket
// does when the client has stopped reading. A zero deadline never unblocks on its own.
// observed lets the test read the subscription while Write is still parked: unsub runs only
// after Write returns, and sampling the map in that window races the delete.
type stallWriter struct {
	h        http.Header
	mu       sync.Mutex
	deadline time.Time
	entered  chan struct{}
	observed chan struct{}
	stop     chan struct{}
	once     sync.Once
}

func (w *stallWriter) Header() http.Header {
	if w.h == nil {
		w.h = http.Header{}
	}
	return w.h
}

func (w *stallWriter) WriteHeader(int) {}

func (w *stallWriter) Flush() {}

func (w *stallWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	w.deadline = t
	w.mu.Unlock()
	return nil
}

func (w *stallWriter) Write([]byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	// Stay inside Write until the test has sampled the subscription. The deadline
	// is applied after that, so a slow sample cannot overlap unsub.
	select {
	case <-w.observed:
	case <-w.stop:
		return 0, errors.New("stopped")
	}
	w.mu.Lock()
	dl := w.deadline
	w.mu.Unlock()
	if dl.IsZero() {
		<-w.stop
		return 0, errors.New("write had no deadline")
	}
	timer := time.NewTimer(time.Until(dl))
	defer timer.Stop()
	select {
	case <-timer.C:
		return 0, os.ErrDeadlineExceeded
	case <-w.stop:
		return 0, errors.New("stopped")
	}
}

// A client that never reads must not hold the handler or its subscription past the write deadline.
func TestEventStreamWriteDeadlineReleasesUnreadClient(t *testing.T) {
	s, _ := testServer(t)
	s.eventWrite = 300 * time.Millisecond
	id, _ := s.sessions.create(s.now(), "test")
	ctx, cancel := context.WithCancel(context.Background())
	stall := &stallWriter{entered: make(chan struct{}), observed: make(chan struct{}), stop: make(chan struct{})}
	t.Cleanup(func() {
		close(stall.stop)
		cancel()
	})
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/events", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
	done := make(chan struct{})
	go func() {
		s.events(stall, req)
		close(done)
	}()

	select {
	case <-stall.entered:
	case <-time.After(time.Second):
		t.Fatal("the handler never wrote")
	}
	if n := watcherCount(s.svc); n != 1 {
		t.Fatalf("subscription while writing: %d", n)
	}
	close(stall.observed)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a client that never reads kept the handler past the write deadline")
	}
	if n := watcherCount(s.svc); n != 0 {
		t.Fatalf("subscription leaked after the write failed: %d", n)
	}
}

type heldStream struct {
	cancel func()
	closed chan struct{}
}

func holdStream(t *testing.T, c *http.Client, url string) heldStream {
	t.Helper()
	cancel, br := openStream(t, c, url)
	closed := make(chan struct{})
	go func() {
		defer close(closed)
		_, _ = io.Copy(io.Discard, br)
	}()
	return heldStream{cancel: cancel, closed: closed}
}

func waitClosed(t *testing.T, h heldStream, d time.Duration) bool {
	t.Helper()
	select {
	case <-h.closed:
		return true
	case <-time.After(d):
		return false
	}
}

// Eight streams is the cap. A ninth from the same session drops that session's oldest stream, so a
// reloaded tab gets a live stream while the previous connection is still stuck. Another session's
// stream is left alone. A session that has none yet drops the global oldest instead of being refused.
func TestEventStreamCapKeepsAReloadedTab(t *testing.T) {
	s, ts := testServer(t)
	url := ts.URL + "/api/events"

	t.Run("same session", func(t *testing.T) {
		other := client()
		signIn(t, s, ts, other)
		mine := client()
		signIn(t, s, ts, mine)
		keep := holdStream(t, other, url)
		var mineStreams []heldStream
		for range maxEventStreams - 1 {
			mineStreams = append(mineStreams, holdStream(t, mine, url))
		}
		fresh := holdStream(t, mine, url)
		t.Cleanup(func() {
			keep.cancel()
			fresh.cancel()
			for _, h := range mineStreams {
				h.cancel()
			}
			<-keep.closed
			<-fresh.closed
			for _, h := range mineStreams {
				<-h.closed
			}
		})
		if !waitClosed(t, mineStreams[0], 3*time.Second) {
			t.Fatal("the reloaded session's oldest stream stayed open past the cap")
		}
		if waitClosed(t, keep, 200*time.Millisecond) {
			t.Fatal("another session's stream was closed to make room")
		}
		if n := s.openEventStreams(); n > maxEventStreams {
			t.Fatalf("open streams %d, cap %d", n, maxEventStreams)
		}
	})

	t.Run("new session", func(t *testing.T) {
		owner := client()
		signIn(t, s, ts, owner)
		var held []heldStream
		for range maxEventStreams {
			held = append(held, holdStream(t, owner, url))
		}
		extra := client()
		signIn(t, s, ts, extra)
		fresh := holdStream(t, extra, url)
		t.Cleanup(func() {
			fresh.cancel()
			for _, h := range held {
				h.cancel()
			}
			<-fresh.closed
			for _, h := range held {
				<-h.closed
			}
		})
		if !waitClosed(t, held[0], 3*time.Second) {
			t.Fatal("a new session at the cap did not close the oldest stream")
		}
		if n := s.openEventStreams(); n > maxEventStreams {
			t.Fatalf("open streams %d, cap %d", n, maxEventStreams)
		}
	})
}

// deadlineWriter blocks in Write until the write deadline is due. Cancelling the
// request does not unblock it: only SetWriteDeadline does, which is what eviction uses.
type deadlineWriter struct {
	h        http.Header
	mu       sync.Mutex
	deadline time.Time
	kick     chan struct{}
	entered  chan struct{}
	stop     chan struct{}
	once     sync.Once
}

func newDeadlineWriter() *deadlineWriter {
	return &deadlineWriter{
		kick:    make(chan struct{}, 1),
		entered: make(chan struct{}),
		stop:    make(chan struct{}),
	}
}

func (w *deadlineWriter) Header() http.Header {
	if w.h == nil {
		w.h = http.Header{}
	}
	return w.h
}

func (w *deadlineWriter) WriteHeader(int) {}

func (w *deadlineWriter) Flush() {}

func (w *deadlineWriter) SetWriteDeadline(t time.Time) error {
	w.mu.Lock()
	w.deadline = t
	w.mu.Unlock()
	select {
	case w.kick <- struct{}{}:
	default:
	}
	return nil
}

func (w *deadlineWriter) Write([]byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	for {
		w.mu.Lock()
		dl := w.deadline
		w.mu.Unlock()
		if !dl.IsZero() && !dl.After(time.Now()) {
			return 0, os.ErrDeadlineExceeded
		}
		wait := time.Hour
		if !dl.IsZero() {
			wait = time.Until(dl)
			if wait < 0 {
				wait = 0
			}
		}
		timer := time.NewTimer(wait)
		select {
		case <-w.kick:
			timer.Stop()
		case <-timer.C:
			// The deadline stored above has arrived.
		case <-w.stop:
			timer.Stop()
			return 0, errors.New("stopped")
		}
	}
}

// Evicting a stream stuck in Write must release it now, not after the long write timeout.
func TestEventStreamEvictUnblocksStuckWrite(t *testing.T) {
	s, _ := testServer(t)
	s.eventWrite = 30 * time.Second
	id, _ := s.sessions.create(s.now(), "test")
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stall := newDeadlineWriter()
	t.Cleanup(func() { close(stall.stop) })
	req := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/events", nil)
	req.AddCookie(&http.Cookie{Name: sessionCookie, Value: id})
	done := make(chan struct{})
	go func() {
		s.events(stall, req)
		close(done)
	}()
	select {
	case <-stall.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler never wrote")
	}
	// The stalled handler holds one slot. Filling the cap and opening one more of
	// the same session evicts it. The extra streams are not read by anyone.
	var cancels []context.CancelFunc
	t.Cleanup(func() {
		for _, c := range cancels {
			c()
		}
	})
	for range maxEventStreams {
		_, c := context.WithCancel(context.Background())
		cancels = append(cancels, c)
		s.beginEventStream(id, c, func() {})
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("evicting the stuck stream left its write blocked")
	}
}

// A short per-write deadline must not cut a client that keeps reading. The deadline
// is cleared between writes, and the ping interval is shorter than that deadline.
func TestEventStreamShortWriteDeadlineKeepsPings(t *testing.T) {
	s, ts := testServer(t)
	s.eventWrite = 100 * time.Millisecond
	s.ping = 30 * time.Millisecond
	c := client()
	signIn(t, s, ts, c)
	ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ts.URL+"/api/events", nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := c.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		t.Fatalf("events %d", res.StatusCode)
	}
	br := bufio.NewReader(res.Body)
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("no hello: %v", err)
		}
		if strings.HasPrefix(line, "event: hello") {
			break
		}
	}
	deadline := time.Now().Add(400 * time.Millisecond)
	pings := 0
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("stream ended after %d pings: %v", pings, err)
		}
		if strings.HasPrefix(line, ": ping") {
			pings++
		}
		if !time.Now().Before(deadline) {
			break
		}
	}
	if pings < 4 {
		t.Fatalf("only %d pings across the short write deadline", pings)
	}
}

// writeEvent must clear the deadline after the write. A deadline left in place
// expires during the idle gap before the next ping.
type deadlineRecord struct {
	h         http.Header
	deadlines []time.Time
}

func (w *deadlineRecord) Header() http.Header {
	if w.h == nil {
		w.h = http.Header{}
	}
	return w.h
}

func (w *deadlineRecord) WriteHeader(int) {}

func (w *deadlineRecord) Write(p []byte) (int, error) { return len(p), nil }

func (w *deadlineRecord) Flush() {}

func (w *deadlineRecord) SetWriteDeadline(t time.Time) error {
	w.deadlines = append(w.deadlines, t)
	return nil
}

func TestEventStreamWriteClearsDeadlineBetweenWrites(t *testing.T) {
	w := &deadlineRecord{}
	rc := http.NewResponseController(w)
	if err := writeEvent(rc, w, ": ping\n\n", 100*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if len(w.deadlines) != 2 || w.deadlines[0].IsZero() || !w.deadlines[1].IsZero() {
		t.Fatalf("deadlines = %v, want a write deadline then a clear", w.deadlines)
	}
}
