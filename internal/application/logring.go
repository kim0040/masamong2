package application

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/observability"
)

// The daemon's structured log is teed into a bounded in-memory ring so the console can follow it
// live and search recent entries without reading log files. Attribute values under sensitive keys
// are replaced before they enter the ring, and the message and every other value go through the same
// observability.Redact as the file log (the console must not show what the file log masks).

const logRingSize = 3000

type logEntry struct {
	ID    int64             `json:"id"`
	At    string            `json:"at"`
	Level string            `json:"level"`
	Msg   string            `json:"msg"`
	Attrs map[string]string `json:"attrs,omitempty"`
}

// logRing is the console's live log of one service (Service.logs); Close empties and closes it.
type logRing struct {
	mu      sync.Mutex
	entries []logEntry
	next    int64
	closed  bool
}

func (s *Service) ring() *logRing { return &s.logs }

// close drops the entries and refuses later ones.
func (r *logRing) close() {
	r.mu.Lock()
	r.closed = true
	r.entries = nil
	r.mu.Unlock()
}

func (r *logRing) add(e logEntry) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return
	}
	r.next++
	e.ID = r.next
	r.entries = append(r.entries, e)
	if len(r.entries) > logRingSize {
		r.entries = append([]logEntry(nil), r.entries[len(r.entries)-logRingSize:]...)
	}
	r.mu.Unlock()
}

var sensitiveLogKeys = []string{"token", "secret", "password", "dsn", "credential", "authorization", "api_key", "apikey", "content", "message_body"}

func redactAttr(k, v string) string {
	lk := strings.ToLower(k)
	for _, s := range sensitiveLogKeys {
		if strings.Contains(lk, s) {
			return "[redacted]"
		}
	}
	v = observability.Redact(v) // before the cut, so a secret is never split past recognition
	if len(v) > 500 {
		return v[:500] + "…"
	}
	return v
}

type ringHandler struct {
	next  slog.Handler
	ring  *logRing
	attrs []slog.Attr
	group string
}

func (h *ringHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= slog.LevelInfo || h.next.Enabled(ctx, l)
}

func (h *ringHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level >= slog.LevelInfo {
		attrs := map[string]string{}
		add := func(a slog.Attr) {
			k := a.Key
			if h.group != "" {
				k = h.group + "." + k
			}
			attrs[k] = redactAttr(k, a.Value.Resolve().String())
		}
		for _, a := range h.attrs {
			add(a)
		}
		r.Attrs(func(a slog.Attr) bool { add(a); return true })
		h.ring.add(logEntry{At: r.Time.UTC().Format(time.RFC3339Nano), Level: strings.ToLower(r.Level.String()), Msg: observability.Redact(r.Message), Attrs: attrs})
	}
	if h.next.Enabled(ctx, r.Level) {
		return h.next.Handle(ctx, r)
	}
	return nil
}

func (h *ringHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return &ringHandler{next: h.next.WithAttrs(as), ring: h.ring, attrs: append(append([]slog.Attr(nil), h.attrs...), as...), group: h.group}
}

func (h *ringHandler) WithGroup(name string) slog.Handler {
	g := name
	if h.group != "" {
		g = h.group + "." + name
	}
	return &ringHandler{next: h.next.WithGroup(name), ring: h.ring, attrs: h.attrs, group: g}
}

// CaptureLogs returns a logger that also feeds this service's live log (runtime.logs). A logger that
// already feeds another service's log is unwrapped first: the practice instance is opened with the
// real instance's logger, and each console shows only its own instance's lines.
func (s *Service) CaptureLogs(l *slog.Logger) *slog.Logger {
	if l == nil {
		l = slog.Default()
	}
	next := l.Handler()
	if rh, ok := next.(*ringHandler); ok {
		next = rh.next
	}
	return slog.New(&ringHandler{next: next, ring: s.ring()})
}

// runtimeLogs: after_id (follow), level (minimum), q (text), limit.
func (s *Service) runtimeLogs(req action.Request) action.Result {
	in := inputMap(req)
	after := int64(intVal(in, "after_id", 0))
	limit := min(max(intVal(in, "limit", 200), 1), 1000)
	q := strings.ToLower(strVal(in, "q", ""))
	rank := map[string]int{"debug": 0, "info": 1, "warn": 2, "error": 3}
	minLevel := rank[strVal(in, "level", "info")]
	r := s.ring()
	r.mu.Lock()
	src := append([]logEntry(nil), r.entries...)
	last := r.next
	r.mu.Unlock()
	oldest := int64(0)
	if len(src) > 0 {
		oldest = src[0].ID
	}
	out := []logEntry{}
	for i := len(src) - 1; i >= 0 && len(out) < limit; i-- {
		e := src[i]
		if e.ID <= after || rank[e.Level] < minLevel {
			continue
		}
		if q != "" && !strings.Contains(strings.ToLower(e.Msg+" "+fmt.Sprint(e.Attrs)), q) {
			continue
		}
		out = append(out, e)
	}
	// oldest first
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
		out[i], out[j] = out[j], out[i]
	}
	return action.OK(req, map[string]any{"entries": out, "last_id": last, "oldest_id": oldest, "buffer": logRingSize, "source": "memory"})
}
