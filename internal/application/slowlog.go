package application

import "time"

// laps times the steps of a read that the console waits on. When the whole read is slow it logs the
// steps that took the time, so a slow page names its cause in the log instead of only being slow.
type laps struct {
	s           *Service
	name        string
	start, last time.Time
	parts       []any
}

const (
	lapNoted = 150 * time.Millisecond // a step at least this long is named
	lapSlow  = 2 * time.Second        // a read at least this long is logged
)

func (s *Service) laps(name string) *laps {
	now := time.Now()
	return &laps{s: s, name: name, start: now, last: now}
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
	l.s.Log.Warn(l.name+": slow", append(l.parts, "total_ms", total.Milliseconds())...)
}
