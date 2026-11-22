package application

import (
	"context"
	"errors"
	"strings"
	"time"

	"masamong/internal/chat"
	"masamong/internal/errclass"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/store"
	"masamong/internal/observability"
)

// ---------------------------------------------------------------------------
// Chat turns (Discord replies and the chat.respond Action)
// ---------------------------------------------------------------------------

// The clock of a chat turn, the longest of them (the connector's: a command's follow-up has 180 s in all, a chime-in 2
// minutes, and a lab turn stores nothing). A turn gets ChatTurnTimeout once it has its place in the channel, and the
// wait for that place is bounded by ChatQueueWait. ChatTurnCeiling bounds the whole call, whatever happens in between.
const (
	ChatTurnTimeout = 180 * time.Second
	ChatQueueWait   = 120 * time.Second
	ChatTurnCeiling = ChatQueueWait + ChatTurnTimeout + 30*time.Second
)

// chatLinkHold is how long a turn removed by a deletion stays indexed for a turn that read it before it went, even with
// nothing linking to it yet (store.PruneChatHolders). A turn reads the conversation once it has its place, so
// ChatTurnCeiling is more than it can run for after that; the margin is the wait for the write connection its commit
// takes after its clock ran out, which no clock bounds. Retention runs once a day: a longer hold costs one more day of
// a few index rows, a shorter one a reply.
const chatLinkHold = ChatTurnCeiling + 5*time.Minute

func toolsetHas(ts chat.Toolset, id string) (chat.ToolInfo, bool) {
	for _, t := range ts.Enabled {
		if t.ID == id {
			return t, true
		}
	}
	return chat.ToolInfo{}, false
}

// ChatTurn answers one message in a channel: planner → tools → channel memory → friend-like reply.
// ChatTurn answers one message and records the outcome in the turn log (answered, refused and failed
// turns alike), so the console can show what the bot did and why.
func (s *Service) ChatTurn(ctx context.Context, in ChatInput) (chat.Result, error) {
	ctx, done, ok := s.beginWork(ctx)
	if !ok {
		return chat.Result{}, errors.New("service closed")
	}
	defer done()
	if s.Store != nil && s.Store.HasPendingEvents("source-change") {
		return chat.Result{}, ErrChatBusy
	}
	if in.GuildID == "" {
		if !in.DryRun && !s.DMChatAllowed(ctx, in.SpeakerID) {
			return chat.Result{}, ErrDMClosedChat
		}
		in.NameOnly, in.FollowUp, in.Proactive = false, false, false
	}
	started := time.Now()
	in.turnID = newID("turn_")
	in.started = started
	flight, land := s.flyTurn(in)
	defer land()
	in.flight = flight
	in.readAsOf = store.SettledMark(s.Store)
	if len(in.Earlier) > 0 || len(in.Images) > 0 {
		in.readAsOf = min(in.readAsOf, in.ReadFrom)
	}
	ctx = observability.WithTurn(ctx, in.turnID)
	sources := append([]string{}, in.Fragments...)
	in.sourceRefs = &sources
	in.context = map[string]any{}
	if len(in.Images) > 0 {
		in.imageOnly = in.ImageOnly || strings.TrimSpace(in.Message) == ""
		in.vision = &visionState{SkipCounts: map[string]int{}}
	}
	res, err := s.chatTurn(ctx, in)
	if (strings.TrimSpace(in.Message) == "" && !in.imageOnly) || in.ChannelID == "" {
		return res, err
	}
	outcome := "answered"
	switch {
	case errors.Is(err, ErrChatPaused):
		outcome = "paused"
	case errors.Is(err, ErrChatDisabled):
		outcome = "disabled"
	case errors.Is(err, ErrChatLimited), errors.Is(err, ErrChatBusy), errors.As(err, new(DMLimited)):
		outcome = "limited"
	case errors.Is(err, ErrChatBudget):
		outcome = "budget"
	case errors.Is(err, ErrNotAddressed):
		outcome = "not_addressed"
	case errors.Is(err, ErrNoVision), errors.Is(err, ErrImageUnreadable):
		outcome = "no_vision" // a message that was only a picture nobody could look at
	case errors.As(err, new(ReactOnly)):
		outcome = "reacted"
	case errors.Is(err, context.Canceled): // the person said stop, or the daemon is going down
		outcome = "canceled"
	case err != nil:
		outcome = "error"
	case strings.TrimSpace(res.Answer) == "":
		outcome = "silent"
	}
	names := []string{}
	for _, t := range res.Tools {
		names = append(names, t.Tool)
	}
	message, messageMax := in.Message, 400
	if in.vision != nil && in.vision.Message != "" {
		message, messageMax = in.vision.Message, 1200 // with the picture's tag or description
	}
	body := map[string]any{"message": clip(message, messageMax), "message_id": in.MessageID, "speaker": in.Speaker, "rag_state": res.RagState,
		"answer_preview": clip(res.Answer, 280), "answer": clip(res.Answer, 4000), "timing_ms": res.Timing,
		"memory_state": res.Assess.MemoryState, "intent_source": res.Intent.Source, "tools": recordTools(res.Tools),
		"plan": res.Plan.ToolPlan, "hits": hitBrief(res.Hits), "ranking": res.Retrieve["applied"],
		"context": map[string]any{"observation_ids": uniqueStrings(sources), "attribution": in.context}}
	if err != nil && outcome != "reacted" && outcome != "not_addressed" && outcome != "canceled" && outcome != "no_vision" { // decisions, not failures
		body["error"] = err.Error()
	}
	if outcome == "error" && !in.DryRun {
		s.LogProblem(in.GuildID, in.ChannelID, logReplyFailed, "", errclass.Of(err))
	}
	if len(res.Skipped) > 0 {
		body["skipped"] = res.Skipped // steps the per-turn limits held back (chat.Guard)
	}
	var na notAddressedErr
	if errors.As(err, &na) {
		body["name_call"] = map[string]any{"decision": na.decision, "tone": na.tone} // closed values, never model text
	}
	addToolLoop(body, res)
	addTurnSpans(body, res)
	visionRecord(body, in.vision)
	noteRoomMemory(body, res)
	res.TurnID = in.turnID
	for _, step := range res.Skipped { // optional steps the turn's Guard held back stay visible in the call ledger
		s.noteToolCall(ctx, toolCall{Tool: step, Guard: "skipped", Status: "skipped"})
	}
	if !in.DryRun { // a console test chat is the operator looking on purpose
		// a remark about the bot that was not said to it follows the room's logging level like any turn:
		// the operator checks the name-call decision against the words (the memory captures them anyway)
		body = s.withLoggingLevel(s.channelPolicy(in.GuildID, in.ChannelID).Logging.Level, outcome, err, body)
	}
	s.recordTurn(TurnRecord{ID: res.TurnID, GuildID: in.GuildID, ChannelID: in.ChannelID, SpeakerID: in.SpeakerID, Outcome: outcome, Tools: names, Hits: len(res.Hits),
		CostUSD: res.CostUSD, LatencyMS: int(time.Since(started).Milliseconds()), Lab: in.DryRun, ReadAsOf: in.readAsOf, Fragments: in.Fragments, Body: body})
	return res, err
}

// chatTurn runs one turn's stages in order: wait for the channel (enterTurn), admit the turn (admitTurn),
// open it (openTurn), read the conversation (readTurnContext), pick the tools (turnTools), write the rules
// (turnRuntime), read room memory (turnMemory), look at the pictures, answer, then note the answer
// (noteAnswer), deliver it (deliverTurn), open the follow-up window (afterDelivery) and keep the exchange
// (keepTurn).
func (s *Service) chatTurn(ctx context.Context, in ChatInput) (chat.Result, error) {
	if (strings.TrimSpace(in.Message) == "" && !in.imageOnly) || in.ChannelID == "" {
		return chat.Result{}, errors.New("channel_id and message required")
	}
	release, queueWait, err := s.enterTurn(ctx, in)
	if err != nil {
		return chat.Result{}, err
	}
	defer release()
	if in.TurnTimeout > 0 {
		var stopTurn context.CancelFunc
		ctx, stopTurn = context.WithTimeout(ctx, in.TurnTimeout)
		defer stopTurn()
	}
	if err := s.admitTurn(ctx, &in); err != nil {
		return chat.Result{}, err
	}
	t, err := s.openTurn(&in)
	if err != nil {
		return chat.Result{}, err
	}
	s.readTurnContext(ctx, &in, t)
	s.turnTools(ctx, in, t)
	rt := s.turnRuntime(in, t)
	s.turnMemory(ctx, &in, t)
	var images []chat.Image
	if in.vision != nil {
		// the pictures the bot was called with: fetched now, looked at by the answer model or described
		images, t.visionCost, err = s.prepareVision(ctx, &in, t.doc, t.llm, t.voice.Pack)
		if err != nil {
			return chat.Result{CostUSD: t.visionCost}, err
		}
		if len(images) > 0 {
			ctx = withVision(ctx, in.vision)
		}
	}
	res, err := rt.Answer(ctx, s.turnRequest(in, t, images))
	s.noteAnswer(ctx, &in, t, &res, queueWait)
	if err != nil {
		return res, err
	}
	res.Answer = s.safeAnswer(res.Answer, t.voice, chat.ComposeSystem(t.voice, t.persona, t.rules, t.doc.Memory.AgentPrompt))
	said := res.Answer
	res.TurnID = in.turnID
	if in.vision != nil {
		s.notePartialVision(&in, in.vision)
	}
	s.decideThreadOffer(in, t.pol, res, in.context)
	var d turnDelivery
	if in.Deliver != nil {
		s.delivering.Store(in.turnID, &d.rejected)
		defer s.delivering.Delete(in.turnID)
		said = deliverTurn(ctx, in, &res, &d)
	}
	s.afterDelivery(in, t, said)
	s.keepTurn(ctx, in, t, res, said, &d)
	s.logTurn(in, res)
	if !d.end.IsZero() {
		noteTurnTiming(&res, "post_send", int(time.Since(d.end).Milliseconds()))
	}
	return res, nil
}

// toolFailureToReplay keeps a failed tool exchange from becoming an answer the bot cites again in an
// unrelated turn. The user message remains in context; the current turn's tool result is still shown.
// A failed web search does not count when another web search of the same turn succeeded for the same
// query (trimmed, case-folded): the answer then rests on the one that did. A failed search for a
// different query still counts, as does one whose query is not known (a call the limits refused), and any
// other tool's failure. A search that found nothing ("empty") is a failure, not a success.
func toolFailureToReplay(results []chat.ToolResult) bool {
	succeeded := map[string]bool{}
	for _, result := range results {
		if q := foldedSearchQuery(result); result.Tool == "search.web" && q != "" && !toolFailed(result) {
			succeeded[q] = true
		}
	}
	for _, result := range results {
		if q := foldedSearchQuery(result); result.Tool == "search.web" && q != "" && succeeded[q] {
			continue
		}
		if toolFailed(result) {
			return true
		}
	}
	return false
}

// foldedSearchQuery is the query a search.web result was asked, trimmed and case-folded, so two
// searches for the same words compare equal. An absent query is empty and matches nothing.
func foldedSearchQuery(result chat.ToolResult) string {
	if result.Data == nil {
		return ""
	}
	q, _ := result.Data["query"].(string)
	return strings.ToLower(strings.TrimSpace(q))
}

// toolFailed reports whether one tool result is a failure, a refusal or an empty answer.
func toolFailed(result chat.ToolResult) bool {
	status := result.Status
	if status == "" {
		status, _ = result.Data["status"].(string)
	}
	switch status {
	case "unconfigured", "unavailable", "no_results", "empty", "no_area", "not_found", "error":
		return true
	}
	return !result.OK
}

func rankingFor(override string, def retrieval.RankingMode) retrieval.RankingMode {
	if override != "" {
		if m, ok := retrieval.Normalize(override); ok {
			return m
		}
	}
	return def
}
