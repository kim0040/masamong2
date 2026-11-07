package application

import (
	"context"
	"log/slog"
	"time"
)

// laps times the steps of a read that the console waits on. When the whole read is slow it logs the
// steps that took the time, so a slow page names its cause in the log instead of only being slow.
type laps struct {
	s           *Service
	name        string
	level       slog.Level // warn for a read someone waits on, info for one the service makes for itself
	start, last time.Time
	parts       []any
}

const (
	lapNoted = 150 * time.Millisecond // a step at least this long is named
	lapSlow  = 2 * time.Second        // a read at least this long is logged
)

// backgroundReadKey marks a read the service makes for itself (the alert loop re-reads the dashboard and
// the doctor every few minutes): nobody waits on it, so a slow one is info, not a warning. The alerts it
// feeds do not depend on the log line.
type backgroundReadKey struct{}

func withBackgroundRead(ctx context.Context) context.Context {
	return context.WithValue(ctx, backgroundReadKey{}, true)
}

func (s *Service) laps(ctx context.Context, name string) *laps {
	now := time.Now()
	level := slog.LevelWarn
	if bg, _ := ctx.Value(backgroundReadKey{}).(bool); bg {
		level = slog.LevelInfo
	}
	return &laps{s: s, name: name, level: level, start: now, last: now}
}

func (l *laps) mark(step string) {
	now := time.Now()
	if d := now.Sub(l.last); d >= lapNoted {
		l.parts = append(l.parts, step+"_ms", d.Milliseconds())
	}
	l.last = now
}

func (l *laps) done() {
	total := time.Since(l.start)
	if total < lapSlow || l.s.Log == nil {
		return
	}
	l.s.Log.Log(context.Background(), l.level, l.name+": slow", append(l.parts, "total_ms", total.Milliseconds())...)
}
