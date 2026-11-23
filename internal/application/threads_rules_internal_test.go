package application

import (
	"testing"
	"time"

	"masamong/internal/conversation"
	"masamong/internal/lang"
	"masamong/internal/memory/store"
)

// The offer score adds two for a long run of turns, long replies, three tools and four lines left out, and
// one for a busy channel.
func TestThreadOfferScore(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		sig  offerSignals
		want int
	}{
		{offerSignals{}, 0},
		{offerSignals{turns: 6}, 2},
		{offerSignals{chars: 1200}, 2},
		{offerSignals{answer: 600}, 2},
		{offerSignals{chars: 1199, answer: 599}, 0},
		{offerSignals{tools: 3}, 2},
		{offerSignals{excluded: 4}, 2},
		{offerSignals{busy: 10}, 1},
		{offerSignals{turns: 6, chars: 1200, tools: 3, excluded: 4, busy: 10}, 9},
	} {
		if got := tc.sig.score(); got != tc.want {
			t.Errorf("%+v: %d, want %d", tc.sig, got, tc.want)
		}
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	lines := []conversation.Line{{At: now}, {At: now.Add(-time.Minute)}, {At: now.Add(-time.Minute - time.Second)}}
	if got := busyLines(lines, now); got != 2 {
		t.Errorf("lines in the last minute: %d", got)
	}
}

// The offer decision checks, in order: the score, the mode off, an offer already made, a "no" still
// resting, a backed-off channel (logged once per conversation), the hourly limit, a group, then auto or offer.
func TestThreadOfferDecision(t *testing.T) {
	t.Parallel()
	ready := offerFacts{score: threadOfferScore, mode: "button"}
	for _, tc := range []struct {
		name      string
		f         offerFacts
		want      string
		backedOff bool
	}{
		{"below the score", offerFacts{score: threadOfferScore - 1, mode: "off", offered: true}, "", false},
		{"off", offerFacts{score: threadOfferScore, mode: "off", offered: true}, "would_offer", false},
		{"offered", offerFacts{score: threadOfferScore, mode: "button", offered: true, declined: true}, "skip_offered", false},
		{"declined", offerFacts{score: threadOfferScore, mode: "button", declined: true, backoff: true}, "skip_declined", false},
		{"backoff", offerFacts{score: threadOfferScore, mode: "button", backoff: true, offersThisHour: 9}, "skip_backoff", true},
		{"backoff logged", offerFacts{score: threadOfferScore, mode: "button", backoff: true, backedOff: true}, "", true},
		{"rate", offerFacts{score: threadOfferScore, mode: "button", offersThisHour: threadOffersPerHour, callers: 2}, "skip_rate", false},
		{"group", offerFacts{score: threadOfferScore, mode: "auto_task", callers: 2, tools: 3}, "skip_group", false},
		{"auto", offerFacts{score: threadOfferScore, mode: "auto_task", tools: 3}, "auto", false},
		{"auto mode, few tools", offerFacts{score: threadOfferScore, mode: "auto_task", tools: 2}, "offer", false},
		{"offer", ready, "offer", false},
		{"backed off earlier, not now", offerFacts{score: threadOfferScore, mode: "button", backedOff: true}, "offer", true},
	} {
		got, backedOff := tc.f.decide()
		if got != tc.want || backedOff != tc.backedOff {
			t.Errorf("%s: %q %v, want %q %v", tc.name, got, backedOff, tc.want, tc.backedOff)
		}
	}
}

func TestThreadStopCueAndLines(t *testing.T) {
	t.Parallel()
	if got := threadStopText("  !그만!! "); got != "그만" {
		t.Fatalf("stop text: %q", got)
	}
	if got := threadStopText("/STOP 🙏"); got != "stop" {
		t.Fatalf("stop text: %q", got)
	}
	var ko, en lang.Pack
	ko.Thread.StopCues = []string{"그만"}
	en.Thread.StopCues = []string{"Stop"}
	if !threadStopCueIn("stop", &ko, &en) || threadStopCueIn("stopp", &ko, &en) {
		t.Fatal("stop cues")
	}
	if !aloneIn(map[string]bool{"a": true}, "a") || aloneIn(map[string]bool{"a": true, "b": true}, "a") || !aloneIn(nil, "a") {
		t.Fatal("alone")
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)
	turns := []store.ChatTurn{{Speaker: "u", Content: "hi", NativeID: "m1"}, {Speaker: "x", Content: "yo", Role: "assistant"}, {Speaker: "u", Content: "bye", NativeID: "m3"}}
	got := threadLines(turns, "t", "bot", now, func(turn store.ChatTurn) bool { return turn.NativeID == "m1" })
	if len(got) != 2 || got[0].ID != "t1" || !got[0].Bot || got[0].Author != "bot" || got[1].ID != "t2" || got[1].Author != "u" || got[1].Bot || !got[1].At.Equal(now) {
		t.Fatalf("lines: %+v", got)
	}
	if got := threadLines(turns, "s", "bot", now, nil); len(got) != 3 || got[0].ID != "s0" {
		t.Fatalf("lines without skip: %+v", got)
	}
}
