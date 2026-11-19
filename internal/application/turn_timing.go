package application

import (
	"context"
	"strings"
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

// deliverTimed preserves the exact acknowledgement time for a connector editing a pre-existing post.
func deliverTimed(ctx context.Context, in ChatInput, res chat.Result) (said string, firstSend, deliverEnd time.Time) {
	dctx, mark := withFirstSend(ctx)
	said = in.Deliver(dctx, res)
	return said, mark.at, time.Now()
}

// spokenTimes uses Discord's clock for both sides of an exchange. The observation or send
// acknowledgement fills in for a connector without snowflakes; neither time waits for storage.
func (s *Service) spokenTimes(in ChatInput, firstSend, deliverEnd time.Time, text string) (request, answer time.Time, spans string) {
	request = s.callCutoff(in)
	live := s.liveLines(in.GuildID, in.ChannelID)
	for _, l := range live {
		if l.Bot && !deliverEnd.IsZero() {
			if in.MessageID != "" && l.ReplyTo == in.MessageID && (answer.IsZero() || l.At.Before(answer)) {
				answer = l.At
			}
		}
	}
	if !deliverEnd.IsZero() {
		// A tool attachment may also reply directly to the request, before the
		// text. The registered parts identify which post actually carried the answer.
		if parts := s.registeredParts(in.turnID, live); len(parts) != 0 {
			if strings.HasPrefix(in.MessageID, "dc:") && !firstSend.IsZero() {
				// Slash follow-ups edit the deferred first post. Its snowflake
				// dates the placeholder, not the publication of its text.
				parts[0].At = firstSend
			}
			if len(parts) > 1 {
				spans = encodeSpans(partSpans(text, parts))
			}
			return request, parts[0].At, spans
		}
		if answer.IsZero() {
			answer = deliverEnd
			if !firstSend.IsZero() {
				answer = firstSend
			}
		}
	}
	if answer.IsZero() {
		answer = s.Clock.Now() // a synchronous reply with no delivery callback
	}
	return request, answer, spans
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
