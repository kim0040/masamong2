package application

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/lang"
	"masamong/internal/memory/blocks"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
)

// The stages of a chat turn before its answer (chatTurn runs them in this order): wait for the channel,
// admit the turn, open it, read the conversation, pick the tools, write the rules, read room memory.

// turnRun is what one chat turn's stages share: the configuration it opened with and what it read and
// decided before the answer.
type turnRun struct {
	doc   config.Document
	eng   *engine.Engine
	pol   config.ChannelPolicy
	voice chat.Voice
	llm   chatLLM
	key   string // the channel's conversation key (blocks.ChannelKey)
	room  string // the channel's memory room (engine.RoomKey)

	// the conversation read (readTurnContext)
	windowAll  []string // every source the recent turns read
	ambient    []string // the recent lines' observation IDs
	history    []chat.Turn
	recent     []corpus.Message
	maxHistory int
	conv       convContext

	// the answer's setup
	toolset       chat.Toolset
	memoryOn      bool
	tz            *time.Location
	rules         string
	persona       string
	roomMemory    *chat.RoomMemory
	people        func([]string) *chat.RoomMemory
	personSources []string
	explicitCost  float64
	visionCost    float64
}

// enterTurn waits for the turn's place in its channel. Channels answer in parallel, one turn at a time
// within a channel, at most limits.max_concurrent_turns at once. The limits and budget are checked after
// the wait so they see what the turns ahead of this one spent. queueWait is the wait in milliseconds.
func (s *Service) enterTurn(ctx context.Context, in ChatInput) (release func(), queueWait int, err error) {
	waitCtx, stopWait := ctx, context.CancelFunc(func() {})
	if in.MaxWait > 0 {
		waitCtx, stopWait = context.WithTimeout(ctx, in.MaxWait)
	}
	queued := time.Now()
	release, err = s.turns.enter(waitCtx, blocks.ChannelKey(in.GuildID, in.ChannelID), s.Doc().Limits.ConcurrentTurns())
	queueWait = int(time.Since(queued).Milliseconds())
	stopWait()
	if err != nil {
		if ctx.Err() == nil && errors.Is(err, context.DeadlineExceeded) {
			err = ErrChatBusy // waited too long: the message is stale, the person is told to ask again
		}
		return nil, queueWait, err
	}
	return release, queueWait, nil
}

// admitTurn decides whether the turn is answered: a name alone goes through the name-call gate (which may
// make it a banter line), then every turn through the channel's gate.
func (s *Service) admitTurn(ctx context.Context, in *ChatInput) error {
	if in.NameOnly && !in.DryRun && !s.Paused(in.GuildID, in.ChannelID) {
		// the classifier is a model call and a banter emoji skips the reply: neither where replies are
		// off or the budget is spent (a name alone gets no budget notice)
		if err := s.chatOpen(*in); err != nil {
			if errors.Is(err, ErrChatBudget) {
				err = ErrNotAddressed
			}
			return err
		}
		banter, err := s.nameCallGate(ctx, *in)
		if err != nil {
			return err
		}
		in.banter = banter
		if in.Addressed != nil {
			in.Addressed()
		}
	}
	return s.chatGate(*in)
}

// openTurn reads the configuration the turn runs with: the document, the memory engine, the channel's
// policy and voice, and the model routes. A picture alone becomes the request's text.
func (s *Service) openTurn(in *ChatInput) (*turnRun, error) {
	doc := s.Doc()
	eng, err := s.MemoryEngine()
	if err != nil {
		return nil, err
	}
	pol := s.channelPolicy(in.GuildID, in.ChannelID)
	voice := s.voiceFor(in.GuildID, pol)
	voice.Pack = lang.For(chat.DetectLanguage(in.Message), in.Locale, voice.Pack.Tag)
	if in.imageOnly && strings.TrimSpace(in.Message) == "" {
		in.Message = voice.Pack.VisionText().Attached // a picture alone: the request is the picture
	}
	routes, err := s.chatRoutes(doc)
	if err != nil {
		return nil, err
	}
	return &turnRun{doc: doc, eng: eng, pol: pol, voice: voice, llm: chatLLM{s: s, routes: routes},
		key: blocks.ChannelKey(in.GuildID, in.ChannelID), room: engine.RoomKey(in.GuildID, in.ChannelID)}, nil
}

// readTurnContext reads the conversation the answer is built on: the caller's own conversation where the
// channel attributes one, else the channel's plain history and recent lines. It records what the reply is
// built on in in.sourceRefs.
//
// The window is more than a prompt shows. What reaches the prompt is read; a turn further back is held by its link
// alone, as is every turn in a channel that shows each caller only their conversation: a withdrawal follows the
// link, and a deleted message takes only the replies that read it (store.ChatReaders). No prompt holds a row before
// the last chat.HistoryShown, whichever rows are empty or long: every prompt built from this history keeps to them,
// and a caller's conversation records each line it shows (conv.sources).
func (s *Service) readTurnContext(ctx context.Context, in *ChatInput, t *turnRun) {
	var windowShown, windowLinks []string
	cutoff := s.callCutoff(*in)
	turns, readErr := t.eng.Store.RecentChatTurns(t.key, 16)
	if readErr == nil {
		t.windowAll, windowShown, windowLinks = chatWindow(turns, t.room)
	}
	t.recent = recentLines(t.eng, t.room, in.Message)
	t.ambient = make([]string, 0, len(t.recent))
	for _, m := range t.recent {
		t.ambient = append(t.ambient, m.ObservationID)
	}
	conv, attributed := s.callerConversation(ctx, *in, t.eng, t.pol, t.voice, t.llm)
	t.conv = conv
	if attributed {
		// the caller's conversation replaces the channel's plain history and recent lines: the other
		// conversations of the room are not shown to the planner or the answer at all
		t.history, t.recent, t.maxHistory = conv.history, nil, 80
		if in.sourceRefs != nil {
			*in.sourceRefs = append(append(*in.sourceRefs, windowLinks...), conv.sources...)
		}
		for k, v := range conv.ledger {
			if in.context != nil {
				in.context[k] = v
			}
		}
		return
	}
	// Only plain history needs these timestamps; attribution resolves its own larger window.
	if readErr == nil {
		t.history = s.plainHistory(*in, turns, cutoff)
	}
	// the recent lines reach the reply only here (RecentRoom), so only here is it built on them. With the
	// caller's conversation in their place it is built on the lines that conversation picked (conv.sources). A
	// line offered to the reference classifier and not picked leaves nothing: it answers with labels only.
	if in.sourceRefs != nil {
		*in.sourceRefs = append(append(append(*in.sourceRefs, windowLinks...), windowShown...), t.ambient...)
	}
}

// chatWindow is what the channel's recent turns read: all of it, what the last chat.HistoryShown of them
// show, and each turn's own link.
func chatWindow(turns []store.ChatTurn, room string) (all, shown, links []string) {
	for i, t := range turns {
		read := t.SourceIDs
		if t.Role == "user" && t.NativeID != "" {
			read = append(slices.Clone(read), corpus.DiscordLiveObservationID(room, t.NativeID))
		}
		all = append(all, read...)
		links = append(links, t.SourceIDs[0]) // its own link comes first
		if i >= len(turns)-chat.HistoryShown {
			shown = append(shown, read...)
		}
	}
	return all, shown, links
}

// recentLines is the room's latest twelve captured lines before the message being answered (the latest one
// captured with its words; an earlier message with the same words, someone else's "ㅇㅇ", stays).
func recentLines(eng *engine.Engine, room, message string) []corpus.Message {
	var recent []corpus.Message
	msgs, err := eng.Store.RecentMessages(room, 13)
	if err != nil {
		return nil
	}
	dropped := false
	for i := len(msgs) - 1; i >= 0; i-- {
		if !dropped && msgs[i].Content == strings.TrimSpace(message) {
			dropped = true
			continue
		}
		recent = append(recent, msgs[i])
	}
	slices.Reverse(recent)
	if len(recent) > 12 {
		recent = recent[len(recent)-12:]
	}
	return recent
}

// plainHistory is the channel's recent turns as the prompt shows them, each with the time it was spoken
// and only the part of a reply already delivered at the call's cutoff.
func (s *Service) plainHistory(in ChatInput, turns []store.ChatTurn, cutoff time.Time) []chat.Turn {
	var history []chat.Turn
	_ = s.timeChatTurns(in, turns) // fills the spoken times in place; a turn it cannot time keeps its stored time
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
	return history
}

// turnTools picks the tools the turn may use and whether it searches memory. A banter line uses none.
func (s *Service) turnTools(ctx context.Context, in ChatInput, t *turnRun) {
	t.toolset = s.chatToolset(t.voice, in.GuildID, in.ChannelID)
	_, memTool := toolsetHas(t.toolset, "memory.search")
	t.memoryOn = t.pol.MemoryUse != "off" && t.doc.Memory.Retrieve != "off" && memTool
	if in.banter {
		// one light line back: nothing looked up, nothing run
		t.memoryOn, t.toolset = false, chat.Toolset{Known: map[string]bool{}}
	}
	if t.memoryOn {
		if err := t.eng.EnsureLoaded(); err != nil {
			s.Log.WarnContext(ctx, "memory reload failed", "err", err)
		}
	}
}

// turnRuntime writes the turn's rules and persona and builds the runtime that answers it.
func (s *Service) turnRuntime(in ChatInput, t *turnRun) *chat.Runtime {
	t.tz = s.instanceZone()
	rules := draftOr(in, in.DraftRules, t.pol.Rules)
	if in.Proactive {
		rules = strings.TrimSpace(rules + "\n\n" + t.voice.Pack.Features.ProactiveRules)
	}
	if in.banter {
		rules = strings.TrimSpace(rules + "\n\n" + t.voice.Pack.Conversation.BanterRules)
	}
	if note := s.roomStyleRules(in.GuildID, in.ChannelID, t.pol, t.voice.Pack); note != "" && !in.DryRun {
		rules = strings.TrimSpace(rules + "\n\n" + note)
	}
	t.rules = rules
	mem := gatedMemory{s: s, eng: t.eng, inv: &action.Invoker{Via: "chat", GuildID: in.GuildID, ChannelID: in.ChannelID, UserID: in.SpeakerID}, noWarm: !s.warmsQueries(t.doc, in)}
	rt := &chat.Runtime{LLM: t.llm, Memory: mem, Tools: s.chatTool(in, t.voice)}
	t.persona = draftOr(in, in.DraftStyle, s.personaText(personaFor(in, t.pol.PersonaRef)))
	return rt
}

// turnMemory reads room memory (roommemory_read.go) and answers an explicit memory request (explicit.go);
// the explicit answer joins the turn's earlier results.
func (s *Service) turnMemory(ctx context.Context, in *ChatInput, t *turnRun) {
	roomIn := *in
	if in.sourceRefs != nil {
		// what room memory takes as already known in the room does not depend on what this reply is built on
		known := append(append(slices.Clone(*in.sourceRefs), t.windowAll...), t.ambient...)
		roomIn.sourceRefs = &known
	}
	t.roomMemory, t.people = s.turnRoomMemory(ctx, roomIn, t.pol, t.voice, t.history, t.recent, &t.personSources)
	explicit, explicitCost := s.explicitMemoryTurn(ctx, *in, t.voice, t.llm, t.history)
	t.explicitCost = explicitCost
	if explicit != nil {
		in.Earlier = append(in.Earlier, *explicit)
	}
}

// turnRequest is the request the runtime answers.
func (s *Service) turnRequest(in ChatInput, t *turnRun, images []chat.Image) chat.Request {
	doc := t.doc
	return chat.Request{
		Message: in.Message, Speaker: in.Speaker, History: t.history, RecentRoom: t.recent,
		Persona: t.persona, Rules: t.rules, AgentPrompt: doc.Memory.AgentPrompt,
		Rooms: s.channelRooms(in.GuildID, in.ChannelID), MemoryOn: t.memoryOn, Tools: t.toolset, Voice: t.voice,
		Ranking: rankingFor(in.Ranking, s.retrieveMode()), PlannerModel: "planner", LoopModel: doc.Chat.LoopRole(), ResponseModel: "response", TZ: t.tz,
		BudgetUSD: parseUSD(doc.Memory.TurnBudgetUSD, 0.05), Earlier: in.Earlier,
		Guard:      &chat.Guard{MaxUSD: parseUSD(doc.Limits.PerRequestBudgetUSD, 0), MaxCalls: doc.Limits.MaxExternalAttempts},
		MaxHistory: t.maxHistory, ToolMode: s.toolMode(), LoopFinal: doc.Chat.LoopFinalMode(), RoomMemory: t.roomMemory, People: t.people, Images: images, TurnStart: in.started,
		OnProgress: in.OnProgress, // nil-safe in the runtime; a lab or dry-run turn passes it on when set
	}
}
