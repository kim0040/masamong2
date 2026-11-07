package application

import (
	"context"
	"sync"
	"time"

	"masamong/internal/chat"
)

// The turn's own clock around the steps the chat runtime times (internal/chat Result.Timing): the wait
// for the channel's turn and a model slot ("queue_wait"), the time to the first piece of the reply on
// screen ("first_send": from the turn's start, queue wait included, to the first message the person can
// read), and what the turn still did after the reply went out ("post_send": keeping the conversation,
// the follow-up window, the trace). Without them a third of a turn's time is in no step.

// firstSendKey carries a turn's first-send marker to the connector that posts the reply.
type firstSendKey struct{}

type firstSendMark struct {
	once sync.Once
	at   time.Time
}

func withFirstSend(ctx context.Context) (context.Context, *firstSendMark) {
	m := &firstSendMark{}
	return context.WithValue(ctx, firstSendKey{}, m), m
}

// NoteFirstSend is called by a connector when the first message of a reply has reached the channel; ctx
// is the context ChatInput.Deliver was given. Later calls and other contexts do nothing.
func NoteFirstSend(ctx context.Context) {
	if m, _ := ctx.Value(firstSendKey{}).(*firstSendMark); m != nil {
		m.once.Do(func() { m.at = time.Now() })
	}
}

// deliverTimed runs the turn's Deliver and says how long the reply took to appear (-1: the connector did
// not report a first message, as when nothing was sent).
func deliverTimed(ctx context.Context, in ChatInput, res chat.Result) (said string, firstMS int, deliverEnd time.Time) {
	dctx, mark := withFirstSend(ctx)
	said = in.Deliver(dctx, res)
	deliverEnd = time.Now()
	firstMS = -1
	if !mark.at.IsZero() && !in.started.IsZero() {
		firstMS = max(int(mark.at.Sub(in.started).Milliseconds()), 0)
	}
	return said, firstMS, deliverEnd
}

// addTurnSpans puts the runtime's spans and the join's side into a turn record, as fields of their own
// next to timing_ms (consumers of the old keys are untouched).
func addTurnSpans(body map[string]any, res chat.Result) {
	if len(res.Spans) > 0 {
		body["spans"] = res.Spans
	}
	if res.WaitedOn != "" {
		body["waited_on"] = res.WaitedOn
	}
}

// noteTurnTiming adds one of the application's timings to the result; a negative value is "not measured"
// and a result that never reached the runtime has no timing map.
func noteTurnTiming(res *chat.Result, key string, ms int) {
	if res.Timing != nil && ms >= 0 {
		res.Timing[key] = ms
	}
}
