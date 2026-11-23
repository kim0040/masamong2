package application

import (
	"sort"
	"time"
)

// The thread rules that need no I/O: the offer track of a caller's conversation, sweeping and trimming the
// in-memory thread table (with threadState.mu held), and the facts a thread offer is decided on.

// offerTrack is one caller's conversation in a channel, for the offer score.
type offerTrack struct {
	turns         []time.Time
	chars         int
	offered       bool
	declinedUntil time.Time
	offerTurn     string // the turn whose reply carried the offer (a "keep it here" is noted on it)
	backedOff     bool   // skip_backoff is logged once per conversation, not once per reply
}

// threadTrackIdle: a track this quiet is a finished conversation, and its "no" has run out too.
const threadTrackIdle = time.Hour

// note adds a turn and returns how many of the track's turns are within threadOfferGap: only those count
// for the score, so only those are kept.
func (tr *offerTrack) note(now time.Time) int {
	kept := tr.turns[:0]
	for _, at := range tr.turns {
		if now.Sub(at) <= threadOfferGap {
			kept = append(kept, at)
		}
	}
	tr.turns = append(kept, now)
	return len(tr.turns)
}

// sweepTracks forgets the tracks of conversations quiet for threadTrackIdle and the offers older than an
// hour (the lock held).
func (t *threadState) sweepTracks(now time.Time) {
	for key, tr := range t.tracks {
		if n := len(tr.turns); (n == 0 || now.Sub(tr.turns[n-1]) > threadTrackIdle) && !now.Before(tr.declinedUntil) {
			delete(t.tracks, key)
		}
	}
	for ch, list := range t.offersAt {
		kept := list[:0]
		for _, at := range list {
			if now.Sub(at) < time.Hour {
				kept = append(kept, at)
			}
		}
		if len(kept) == 0 {
			delete(t.offersAt, ch)
		} else {
			t.offersAt[ch] = kept
		}
	}
}

func callers(attribution map[string]any) int {
	n, _ := attribution["callers"].(int)
	return n
}

// trimKnown brings the thread table down to threadMaxKnown by letting go of the quietest threads that
// are not the bot's own open ones (the lock held). Their rows stay.
func (t *threadState) trimKnown(now time.Time) {
	over := len(t.threads) - threadMaxKnown
	if over <= 0 {
		return
	}
	type quiet struct {
		id string
		at time.Time
	}
	var drop []quiet
	for id, info := range t.threads {
		if !(info.BotOwned && info.Open) {
			drop = append(drop, quiet{id, info.LastAt})
		}
	}
	sort.Slice(drop, func(i, j int) bool { return drop[i].at.Before(drop[j].at) })
	for i := 0; i < over && i < len(drop); i++ {
		delete(t.threads, drop[i].id)
	}
}
