package discord

import "time"

// shutdownGrace replaces turnGrace and afterGrace for one gateway; a zero field keeps the default.
type shutdownGrace struct{ turn, after time.Duration }

func (s shutdownGrace) turnLimit() time.Duration  { return durationOr(s.turn, turnGrace) }
func (s shutdownGrace) afterLimit() time.Duration { return durationOr(s.after, afterGrace) }

func durationOr(d, def time.Duration) time.Duration {
	if d == 0 {
		return def
	}
	return d
}
