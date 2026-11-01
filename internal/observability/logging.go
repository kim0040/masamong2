package observability

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
)

type ctxKey int

const (
	turnKey ctxKey = iota
	jobKey
)

// WithTurn marks ctx as belonging to one chat turn, so log lines written with the *Context logging
// calls carry its id.
func WithTurn(ctx context.Context, turnID string) context.Context {
	return context.WithValue(ctx, turnKey, turnID)
}

// TurnID returns the id set by WithTurn, or "".
func TurnID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(turnKey).(string)
	return s
}

// WithJob marks ctx as belonging to one background job (import, backfill, index run).
func WithJob(ctx context.Context, jobID string) context.Context {
	return context.WithValue(ctx, jobKey, jobID)
}

// JobID returns the id set by WithJob, or "".
func JobID(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	s, _ := ctx.Value(jobKey).(string)
	return s
}

// Level is the daemon's log threshold; the JSON handler reads it on every record, so SetLevel takes
// effect without rebuilding any logger.
var Level = new(slog.LevelVar)

// SetLevel changes Level. Names are debug, info, warn and error in any case; "" means info.
func SetLevel(name string) error {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "", "info":
		Level.Set(slog.LevelInfo)
	case "debug":
		Level.Set(slog.LevelDebug)
	case "warn":
		Level.Set(slog.LevelWarn)
	case "error":
		Level.Set(slog.LevelError)
	default:
		return fmt.Errorf("log level %q: use debug, info, warn or error", name)
	}
	return nil
}

// ContextHandler adds turn_id and job_id to records logged with a context that carries them.
func ContextHandler(inner slog.Handler) slog.Handler { return contextHandler{inner} }

type contextHandler struct{ inner slog.Handler }

func (h contextHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h contextHandler) Handle(ctx context.Context, r slog.Record) error {
	if id := TurnID(ctx); id != "" {
		r.AddAttrs(slog.String("turn_id", id))
	}
	if id := JobID(ctx); id != "" {
		r.AddAttrs(slog.String("job_id", id))
	}
	return h.inner.Handle(ctx, r)
}

func (h contextHandler) WithAttrs(as []slog.Attr) slog.Handler {
	return contextHandler{h.inner.WithAttrs(as)}
}

func (h contextHandler) WithGroup(name string) slog.Handler {
	return contextHandler{h.inner.WithGroup(name)}
}

// RedactHandler runs Redact over the message and every textual attribute value (strings, errors and
// fmt.Stringers, inside groups too) before the record reaches inner. It never drops a record.
func RedactHandler(inner slog.Handler) slog.Handler { return redactHandler{inner} }

type redactHandler struct{ inner slog.Handler }

func (h redactHandler) Enabled(ctx context.Context, l slog.Level) bool {
	return h.inner.Enabled(ctx, l)
}

func (h redactHandler) Handle(ctx context.Context, r slog.Record) error {
	out := slog.NewRecord(r.Time, r.Level, Redact(r.Message), r.PC)
	r.Attrs(func(a slog.Attr) bool {
		out.AddAttrs(redactAttr(a))
		return true
	})
	return h.inner.Handle(ctx, out)
}

func (h redactHandler) WithAttrs(as []slog.Attr) slog.Handler {
	red := make([]slog.Attr, len(as))
	for i, a := range as {
		red[i] = redactAttr(a)
	}
	return redactHandler{h.inner.WithAttrs(red)}
}

func (h redactHandler) WithGroup(name string) slog.Handler {
	return redactHandler{h.inner.WithGroup(name)}
}

func redactAttr(a slog.Attr) slog.Attr {
	v := a.Value.Resolve()
	switch v.Kind() {
	case slog.KindString:
		return slog.String(a.Key, Redact(v.String()))
	case slog.KindGroup:
		g := v.Group()
		red := make([]any, len(g))
		for i, ga := range g {
			red[i] = redactAttr(ga)
		}
		return slog.Group(a.Key, red...)
	case slog.KindAny:
		switch x := v.Any().(type) {
		case error:
			return slog.String(a.Key, Redact(safeText(x.Error)))
		case fmt.Stringer:
			return slog.String(a.Key, Redact(safeText(x.String)))
		}
	}
	return slog.Attr{Key: a.Key, Value: v}
}

// safeText calls a value's Error or String method; a nil pointer receiver must not take logging down.
func safeText(f func() string) (s string) {
	defer func() {
		if recover() != nil {
			s = "<nil>"
		}
	}()
	return f()
}

// MinLevel passes only records at or above level to h.
func MinLevel(h slog.Handler, level slog.Level) slog.Handler { return minLevel{h, level} }

type minLevel struct {
	inner slog.Handler
	level slog.Level
}

func (m minLevel) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= m.level && m.inner.Enabled(ctx, l)
}

func (m minLevel) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < m.level {
		return nil
	}
	return m.inner.Handle(ctx, r)
}

func (m minLevel) WithAttrs(as []slog.Attr) slog.Handler {
	return minLevel{m.inner.WithAttrs(as), m.level}
}

func (m minLevel) WithGroup(name string) slog.Handler {
	return minLevel{m.inner.WithGroup(name), m.level}
}

// Fanout sends every record to all handlers that are enabled for its level. One handler's failure does
// not keep the others from getting the record; the first error is returned.
func Fanout(handlers ...slog.Handler) slog.Handler { return fanout(handlers) }

type fanout []slog.Handler

func (f fanout) Enabled(ctx context.Context, l slog.Level) bool {
	for _, h := range f {
		if h.Enabled(ctx, l) {
			return true
		}
	}
	return false
}

func (f fanout) Handle(ctx context.Context, r slog.Record) error {
	var first error
	for _, h := range f {
		if !h.Enabled(ctx, r.Level) {
			continue
		}
		if err := h.Handle(ctx, r.Clone()); err != nil && first == nil {
			first = err
		}
	}
	return first
}

func (f fanout) WithAttrs(as []slog.Attr) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithAttrs(as)
	}
	return out
}

func (f fanout) WithGroup(name string) slog.Handler {
	out := make(fanout, len(f))
	for i, h := range f {
		out[i] = h.WithGroup(name)
	}
	return out
}
