package application

import (
	"context"
	"strings"
	"sync/atomic"
	"time"

	"masamong/internal/chat"
	"masamong/internal/memory/store"
)

// The stages of a chat turn after its answer (chatTurn runs them in this order): note what the answer
// cost and used, deliver it, open the follow-up window, keep the exchange in the conversation.

// turnDelivery is when a turn's reply went out and whether its registration lost the delivery boundaries.
type turnDelivery struct {
	firstSend, end time.Time
	rejected       atomic.Bool
}

// noteAnswer adds to the result what the turn spent outside the runtime and the time it waited for its
// place, and joins a picture's description made while answering to the kept message. Memory hits outside the
// channel's rooms are never expected (search keeps to them); they were dropped before the prompt, and the
// operator hears of it because it means a defect in the isolation between rooms.
func (s *Service) noteAnswer(ctx context.Context, in *ChatInput, t *turnRun, res *chat.Result, queueWait int) {
	if in.sourceRefs != nil && res.Room != nil && res.Room.Cards > 0 {
		*in.sourceRefs = append(*in.sourceRefs, t.personSources...)
	}
	noteTurnTiming(res, "queue_wait", queueWait)
	res.CostUSD += t.conv.costUSD + t.explicitCost + t.visionCost
	if in.vision != nil && in.vision.Message != "" {
		in.Message = in.vision.Message // a description made while answering joins the kept message
	}
	if res.ScopeDropped > 0 {
		s.Log.ErrorContext(ctx, "memory hits outside the channel's rooms were dropped before the prompt", "dropped", res.ScopeDropped, "rooms", len(s.channelRooms(in.GuildID, in.ChannelID)))
		s.queueAlert(alertMemoryScope, map[string]any{"n": res.ScopeDropped})
	}
}

// deliverTurn sends the reply through the connector, when it has one, and notes when the first part went
// out. said is the text delivered. The caller registers d.rejected for the turn before and removes it after
// the turn is kept.
func deliverTurn(ctx context.Context, in ChatInput, res *chat.Result, d *turnDelivery) (said string) {
	said, d.firstSend, d.end = deliverTimed(ctx, in, *res)
	if !d.firstSend.IsZero() && !in.started.IsZero() {
		noteTurnTiming(res, "first_send", max(int(d.firstSend.Sub(in.started).Milliseconds()), 0))
	}
	return said
}

// afterDelivery opens the follow-up window after a reply that said something, and counts a banter line
// that went out.
func (s *Service) afterDelivery(in ChatInput, t *turnRun, said string) {
	if strings.TrimSpace(said) != "" && !in.Proactive && !in.banter {
		s.openFollowWindow(in, said, t.pol, in.FollowUp)
		s.implicitNoteReply(in)
	}
	if in.banter && in.Deliver != nil && strings.TrimSpace(said) != "" {
		s.noteBanter(in.GuildID, in.ChannelID, "line", in.turnID)
	}
}

// keepTurn keeps the request and the reply in the channel's conversation, unless the speaker withdrew
// consent to being remembered (they are not kept in the chat context either). A reply with no text (a
// picture the tools posted) keeps the request in the conversation as before.
func (s *Service) keepTurn(ctx context.Context, in ChatInput, t *turnRun, res chat.Result, said string, d *turnDelivery) {
	kept := in.Deliver == nil || strings.TrimSpace(said) != "" || strings.TrimSpace(res.Answer) == ""
	callAddress := store.CallAccepted
	if kept && !in.DryRun && !in.Proactive && !in.banter && in.MessageID != "" {
		if s.liveToBot(in) {
			callAddress = store.CallReply
		}
	}
	if !in.DryRun && s.MemoryAllowed(in.SpeakerID) && kept {
		s.storeTurn(ctx, in, t, res, said, d, callAddress)
	}
}

// storeTurn appends the exchange to the channel's conversation, with what the reply read and when each
// side was spoken, and links the posted reply to its request.
func (s *Service) storeTurn(ctx context.Context, in ChatInput, t *turnRun, res chat.Result, said string, d *turnDelivery, callAddress int) {
	requestAt, answerAt, spans := s.spokenTimes(in, d.firstSend, d.end, said)
	if d.rejected.Load() {
		// The text was delivered, but the registration transaction lost its
		// boundaries. Do not offer any of it before delivery completed.
		answerAt, spans = d.end, ""
	}
	answerAt = afterRequest(answerAt, requestAt)
	var sources []string
	if in.sourceRefs != nil {
		sources = *in.sourceRefs
	}
	sources = uniqueStrings(append(append(append([]string{}, sources...), lookedUp(res)...), in.MessageID, personRef(in.SpeakerID)))
	turns := []store.ChatTurn{{Role: "user", Speaker: in.Speaker, Content: in.Message, NativeID: in.MessageID, SpeakerID: in.SpeakerID, Merged: in.Fragments, ReadAsOf: in.readAsOf, SpokenAt: requestAt.UTC().Format(time.RFC3339Nano), CallAddress: callAddress}}
	if in.vision != nil && in.vision.Mode == "described" {
		// the description came from other messages too: deleting one of them removes this turn
		turns[0].DerivedFrom = in.vision.Sources
	}
	if !toolFailureToReplay(res.Tools) {
		turns = append(turns, store.ChatTurn{Role: "assistant", Content: said, NativeID: in.MessageID, SpeakerID: in.SpeakerID,
			SourceIDs: sources, Merged: in.Fragments, ReadAsOf: in.readAsOf, SpokenAt: answerAt.UTC().Format(time.RFC3339Nano), DeliveryTurn: in.turnID, DeliverySpans: spans})
	}
	committed, err := t.eng.Store.AppendDecidedChatTurns(t.key, func() []store.ChatTurn { return s.flights.toKeep(in.flight, turns) }, chatTurnsKept)
	if err != nil {
		msg := "could not keep delivered conversation"
		if committed {
			msg = "kept delivered conversation but could not prune older turns"
		}
		s.Log.WarnContext(ctx, msg, "turn", in.turnID, "err", err)
	}
	if committed && len(turns) == 2 && in.Deliver != nil && !d.rejected.Load() && in.MessageID != "" {
		s.linkPostedReply(in.turnID, in.MessageID, t.key)
	}
}
