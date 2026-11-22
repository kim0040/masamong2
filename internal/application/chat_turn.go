package application

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/errclass"
	"masamong/internal/lang"
	"masamong/internal/memory/blocks"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
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

func (s *Service) chatTurn(ctx context.Context, in ChatInput) (chat.Result, error) {
	if (strings.TrimSpace(in.Message) == "" && !in.imageOnly) || in.ChannelID == "" {
		return chat.Result{}, errors.New("channel_id and message required")
	}
	// Channels answer in parallel, one turn at a time within a channel, at most
	// limits.max_concurrent_turns at once. The limits and budget below are checked after the wait so
	// they see what the turns ahead of this one spent.
	waitCtx, stopWait := ctx, context.CancelFunc(func() {})
	if in.MaxWait > 0 {
		waitCtx, stopWait = context.WithTimeout(ctx, in.MaxWait)
	}
	queued := time.Now()
	release, err := s.turns.enter(waitCtx, blocks.ChannelKey(in.GuildID, in.ChannelID), s.Doc().Limits.ConcurrentTurns())
	queueWait := int(time.Since(queued).Milliseconds())
	stopWait()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			err = ErrChatBusy // waited too long: the message is stale, the person is told to ask again
		}
		return chat.Result{}, err
	}
	defer release()
	if in.TurnTimeout > 0 {
		var stopTurn context.CancelFunc
		ctx, stopTurn = context.WithTimeout(ctx, in.TurnTimeout)
		defer stopTurn()
	}
	if in.NameOnly && !in.DryRun && !s.Paused(in.GuildID, in.ChannelID) {
		// the classifier is a model call and a banter emoji skips the reply: neither where replies are
		// off or the budget is spent (a name alone gets no budget notice)
		if err := s.chatOpen(in); err != nil {
			if errors.Is(err, ErrChatBudget) {
				err = ErrNotAddressed
			}
			return chat.Result{}, err
		}
		banter, err := s.nameCallGate(ctx, in)
		if err != nil {
			return chat.Result{}, err
		}
		in.banter = banter
		if in.Addressed != nil {
			in.Addressed()
		}
	}
	if err := s.chatGate(in); err != nil {
		return chat.Result{}, err
	}
	doc := s.Doc()
	eng, err := s.MemoryEngine()
	if err != nil {
		return chat.Result{}, err
	}
	pol := s.channelPolicy(in.GuildID, in.ChannelID)
	voice := s.voiceFor(in.GuildID, pol)
	voice.Pack = lang.For(chat.DetectLanguage(in.Message), in.Locale, voice.Pack.Tag)
	if in.imageOnly && strings.TrimSpace(in.Message) == "" {
		in.Message = voice.Pack.VisionText().Attached // a picture alone: the request is the picture
	}
	routes, err := s.chatRoutes(doc)
	if err != nil {
		return chat.Result{}, err
	}
	key := blocks.ChannelKey(in.GuildID, in.ChannelID)
	room := engine.RoomKey(in.GuildID, in.ChannelID)
	var history []chat.Turn
	// The window is more than a prompt shows. What reaches the prompt is read; a turn further back is held by its link
	// alone, as is every turn in a channel that shows each caller only their conversation: a withdrawal follows the
	// link, and a deleted message takes only the replies that read it (store.ChatReaders). No prompt holds a row before
	// the last chat.HistoryShown, whichever rows are empty or long: every prompt built from this history keeps to them,
	// and a caller's conversation records each line it shows (conv.sources).
	var windowAll, windowShown, windowLinks []string
	cutoff := s.callCutoff(in)
	turns, readErr := eng.Store.RecentChatTurns(key, 16)
	if readErr == nil {
		for i, t := range turns {
			read := t.SourceIDs
			if t.Role == "user" && t.NativeID != "" {
				read = append(slices.Clone(read), corpus.DiscordLiveObservationID(room, t.NativeID))
			}
			windowAll = append(windowAll, read...)
			windowLinks = append(windowLinks, t.SourceIDs[0]) // its own link comes first
			if i >= len(turns)-chat.HistoryShown {
				windowShown = append(windowShown, read...)
			}
		}
	}
	var recent []corpus.Message
	if msgs, err := eng.Store.RecentMessages(room, 13); err == nil {
		// the message being answered is the latest one captured; an earlier message with the same
		// words (someone else's "ㅇㅇ") stays
		dropped := false
		for i := len(msgs) - 1; i >= 0; i-- {
			if !dropped && msgs[i].Content == strings.TrimSpace(in.Message) {
				dropped = true
				continue
			}
			recent = append(recent, msgs[i])
		}
		slices.Reverse(recent)
		if len(recent) > 12 {
			recent = recent[len(recent)-12:]
		}
	}
	ambient := make([]string, 0, len(recent))
	for _, m := range recent {
		ambient = append(ambient, m.ObservationID)
	}
	llm := chatLLM{s: s, routes: routes}
	maxHistory := 0
	conv, attributed := s.callerConversation(ctx, in, eng, pol, voice, llm)
	if attributed {
		// the caller's conversation replaces the channel's plain history and recent lines: the other
		// conversations of the room are not shown to the planner or the answer at all
		history, recent, maxHistory = conv.history, nil, 80
		if in.sourceRefs != nil {
			*in.sourceRefs = append(append(*in.sourceRefs, windowLinks...), conv.sources...)
		}
		for k, v := range conv.ledger {
			if in.context != nil {
				in.context[k] = v
			}
		}
	} else {
		// Only plain history needs these timestamps; attribution resolves its own larger window.
		if readErr == nil {
			_ = s.timeChatTurns(in, turns)
			for _, t := range turns {
				stamp := t.SpokenAt
				if stamp == "" {
					stamp = t.CreatedAt
				}
				at, _ := time.Parse(time.RFC3339Nano, stamp)
				content := visibleDelivery(t.Content, t.DeliverySpans, cutoff)
				if t.Role == "assistant" && in.MessageID != "" && !cutoff.IsZero() && at.After(cutoff) {
					content = "" // plain history also cannot see a delivery not yet started
				}
				history = append(history, chat.Turn{Role: t.Role, Speaker: t.Speaker, Content: content, At: at})
			}
		}
		// the recent lines reach the reply only here (RecentRoom below), so only here is it built on them. With the
		// caller's conversation in their place it is built on the lines that conversation picked (conv.sources). A
		// line offered to the reference classifier and not picked leaves nothing: it answers with labels only.
		if in.sourceRefs != nil {
			*in.sourceRefs = append(append(append(*in.sourceRefs, windowLinks...), windowShown...), ambient...)
		}
	}
	toolset := s.chatToolset(voice, in.GuildID, in.ChannelID)
	_, memTool := toolsetHas(toolset, "memory.search")
	memoryOn := pol.MemoryUse != "off" && doc.Memory.Retrieve != "off" && memTool
	if in.banter {
		// one light line back: nothing looked up, nothing run
		memoryOn, toolset = false, chat.Toolset{Known: map[string]bool{}}
	}
	if memoryOn {
		if err := eng.EnsureLoaded(); err != nil {
			s.Log.WarnContext(ctx, "memory reload failed", "err", err)
		}
	}
	tz := s.instanceZone()
	rules := draftOr(in, in.DraftRules, pol.Rules)
	if in.Proactive {
		rules = strings.TrimSpace(rules + "\n\n" + voice.Pack.Features.ProactiveRules)
	}
	if in.banter {
		rules = strings.TrimSpace(rules + "\n\n" + voice.Pack.Conversation.BanterRules)
	}
	if note := s.roomStyleRules(in.GuildID, in.ChannelID, pol, voice.Pack); note != "" && !in.DryRun {
		rules = strings.TrimSpace(rules + "\n\n" + note)
	}
	mem := gatedMemory{s: s, eng: eng, inv: &action.Invoker{Via: "chat", GuildID: in.GuildID, ChannelID: in.ChannelID, UserID: in.SpeakerID}, noWarm: !s.warmsQueries(doc, in)}
	rt := &chat.Runtime{LLM: llm, Memory: mem, Tools: s.chatTool(in, voice)}
	persona := draftOr(in, in.DraftStyle, s.personaText(personaFor(in, pol.PersonaRef)))
	// room memory (P4-3 roommemory_read.go) and explicit memory requests (P4-4 explicit.go)
	roomIn := in
	if in.sourceRefs != nil {
		// what room memory takes as already known in the room does not depend on what this reply is built on
		known := append(append(slices.Clone(*in.sourceRefs), windowAll...), ambient...)
		roomIn.sourceRefs = &known
	}
	var personSources []string
	roomMemory, people := s.turnRoomMemory(ctx, roomIn, pol, voice, history, recent, &personSources)
	explicit, explicitCost := s.explicitMemoryTurn(ctx, in, voice, llm, history)
	if explicit != nil {
		in.Earlier = append(in.Earlier, *explicit)
	}
	var images []chat.Image
	visionCost := 0.0
	if in.vision != nil {
		// the pictures the bot was called with: fetched now, looked at by the answer model or described
		images, visionCost, err = s.prepareVision(ctx, &in, doc, llm, voice.Pack)
		if err != nil {
			return chat.Result{CostUSD: visionCost}, err
		}
		if len(images) > 0 {
			ctx = withVision(ctx, in.vision)
		}
	}
	res, err := rt.Answer(ctx, chat.Request{
		Message: in.Message, Speaker: in.Speaker, History: history, RecentRoom: recent,
		Persona: persona, Rules: rules, AgentPrompt: doc.Memory.AgentPrompt,
		Rooms: s.channelRooms(in.GuildID, in.ChannelID), MemoryOn: memoryOn, Tools: toolset, Voice: voice,
		Ranking: rankingFor(in.Ranking, s.retrieveMode()), PlannerModel: "planner", LoopModel: doc.Chat.LoopRole(), ResponseModel: "response", TZ: tz,
		BudgetUSD: parseUSD(doc.Memory.TurnBudgetUSD, 0.05), Earlier: in.Earlier,
		Guard:      &chat.Guard{MaxUSD: parseUSD(doc.Limits.PerRequestBudgetUSD, 0), MaxCalls: doc.Limits.MaxExternalAttempts},
		MaxHistory: maxHistory, ToolMode: s.toolMode(), LoopFinal: doc.Chat.LoopFinalMode(), RoomMemory: roomMemory, People: people, Images: images, TurnStart: in.started,
		OnProgress: in.OnProgress, // nil-safe in the runtime; a lab or dry-run turn passes it on when set
	})
	if in.sourceRefs != nil && res.Room != nil && res.Room.Cards > 0 {
		*in.sourceRefs = append(*in.sourceRefs, personSources...)
	}
	noteTurnTiming(&res, "queue_wait", queueWait)
	res.CostUSD += conv.costUSD + explicitCost + visionCost
	if in.vision != nil && in.vision.Message != "" {
		in.Message = in.vision.Message // a description made while answering joins the kept message
	}
	if res.ScopeDropped > 0 {
		// never expected: search keeps to the channel's rooms. The hits were dropped before the prompt;
		// the operator hears of it because it means a defect in the isolation between rooms
		s.Log.ErrorContext(ctx, "memory hits outside the channel's rooms were dropped before the prompt", "dropped", res.ScopeDropped, "rooms", len(s.channelRooms(in.GuildID, in.ChannelID)))
		s.queueAlert(alertMemoryScope, map[string]any{"n": res.ScopeDropped})
	}
	if err != nil {
		return res, err
	}
	res.Answer = s.safeAnswer(res.Answer, voice, chat.ComposeSystem(voice, persona, rules, doc.Memory.AgentPrompt))
	said := res.Answer
	res.TurnID = in.turnID
	if in.vision != nil {
		s.notePartialVision(&in, in.vision)
	}
	s.decideThreadOffer(in, pol, res, in.context)
	var firstSend, deliverEnd time.Time
	var rejected atomic.Bool
	if in.Deliver != nil {
		s.delivering.Store(in.turnID, &rejected)
		defer s.delivering.Delete(in.turnID)
		said, firstSend, deliverEnd = deliverTimed(ctx, in, res)
		if !firstSend.IsZero() && !in.started.IsZero() {
			noteTurnTiming(&res, "first_send", max(int(firstSend.Sub(in.started).Milliseconds()), 0))
		}
	}
	if strings.TrimSpace(said) != "" && !in.Proactive && !in.banter {
		s.openFollowWindow(in, said, pol, in.FollowUp)
		s.implicitNoteReply(in)
	}
	if in.banter && in.Deliver != nil && strings.TrimSpace(said) != "" {
		s.noteBanter(in.GuildID, in.ChannelID, "line", in.turnID)
	}
	// a reply with no text (a picture the tools posted) keeps the request in the conversation as before
	kept := in.Deliver == nil || strings.TrimSpace(said) != "" || strings.TrimSpace(res.Answer) == ""
	callAddress := store.CallAccepted
	if kept && !in.DryRun && !in.Proactive && !in.banter && in.MessageID != "" {
		if s.liveToBot(in) {
			callAddress = store.CallReply
		}
	}
	// a person who withdrew consent to being remembered is not kept in the chat context either
	if !in.DryRun && s.MemoryAllowed(in.SpeakerID) && kept {
		requestAt, answerAt, spans := s.spokenTimes(in, firstSend, deliverEnd, said)
		if rejected.Load() {
			// The text was delivered, but the registration transaction lost its
			// boundaries. Do not offer any of it before delivery completed.
			answerAt, spans = deliverEnd, ""
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
		committed, err := eng.Store.AppendDecidedChatTurns(key, func() []store.ChatTurn { return s.flights.toKeep(in.flight, turns) }, chatTurnsKept)
		if err != nil {
			msg := "could not keep delivered conversation"
			if committed {
				msg = "kept delivered conversation but could not prune older turns"
			}
			s.Log.WarnContext(ctx, msg, "turn", in.turnID, "err", err)
		}
		if committed && len(turns) == 2 && in.Deliver != nil && !rejected.Load() && in.MessageID != "" {
			s.linkPostedReply(in.turnID, in.MessageID, key)
		}
	}
	s.logTurn(in, res)
	if !deliverEnd.IsZero() {
		noteTurnTiming(&res, "post_send", int(time.Since(deliverEnd).Milliseconds()))
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
