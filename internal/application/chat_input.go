package application

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/errclass"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/search"
	"masamong/internal/providers/openai"
	"masamong/internal/tools"
	"masamong/internal/vision"
)

// ChatInput is one message addressed to the bot.
type ChatInput struct {
	GuildID, ChannelID string
	MessageID          string // the Discord message being answered (its edit or deletion follows the turn)
	Message            string
	Locale             string // per-interaction locale, used when message text does not identify a language
	Speaker, SpeakerID string
	DryRun             bool   // do not store the turn in the channel conversation (GUI test, RAG lab)
	Ranking            string // optional retrieval mode override (RAG lab comparisons)
	PersonaRef         string // optional persona override for lab turns (persona preview)
	// Draft settings a lab turn tries before they are saved (the channel page's test panel): a persona
	// style text and the channel's extra rules. Live turns never use them.
	DraftStyle *string
	DraftRules *string
	// Proactive is a reply nobody asked for (participation): the pack's proactive rules keep it short.
	Proactive bool
	// NameOnly marks a message that said the bot's name without mentioning or replying to it. With the
	// channel's name_call set to addressed, the turn first asks a classifier whether it was said to the
	// bot or about it, and stays silent (ErrNotAddressed) unless it was said to the bot.
	NameOnly bool
	// FollowUp marks an untagged message the follow-up window decided to answer (followup.go).
	FollowUp bool
	banter   bool // a playful mention answered back with one line (banter.go)
	// Addressed is called once a NameOnly turn is found to be said to the bot (the gateway starts
	// typing then, not before the bot knows it will answer).
	Addressed func()
	// OnRefusal reports a chat refusal when an unanswered command keeps its original card.
	OnRefusal func(error)
	// Earlier is a command's result that had no answer (see CommandToChat).
	Earlier []chat.ToolResult
	// Deliver, when set, posts the reply while the turn still holds its channel's place and returns
	// the text that reached the channel ("" when nothing did). Only that text is kept in the channel
	// conversation: a reply that was withheld or failed to send is not one the next turn builds on.
	// Without it the generated reply is kept as said (the caller posts it afterwards).
	Deliver func(ctx context.Context, res chat.Result) string
	// Images are the pictures the bot was called with: attached to the message, to the message it
	// replies to, or forwarded in it (vision.go). Only references: the turn downloads them, with limits,
	// when chat.vision is on, and never stores them.
	Images []vision.Ref
	// ImageTotal and ImageSkipped describe supported, unique pictures the connector found before its
	// selection limit. They carry counts and reason classes only; image references remain in Images.
	ImageTotal   int
	ImageSkipped map[string]int
	// OnVisionNotice receives a localized omission line after preparation; the connector says it as the
	// first line of the delivered reply (never in a model prompt). It is called once, from the turn's final
	// outcome, and only when some pictures were read and some were not.
	OnVisionNotice func(string)
	// OnProgress hears how far the turn's web search has got (chat.Request.OnProgress); the connector may
	// post a short "still looking" line and delete it before the answer. It must not block. nil = none.
	OnProgress func(chat.Progress)
	// ImageOnly marks a message that said nothing but the call: the pictures are the request. When no
	// model can look at them, the member gets a short honest line instead of an answer.
	ImageOnly bool
	// ImageEarlier marks pictures posted just before the message rather than with it (the connector
	// looked back for them); the answer is told so, and to leave them out when the message is not about
	// them.
	ImageEarlier bool
	imageOnly    bool
	vision       *visionState
	// turnID is the id ChatTurn gives the turn up front, so Deliver can tie the posted reply to it.
	turnID     string
	sourceRefs *[]string
	// context is the body-free record of how the turn's conversation was chosen (attribution)
	context map[string]any
	// MaxWait bounds the wait for the channel's turn and a model slot; a turn that waited longer is
	// stale and is refused with ErrChatBusy. TurnTimeout bounds the turn itself and starts after that
	// wait, so a queue does not use up the turn's time. 0 for either: no limit of its own.
	MaxWait, TurnTimeout time.Duration
	// started is when ChatTurn began, the zero point of the turn's timings and spans (turn_timing.go).
	started time.Time
	// clockStart is the turn's start on the service clock, for the edit-ordering check in liveToBot:
	// a RevisedAt stamped after it keeps the line unmarked. Timings and spans stay on started's wall
	// time; the two clocks can disagree.
	clockStart time.Time
	// Fragments are the other messages of a burst answered as one turn, and of a call a picture continues: Message
	// holds their words. MessageID alone would leave those words behind when one of them is deleted or edited.
	Fragments []string
	// ReadFrom is the SettledMark the connector took before it ran the command whose result is in Earlier, or
	// chose the pictures in Images (Service.SettledMark): an edit settled by then is one they could not have seen
	// the old state of. 0 excuses nothing.
	ReadFrom int64
	// readAsOf is the SettledMark taken before the turn read anything (store.SettleSources). A turn that came with
	// something read before it started has no later number than the one its caller took for that (ReadFrom).
	readAsOf int64
	// flight learns of changes to the messages the turn holds while it runs (turnFlights).
	flight *turnFlight
}

// chatRoute is the model and provider behind one chat role.
type chatRoute struct {
	model, provider string
	a               *openai.Adapter
	timeout         time.Duration // models.<role>.timeout_seconds; 0 = chatDeadline
	effort          string        // models.<role>.effort; "" = low
}

// callEffort is the reasoning effort of the route's calls: the configured one, else low. A quick
// classifier call asks for none instead, unless the role's effort is set (openai.ChatRequest.Quick).
func (r chatRoute) callEffort() string {
	if r.effort != "" {
		return r.effort
	}
	return "low"
}

// chatLLM is the chat.LLM port; the runtime's "model" argument is the role ("planner" | "response").
type chatLLM struct {
	s      *Service
	routes map[string]chatRoute
}

// Chat asks the role's model. When it fails (the model is gone or overloaded, the host times out, a
// reasoning model spends its whole budget thinking), the other chat role's model gets the same
// request once, if it is a different model: a failed intent model otherwise leaves the turn without
// its tools ("I can't see the weather"), a failed answer model without a reply. The previous bot hit
// both often enough to add a fallback lane; this one needs no extra configuration.
func (c chatLLM) Chat(ctx context.Context, role string, msgs []chat.Msg, maxTokens int, temperature float64) (string, float64, error) {
	out, err := c.ChatDetailed(ctx, role, msgs, maxTokens, temperature)
	return out.Text, out.CostUSD, err
}

func (c chatLLM) ChatDetailed(ctx context.Context, role string, msgs []chat.Msg, maxTokens int, temperature float64) (chat.Completion, error) {
	r, ok := c.routes[role]
	if !ok || r.a == nil {
		return chat.Completion{}, fmt.Errorf("no adapter for the %s role", role)
	}
	if hasImages(msgs) {
		return c.chatWithImages(ctx, role, msgs, maxTokens, temperature) // vision.go
	}
	out, err := c.ask(ctx, r, role, msgs, maxTokens, temperature, false)
	if err == nil || ctx.Err() != nil { // the turn's own deadline: no time left to ask another model
		return out, err
	}
	other := map[string]string{"planner": "response", "response": "planner"}[role]
	alt, ok := c.routes[other]
	if !ok || alt.a == nil || (alt.model == r.model && alt.provider == r.provider) {
		return out, err
	}
	c.s.Log.WarnContext(ctx, "chat: model failed, the other chat model answers", "role", role, "model", r.model, "instead", alt.model, "err", err)
	out2, err2 := c.ask(ctx, alt, role, msgs, maxTokens, temperature, true)
	if err2 != nil {
		out2.CostUSD += out.CostUSD
		return out2, fmt.Errorf("%w (then %s: %v)", err, alt.model, err2)
	}
	out2.CostUSD += out.CostUSD
	return out2, nil
}

// chatDeadline is how long one model call of a role may take before the other chat model is asked,
// unless the model sets models.<role>.timeout_seconds. A host that stops answering otherwise holds the
// turn until the HTTP client gives up (120 s): the scenario runs on CometAPI saw intent calls hang for
// 125 s while the answer itself took 3 s. The defaults leave room for a model that thinks first.
// The table is never changed; a test shortens a role's limit for its own service (roleDeadline).
var chatDeadline = map[string]time.Duration{"planner": 30 * time.Second, "response": 90 * time.Second, "vision": 30 * time.Second}

// roleDeadline is chatDeadline's limit for role, or the one a test set for this service.
func (s *Service) roleDeadline(role string) (time.Duration, bool) {
	if m := s.hooks.chatDeadlines.Load(); m != nil {
		if d, ok := (*m)[role]; ok {
			return d, true
		}
	}
	d, ok := chatDeadline[role]
	return d, ok
}

// quickKey marks a small classifier call made under ctx (a JSON line of tens of tokens).
type quickKey struct{}

// withQuickAnswer asks the model to answer without thinking. On the reference and name-call prompts
// (2026-10-14, openai/gpt-6-luna, 60 runs each) "low" thought for 2.8 s (p90 6.1 s) and often spent the
// 60-80 token budget, costing a second call; "none" answered in 1.3 s (p90 1.9 s), the name call 36/36
// against 32/36 and the reference within a few picks (it names fewer lines, never another topic).
func withQuickAnswer(ctx context.Context) context.Context {
	return context.WithValue(ctx, quickKey{}, true)
}

func quickAnswer(ctx context.Context) bool {
	q, _ := ctx.Value(quickKey{}).(bool)
	return q
}

// ask makes one call to r's model; fallback is true when it is the other chat role's model standing in
// (the call ledger tells the two apart).
func (c chatLLM) ask(ctx context.Context, r chatRoute, role string, msgs []chat.Msg, maxTokens int, temperature float64, fallback bool) (chat.Completion, error) {
	d, ok := c.s.roleDeadline(role)
	if r.timeout > 0 {
		d, ok = r.timeout, true // the model's own limit, also when it stands in for the other role
	}
	if ok {
		var cancel context.CancelFunc
		// with fallbacks each model gets its own limit inside the adapter; the whole chain gets their sum
		ctx, cancel = context.WithTimeout(ctx, chainTimeout(r.a, d))
		defer cancel()
	}
	om := make([]openai.Message, len(msgs))
	for i, m := range msgs {
		om[i] = openai.Message{Role: m.Role, Content: m.Content, Images: imageParts(m.Images), CachePoint: m.CachePoint}
	}
	t := temperature
	effort, quick := r.callEffort(), quickAnswer(ctx)
	if quick {
		effort = "none" // the adapter asks for the role's level instead when one is set (RoleEffort)
	}
	began := time.Now()
	end := chat.GuardFrom(ctx).Begin()
	cost := 0.0
	defer func() { end(cost) }()
	res, err := r.a.Chat(ctx, openai.ChatRequest{Model: r.model, Messages: om, MaxTokens: maxTokens, Temperature: &t, Effort: effort, Quick: quick})
	class := errclass.Of(err)
	if err == nil && strings.TrimSpace(res.Content) == "" {
		class = errclass.Empty
		// an empty reply is a failed call, so the other chat model is asked: a planner that answers
		// nothing otherwise leaves the turn without tools ("다음 주 월요일 비 와?" answered with "ask me
		// later"). A reasoning model can also spend the whole output budget thinking.
		err = fmt.Errorf("%s model returned no text (finish_reason=%s)", role, res.FinishReason)
		if res.FinishReason == "length" {
			err = fmt.Errorf("%s model returned no text: output budget of %d tokens used up (finish_reason=length)", role, maxTokens)
		}
	}
	if res.CostUSD != nil {
		cost = *res.CostUSD
	}
	// a failed call counts as a call even when the provider reported no cost (a timed-out call may
	// still be billed; its cost is not known here)
	if eng, e2 := c.s.MemoryEngine(); e2 == nil {
		_ = eng.Store.AddSpend("chat", cost, 1)
	}
	end(cost)
	// with fallbacks, the model that answered (or failed last) is the one the ledger and health name
	served := r
	if n := len(res.Attempts); n > 0 && res.Provider != "" {
		served.provider, served.model = res.Provider, res.Attempts[n-1].Model
	}
	if errors.Is(err, openai.ErrVisionUnsupported) {
		// a model that takes no pictures is not a provider in trouble; the turn describes them instead
		c.s.visionRefused.Store(visionKey(r), c.s.Clock.Now())
	} else {
		c.s.noteProvider(served.provider, err)
	}
	c.s.noteLLMCall(ctx, llmCall{Role: ledgerRole(ctx, role), Model: served.model, Provider: served.provider, FinishReason: res.FinishReason, PromptTokens: res.PromptTokens,
		CompletionTokens: res.CompletionTokens, CachedTokens: res.CachedTokens, Latency: time.Since(began), CostUSD: cost, Class: class, Fallback: fallback || res.Fallback})
	return chat.Completion{Text: res.Content, CostUSD: cost, FinishReason: res.FinishReason}, err
}

// chatRoutes are the chat roles' models. Routes are keyed by role, not by model name: the planner and the
// response role may name the same model on different providers. A missing planner role uses the response
// model (as the engine does).
func (s *Service) chatRoutes(doc config.Document) (map[string]chatRoute, error) {
	routes := map[string]chatRoute{}
	for _, role := range []string{"response", "planner"} {
		src := role
		if _, ok := modelOf(doc, role); !ok && role == "planner" {
			src = "response"
		}
		m, ok := modelOf(doc, src)
		if !ok {
			return nil, fmt.Errorf("models.%s not configured", src)
		}
		a, err := s.llmAdapterFor(doc, src)
		if err != nil {
			return nil, err
		}
		routes[role] = chatRoute{model: m.Model, provider: m.Provider, a: a, timeout: time.Duration(m.TimeoutSeconds) * time.Second, effort: m.Effort}
	}
	// the optional vision role describes pictures first (vision.go); a vision model that cannot be
	// reached (no key, a provider since removed) is left out, and pictures fall back to the chat models
	if m, ok := modelOf(doc, "vision"); ok {
		if a, err := s.visionAdapter(doc); err == nil && a.Configured() {
			routes["vision"] = chatRoute{model: m.Model, provider: m.Provider, a: a, timeout: time.Duration(m.TimeoutSeconds) * time.Second, effort: m.Effort}
		}
	}
	return routes, nil
}

func (s *Service) personaText(ref string) string {
	if p, ok := s.Doc().Personas[ref]; ok && p.Style != "" {
		return p.Style
	}
	if p, ok := config.BuiltinPersonas()[ref]; ok {
		return p.Style
	}
	return ""
}

// Chat turn refusals. In Discord a refusal the person can act on still gets a short notice (a reply
// limit or the monthly model budget); the rest stay silent so a refused turn never spams the channel.
var (
	ErrChatDisabled = errors.New("chat: replies are turned off (tools.chat.respond)")
	ErrChatLimited  = errors.New("chat: reply limit reached (tools.chat.respond.limits)")
	ErrChatBudget   = errors.New("chat: monthly model budget reached (limits.monthly_budget_usd)")
	// ErrNotAddressed: the message only talked about the bot (name_call: addressed).
	ErrNotAddressed = errors.New("chat: the bot was mentioned by name, not addressed")
)

// chatGate applies the chat.respond switch and limits and the monthly model budget.
func (s *Service) chatGate(in ChatInput) error {
	if err := s.chatOpen(in); err != nil {
		return err
	}
	if in.GuildID == "" && !in.DryRun {
		return s.reserveDM(in)
	}
	if r, ok := tools.Find(s.toolRegistry(), "chat.respond"); ok {
		inv := &action.Invoker{Via: "reply", GuildID: in.GuildID, ChannelID: in.ChannelID, UserID: in.SpeakerID}
		if refused := s.reserveTool(action.Request{Action: "chat.respond", Invoker: inv}, r); refused != nil {
			return fmt.Errorf("%w: %s", ErrChatLimited, refused.Error.Args["scope"])
		}
	}
	return nil
}

// chatOpen is chatGate without taking a turn from the limits: paused, switched off here, or over the
// monthly budget.
func (s *Service) chatOpen(in ChatInput) error {
	if !in.DryRun && s.Paused(in.GuildID, in.ChannelID) {
		return ErrChatPaused
	}
	if r, ok := tools.Find(s.toolRegistry(), "chat.respond"); ok {
		inv := &action.Invoker{Via: "reply", GuildID: in.GuildID, ChannelID: in.ChannelID, UserID: in.SpeakerID}
		if !r.Enabled {
			return ErrChatDisabled
		}
		if allowed, _ := s.toolAllowedHere(r, inv); !allowed {
			return ErrChatDisabled
		}
	}
	if budget := parseUSD(s.Doc().Limits.MonthlyBudgetUSD, 0); budget > 0 {
		if _, spent, ok := s.monthSpend(); ok && spent >= budget {
			return ErrChatBudget
		}
	}
	return nil
}

// gatedMemory applies memory.search's tool limits to chat-time memory lookups.
type gatedMemory struct {
	s      *Service
	eng    *engine.Engine
	inv    *action.Invoker
	noWarm bool // the message's embedding waits for the plan (warmsQueries)
}

// WarmQuery starts the embedding of the turn's message ahead of the plan (chat.QueryWarmer). It takes
// nothing from memory.search's tool limits: only a search does.
func (g gatedMemory) WarmQuery(ctx context.Context, query string) {
	if !g.noWarm {
		g.eng.WarmQuery(ctx, query)
	}
}

// warmsQueries reports whether a turn may embed its message before the plan says it needs it. Not in a
// dry-run or practice turn, which have no reply to hurry; not on a provider with a daily_limit, because most
// of these calls end up not needed and would use up a limit the operator set for the calls that are; and not
// for a person who withdrew consent to being remembered, whose words leave only when a search needs them.
// memory.warm_query: off turns it off everywhere; it is read from the document of the turn, so a change
// applies to the next turn without a restart.
func (s *Service) warmsQueries(doc config.Document, in ChatInput) bool {
	return doc.Memory.WarmQueryOn() && !in.DryRun && !s.practice && doc.Providers[doc.Models["embed"].Provider].DailyLimit == 0 && s.MemoryAllowed(in.SpeakerID)
}

func (g gatedMemory) Search(ctx context.Context, req engine.SearchRequest) (search.Result, error) {
	if r, ok := tools.Find(g.s.toolRegistry(), "memory.search"); ok {
		if refused := g.s.reserveTool(action.Request{Action: "memory.search", Invoker: g.inv}, r); refused != nil {
			return search.Result{}, errors.New("memory.search limit reached")
		}
	}
	res, _, err := g.s.reviewedSearch(ctx, g.eng, req)
	return res, err
}

// draftOr is a lab turn's draft value when it has one, else the saved one.
func draftOr(in ChatInput, draft *string, saved string) string {
	if in.DryRun && draft != nil {
		return *draft
	}
	return saved
}

// personaFor lets a lab turn try another persona (preview); live turns always use the room's.
func personaFor(in ChatInput, room string) string {
	if in.DryRun && in.PersonaRef != "" {
		return in.PersonaRef
	}
	return room
}
