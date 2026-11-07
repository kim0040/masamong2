package application

import (
	"slices"
	"strings"
	"sync"
	"time"

	"masamong/internal/memory/store"
)

// A person fixes a typo in the seconds the reply takes. The tombstone of that edit refuses the turn when it commits,
// and in a channel that stores no messages nothing else holds the new words: they would never reach the context.
// So a running turn is told of edits of the message it answers, here. The words live in memory only: a turn that dies
// with the process stores nothing anyway, and the edit's job may be gone from the queue by the time it commits.
//
// An edit is noted before its intent is committed, and the turn decides inside its write transaction. So a turn that
// finds nothing noted cannot find the tombstone of an edit made while it ran, and one that commits first is reached
// by that edit like any stored turn. Nothing under the lock calls the store; the transaction's lock comes first.
//
// A deletion, a blanking edit, a withdrawal and a change of an earlier piece of a burst need no note: their tombstones
// refuse the row, and one that commits ahead of them is removed with its message.
type turnFlights struct {
	mu        sync.Mutex
	byMessage map[string][]*turnFlight
}

// turnFlight is one running turn: the newest revision of its message, once an edit has reached it. Guarded by
// turnFlights.mu.
type turnFlight struct {
	edited bool
	words  string
	at     time.Time
}

// flyTurn registers a turn that may store something; a lab turn stores nothing and has nothing to learn.
func (s *Service) flyTurn(in ChatInput) (*turnFlight, func()) {
	if in.DryRun || in.MessageID == "" {
		return nil, func() {}
	}
	fl, f := &turnFlight{}, &s.flights
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.byMessage == nil {
		f.byMessage = map[string][]*turnFlight{}
	}
	f.byMessage[in.MessageID] = append(f.byMessage[in.MessageID], fl)
	return fl, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		rest := slices.DeleteFunc(f.byMessage[in.MessageID], func(other *turnFlight) bool { return other == fl })
		if len(rest) == 0 {
			delete(f.byMessage, in.MessageID)
		} else {
			f.byMessage[in.MessageID] = rest
		}
	}
}

// edited notes an edit that is not refused as stale. The newest by the revision's time wins, in either arrival order.
func (f *turnFlights) edited(message, words string, at time.Time) {
	if strings.TrimSpace(words) == "" {
		return // a deletion
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fl := range f.byMessage[message] {
		if !fl.edited || !at.Before(fl.at) {
			fl.edited, fl.words, fl.at = true, words, at
		}
	}
}

// keptWords notes an edit that left the words as they were. Alone it changes nothing about the turn. After an edit it
// is the newer revision of the words to keep: the store would find the older one superseded.
func (f *turnFlights) keptWords(message, words string, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, fl := range f.byMessage[message] {
		if fl.edited && !at.Before(fl.at) {
			fl.words, fl.at = words, at
		}
	}
}

// toKeep is what the turn comes to, given the rows it would keep unchanged (the user's first). An edited message gives
// one user row with its new words and no reply: the reply was made from the old ones. Of a burst that row holds the
// answered message's words alone, as after an edit once stored, so it has no tie to the other pieces: the turn's text
// keeps no seams to tell their words apart by.
func (f *turnFlights) toKeep(fl *turnFlight, turns []store.ChatTurn) []store.ChatTurn {
	if fl == nil {
		return turns
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if !fl.edited {
		return turns
	}
	user := turns[0]
	return []store.ChatTurn{{Role: user.Role, Speaker: user.Speaker, Content: fl.words, NativeID: user.NativeID, SpeakerID: user.SpeakerID, Revision: fl.at.UTC().Format(time.RFC3339Nano)}}
}
