package application

import (
	"time"
)

// Thread offers back off when nobody takes them up. In the 2026-11 ops log 53 offers produced 5 threads and
// none of the last 31 (28 of them not answered at all). An offer nobody answers costs the channel a button
// under the bot's reply every time, so a channel whose last offers were all passed over rests, and rests
// longer each time it is passed over again, until someone opens a thread (then it starts over).
//
// There is no table for this: the state is read back from conversation_log (kind thread), which already
// holds every offer ("offer"), every thread opened ("opened") and what failed to show (outcome "not_shown").
// So it survives a restart, follows the log's retention, and goes with a person's rows when they withdraw
// consent (their offers then no longer count against the channel).
//
// An offer counts as passed over when it was neither followed by an "opened" thread nor too recent to have
// been answered (threadOfferSettle). A decline ("keep it here") counts too: it is a no.
const (
	// The first two passed-over offers cost nothing: one "no" is chance, and a second can be a busy hour.
	// The third offer is the one a pattern of ignoring holds back.
	threadBackoffFree = 2
	// The first rest is 6 hours, so the next offer comes in a different stretch of the day and not in the
	// same burst of talk the ignored one belonged to. Each further miss doubles it (6h, 12h, 24h, 48h …).
	threadBackoffBase = 6 * time.Hour
	// The longest rest is a week: seven passed-over offers in a row reach it. It is a cap so that a channel
	// whose habits change is asked again, once a week at the least; one opened thread clears everything.
	threadBackoffCap = 7 * 24 * time.Hour
	// An offer this young may still be taken up (a thread is opened within minutes or not at all), so it is
	// not counted as passed over yet. It is the span a conversation's turns are taken to be close.
	threadOfferSettle = threadOfferGap
	// Rows read back: enough to reach the cap (threadBackoffRest); more would not change the answer.
	threadBackoffRows = 24
	// outcome noted on an offer's log row when its reply never reached the channel: it was not shown, so
	// nobody could pass it over.
	threadOutcomeNotShown = "not_shown"
)

// threadBackoffRest is how long a channel rests after streak passed-over offers in a row.
func threadBackoffRest(streak int) time.Duration {
	if streak < threadBackoffFree {
		return 0
	}
	rest := threadBackoffBase
	for i := threadBackoffFree; i < streak && rest < threadBackoffCap; i++ {
		rest *= 2
	}
	return min(rest, threadBackoffCap)
}

// threadBackedOff reports whether the channel is resting from thread offers at now. When the log cannot
// be read it says no: the offer rules that were there before still apply.
func (s *Service) threadBackedOff(guildID, channelID string, now time.Time) bool {
	db, err := s.adminDB()
	if err != nil {
		return false
	}
	rows, err := db.Reader().Query(`SELECT decision, created_at, outcome FROM conversation_log WHERE guild_id=? AND channel_id=? AND kind=?
		AND decision IN ('offer','auto','opened') ORDER BY created_at DESC LIMIT ?`, guildID, channelID, convThread, threadBackoffRows)
	if err != nil {
		return false
	}
	defer rows.Close()
	var list []backoffRow
	for rows.Next() {
		var r backoffRow
		var at string
		if rows.Scan(&r.decision, &at, &r.outcome) != nil {
			return false
		}
		if r.at, err = time.Parse(time.RFC3339Nano, at); err != nil {
			return false
		}
		list = append(list, r)
	}
	if rows.Err() != nil {
		return false
	}
	return threadBackoffAt(list, now)
}

// backoffRow is one thread row of the log that matters here.
type backoffRow struct {
	decision, outcome string
	at                time.Time
}

// threadBackoffAt reads a channel's offer history (newest first) and says whether now falls in the rest
// the passed-over offers earned. An "opened" ends the count, except one that follows an "auto" (the bot
// opened that thread itself; nobody took anything up).
func threadBackoffAt(newestFirst []backoffRow, now time.Time) bool {
	streak := 0
	var last time.Time
	for i := 0; i < len(newestFirst); i++ {
		r := newestFirst[i]
		switch r.decision {
		case "offer":
			if r.outcome == threadOutcomeNotShown {
				continue
			}
			if last.IsZero() {
				last = r.at
			}
			if now.Sub(r.at) > threadOfferSettle {
				streak++
			}
		case "opened":
			if i+1 < len(newestFirst) && newestFirst[i+1].decision == "auto" {
				i++ // opened by the bot's own "auto": not a yes
				continue
			}
			return threadRestEarned(streak, last, now)
		}
	}
	return threadRestEarned(streak, last, now)
}

func threadRestEarned(streak int, last, now time.Time) bool {
	rest := threadBackoffRest(streak)
	return rest > 0 && !last.IsZero() && now.Sub(last) < rest
}
