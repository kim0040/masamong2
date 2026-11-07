package application

import (
	"cmp"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/crash"
	"masamong/internal/errclass"
	"masamong/internal/errcode"
	"masamong/internal/lang"
	"masamong/internal/memory/blocks"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
	"masamong/internal/observability"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
	"masamong/internal/storage"
	"masamong/internal/tools"
	"masamong/internal/vision"
)

func toolsetHas(ts chat.Toolset, id string) (chat.ToolInfo, bool) {
	for _, t := range ts.Enabled {
		if t.ID == id {
			return t, true
		}
	}
	return chat.ToolInfo{}, false
}

// memoryState caches the engine per config revision (models/settings come from config).
type memoryState struct {
	mu  sync.Mutex
	eng *engine.Engine
	rev int
	key string // memoryConfigKey the engine was built from
}

// memoryConfigKey is everything MemoryEngine builds the engine from. A config change that leaves it
// the same (a channel's rules, a tool limit, a persona...) keeps the engine and its loaded index; with
// a long history a rebuild takes seconds and briefly holds a second copy of the index in memory.
func memoryConfigKey(doc config.Document) string {
	used := map[string]config.Provider{}
	keys := map[string]string{}
	for _, m := range doc.Models {
		if p, ok := doc.Providers[m.Provider]; ok {
			used[m.Provider] = p
			// a key replaced under the same reference (keychain:/file:) must reach the engine's adapter;
			// only a hash of it goes into the comparison
			if cred, err := providers.Resolve(p.CredentialRef); err == nil && cred.Present() {
				sum := sha256.Sum256([]byte(cred.Value()))
				keys[m.Provider] = hex.EncodeToString(sum[:8])
			}
		}
	}
	raw, _ := json.Marshal(struct {
		Memory    config.MemoryFlags
		Models    map[string]config.Model
		Providers map[string]config.Provider
		Keys      map[string]string
		TZ, Loc   string
		BotNames  []string
	}{doc.Memory, doc.Models, used, keys, doc.I18n.Timezone, doc.I18n.DefaultLocale, doc.BotNames()})
	return string(raw)
}

var memStates sync.Map // *Service → *memoryState

func (s *Service) memState() *memoryState {
	v, _ := memStates.LoadOrStore(s, &memoryState{})
	return v.(*memoryState)
}

func modelOf(d config.Document, role string) (config.Model, bool) {
	m, ok := d.Models[role]
	return m, ok && m.Model != ""
}

// llmAdapter returns an OpenAI-compatible adapter for the provider behind a model role, with a long
// timeout (answers and summaries can take tens of seconds; the shared registry client is 15s).
func (s *Service) llmAdapter(role string) (*openai.Adapter, error) {
	return s.llmAdapterFor(s.Doc(), role)
}

// quickEffort is the level a Quick call on an adapter of role asks for (openai.Adapter.RoleEffort): a
// call the code makes thinking-free on purpose (the name-call, reference and follow-up classifiers, a
// fortune). "" (none) while the role sets no effort; once it does, that level capped at low, so a planner
// at high or max does not make a 40-token classifier think hard. The summary role's own effort is the
// level of the job itself (every summary call is Quick) and applies as set.
func quickEffort(role, effort string) string {
	if role == "summary" || effort == "" || slices.Index(config.ModelEfforts, effort) <= slices.Index(config.ModelEfforts, "low") {
		return effort
	}
	return "low"
}

func (s *Service) llmAdapterFor(doc config.Document, role string) (*openai.Adapter, error) {
	m, ok := modelOf(doc, role)
	if !ok {
		return nil, fmt.Errorf("models.%s not configured", role)
	}
	p, ok := doc.Providers[m.Provider]
	if ok && p.Kind == config.KindPractice {
		return practiceAdapter(p.BaseURL, doc.I18n.DefaultLocale, 120*time.Second, s.Log), nil
	}
	if s.practice {
		return nil, fmt.Errorf("models.%s: practice mode only uses the offline practice model", role)
	}
	if !ok || p.Kind != config.KindOpenAI {
		return nil, fmt.Errorf("models.%s.provider %q is not an openai_compat provider", role, m.Provider)
	}
	cred, err := providers.Resolve(p.CredentialRef)
	if err != nil {
		return nil, err
	}
	client := providers.NewClient(120*time.Second, s.Log)
	client.MaxBody = 64 << 20
	a := newLLMAdapter(client, p, cred)
	a.Price, a.CachedPrice, a.ContextPrice = s.pricer(m.Provider), s.cachedPricer(m.Provider), s.contextPricer(m.Provider)
	a.Gate = s.llmGate(m.Provider)
	s.shareCaps(a, m.Provider, p)
	if slices.Contains(config.FallbackRoles, role) {
		// a Quick call of this role (a classifier, a one-line summary) thinks only when the operator set
		// the role's effort, and then at most at low (quickEffort; the precedence is on config.Model.Effort)
		a.RoleEffort = quickEffort(role, m.Effort)
	}
	if len(m.Fallbacks) > 0 && slices.Contains(config.FallbackRoles, role) {
		a.Failover = s.failoverFor(doc, role, m)
	}
	return a, nil
}

// batchResolver finds the provider a memory batch was submitted to (engine.BatchProvider: its config id,
// address and family) for the batch's polls, results, cancel and cleanup: the engine's own adapter when
// its role still uses that provider, else one built from the config by id. A provider that was removed,
// is not an OpenAI-compatible one any more, or now has another address or family cannot read the batch:
// the job stops with that error instead of polling a host that does not know it.
func (s *Service) batchResolver(doc config.Document, eng *engine.Engine) engine.BatchResolver {
	same := func(a *openai.Adapter, p engine.BatchProvider) bool {
		return a != nil && strings.TrimRight(a.BaseURL, "/") == strings.TrimRight(p.BaseURL, "/") && (p.API == "" || a.API == p.API)
	}
	return func(p engine.BatchProvider) (engine.BatchClient, error) {
		if p.ID == eng.CallProviders.Embed && same(eng.LLM, p) {
			return eng.LLM, nil
		}
		if eng.SummaryLLM != nil && p.ID == eng.CallProviders.Summary && same(eng.SummaryLLM, p) {
			return eng.SummaryLLM, nil
		}
		cp, ok := doc.Providers[p.ID]
		switch {
		case !ok:
			return nil, fmt.Errorf("provider %q is not configured any more", p.ID)
		case cp.Kind != config.KindOpenAI || s.practice:
			return nil, fmt.Errorf("provider %q is not an openai_compat provider any more", p.ID)
		}
		cred, err := providers.Resolve(cp.CredentialRef)
		if err != nil {
			return nil, fmt.Errorf("provider %q: %w", p.ID, err)
		}
		client := providers.NewClient(120*time.Second, s.Log)
		client.MaxBody = 64 << 20
		a := newLLMAdapter(client, cp, cred)
		a.Price, a.CachedPrice, a.ContextPrice = s.pricer(p.ID), s.cachedPricer(p.ID), s.contextPricer(p.ID)
		a.Gate = s.llmGate(p.ID)
		s.shareCaps(a, p.ID, cp)
		if !same(a, p) {
			return nil, fmt.Errorf("provider %q is now at %s (%s), not %s (%s)", p.ID, a.BaseURL, a.API, p.BaseURL, p.API)
		}
		return a, nil
	}
}

// roleTimeout is how long one call of role may take before its fallback is asked: the model's own
// limit, else chatDeadline's, else a minute (summaries, features).
func roleTimeout(role string, seconds int) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if d, ok := chatDeadline[role]; ok {
		return d
	}
	return time.Minute
}

// failoverFor builds role's fallback chain. A fallback whose provider cannot be reached now (no key, not
// an openai_compat host, disabled) is left out rather than failing the role.
func (s *Service) failoverFor(doc config.Document, role string, m config.Model) *openai.Failover {
	f := &openai.Failover{Provider: m.Provider, Model: m.Model, Timeout: roleTimeout(role, m.TimeoutSeconds), Breaker: s.breaker(),
		OnFail: func(provider, model string, err error) {
			s.Log.Warn("model failed, its fallback answers", "role", role, "provider", provider, "model", model, "class", errclass.Of(err))
			s.noteProvider(provider, err)
		}}
	for _, fb := range m.Fallbacks {
		p, ok := doc.Providers[fb.Provider]
		if !ok || p.Kind != config.KindOpenAI || p.Disabled {
			continue
		}
		cred, err := providers.Resolve(p.CredentialRef)
		if err != nil || !cred.Present() {
			continue
		}
		client := providers.NewClient(120*time.Second, s.Log)
		client.MaxBody = 64 << 20
		a := newLLMAdapter(client, p, cred)
		a.Price, a.CachedPrice, a.ContextPrice = s.pricer(fb.Provider), s.cachedPricer(fb.Provider), s.contextPricer(fb.Provider)
		a.Gate = s.llmGate(fb.Provider)
		s.shareCaps(a, fb.Provider, p)
		// the fallback's own effort, else the role's, for its Quick calls (capped as the role's are); its
		// other calls take fb.Effort when set (openai.Fallback.Effort), else the role's level the request carries
		a.RoleEffort = quickEffort(role, cmp.Or(fb.Effort, m.Effort))
		f.Fallbacks = append(f.Fallbacks, openai.Fallback{A: a, Provider: fb.Provider, Model: fb.Model, Timeout: roleTimeout(role, fb.TimeoutSeconds), Effort: fb.Effort})
	}
	return f
}

// quotaError is the operator's daily call limit for a model provider reached: class Quota, so the
// breaker rests the provider and a fallback answers; errors.Is(err, errQuota) still holds.
type quotaError struct{ err error }

func (e quotaError) Error() string            { return e.err.Error() }
func (e quotaError) Unwrap() error            { return e.err }
func (e quotaError) ErrClass() errclass.Class { return errclass.Quota }

// llmGate counts a model provider's calls against its daily_limit (providers.<id>.daily_limit; none
// set = counted, never refused), as the search and data services are.
func (s *Service) llmGate(provider string) func() error {
	return func() error {
		if err := s.Registry().reserve(provider); err != nil {
			return quotaError{err}
		}
		return nil
	}
}

// breaker is the service's model breaker, made on first use.
func (s *Service) breaker() *openai.Breaker {
	s.modelBreakerOnce.Do(func() { s.modelBreaker = openai.NewBreaker(func() time.Time { return s.Clock.Now() }) })
	return s.modelBreaker
}

// restingModels are the models the breaker skips now ("provider/model" → until, why), for the console.
func (s *Service) restingModels() []map[string]any {
	out := []map[string]any{}
	for _, e := range s.breaker().Snapshot() {
		out = append(out, map[string]any{"key": e.Key, "until": e.Until.UTC().Format(time.RFC3339), "class": string(e.Class)})
	}
	return out
}

// chainTimeout is the time a whole call of a with fallbacks may take: each model's own limit.
func chainTimeout(a *openai.Adapter, first time.Duration) time.Duration {
	if a == nil || a.Failover == nil {
		return first
	}
	total := first
	for _, fb := range a.Failover.Fallbacks {
		total += fb.Timeout
	}
	return total
}

func parseUSD(s string, def float64) float64 {
	if v, err := strconv.ParseFloat(strings.TrimSpace(s), 64); err == nil && v >= 0 {
		return v
	}
	return def
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, value := range in {
		value = strings.TrimSpace(value)
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

// MemoryEngine returns the memory engine for the current config revision (built lazily).
func (s *Service) MemoryEngine() (*engine.Engine, error) {
	s.lifeMu.Lock()
	defer s.lifeMu.Unlock()
	if s.closing {
		return nil, errors.New("service closed")
	}
	if s.Store == nil {
		return nil, errors.New("run setup first")
	}
	st := s.memState()
	st.mu.Lock()
	defer st.mu.Unlock()
	live := s.snapshot()
	if st.eng != nil && st.rev == live.rev {
		return st.eng, nil
	}
	doc := live.doc
	key := memoryConfigKey(doc)
	if st.eng != nil && st.key == key {
		st.rev = live.rev // nothing the engine uses changed
		return st.eng, nil
	}
	ms, err := store.Open(s.Store)
	if err != nil {
		return nil, err
	}
	var llm *openai.Adapter
	if a, err := s.llmAdapterFor(doc, "embed"); err == nil {
		llm = a
		// the embed adapter also writes the memory summaries when the summary model shares its provider:
		// the one-line summary is a Quick call, thinking only when models.summary.effort is set
		llm.RoleEffort = doc.Models["summary"].Effort
	}
	models := engine.Models{}
	if m, ok := modelOf(doc, "embed"); ok {
		models.Embed, models.EmbedDims = m.Model, m.Dimensions
		_, models.EmbedTruncatable = openai.EmbedFullWidth(m.Model)
		models.EmbedKeepsMessageWidth = openai.EmbedKeepsMessageWidth(m.Model)
		if m.Dimensions == 4096 {
			// The default is config.DefaultEmbedDimensions; only explicit 4096 uses the full-width space,
			// whose name matches the sandbox space.
			models.EmbedDims = 0
		}
	}
	for role, dst := range map[string]*string{"rerank": &models.Rerank, "summary": &models.Summary, "response": &models.Response, "planner": &models.Planner} {
		if m, ok := modelOf(doc, role); ok {
			*dst = m.Model
		}
	}
	if models.Planner == "" {
		models.Planner = models.Response
	}
	if models.Summary == "" {
		models.Summary = models.Response
	}
	recipe := leaves.DefaultRecipe()
	if doc.Memory.LeafSplit == "fixed" {
		recipe = leaves.Recipe{Size: 8, Split: leaves.SplitFixed, WithPrefix: true}
	}
	sc := search.Default()
	if len(doc.Memory.PersonAliases) > 0 {
		// operator-linked names join the ones observed from Discord accounts
		sc.PersonAliases = doc.Memory.PersonAliases
		sc.Retrieval.Person.Identity = "both"
	}
	if doc.Memory.MessageVectorsOn() {
		on := true
		sc.Retrieval.MessageDense.Enabled = &on
	}
	settings := engine.Settings{Search: sc, Recipe: recipe, Summaries: doc.Memory.Summaries,
		Languages: doc.Memory.Languages, BotNames: doc.BotNames(), Workers: doc.Memory.Workers(),
		MessageVectors: doc.Memory.MessageVectorsOn()}
	settings.RefTZ = s.instanceZone()
	if tz := doc.Memory.NaiveTimezone; tz != "" {
		if loc, err := time.LoadLocation(tz); err == nil {
			settings.NaiveTZ = loc
		} else {
			s.Log.Error("unknown memory naive timezone; using the instance timezone", "timezone", tz, "err", err)
			settings.NaiveTZ = s.instanceZone()
		}
	}
	old := st.eng
	st.eng = engine.New(ms, llm, models, settings)
	// roles on another provider than embedding get that provider's adapter (Gemini embeddings with
	// OpenRouter reranking, say); on the same provider the embed adapter serves them
	embedProvider := doc.Models["embed"].Provider
	summaryRole := "summary"
	if _, ok := modelOf(doc, "summary"); !ok {
		summaryRole = "response" // models.Summary fell back to the response model above
	}
	for role, dst := range map[string]**openai.Adapter{"rerank": &st.eng.RerankLLM, summaryRole: &st.eng.SummaryLLM} {
		if m, ok := modelOf(doc, role); ok && m.Provider != embedProvider {
			if a, err := s.llmAdapterFor(doc, role); err == nil {
				*dst = a
			}
		}
	}
	if st.eng.SummaryLLM != nil {
		// the job decides, not the model it falls back to: the response model writing summaries keeps them
		// thinking-free unless models.summary.effort is set, and so do its fallbacks. A fallback with an
		// effort of its own uses it, as featureText does. Only a borrowed fallback (models.summary unset,
		// the response model's chain) is capped at low like any Quick call of that role (quickEffort); a
		// fallback of models.summary itself is not capped, so a summary fallback set to max thinks at max
		// on every summary
		st.eng.SummaryLLM.RoleEffort = doc.Models["summary"].Effort
		if st.eng.SummaryLLM.Failover != nil {
			for _, fb := range st.eng.SummaryLLM.Failover.Fallbacks {
				fb.A.RoleEffort = cmp.Or(quickEffort(summaryRole, fb.Effort), doc.Models["summary"].Effort)
			}
		}
	}
	callProviders := engine.CallProviders{Embed: embedProvider, Rerank: embedProvider, Summary: embedProvider}
	if m, ok := modelOf(doc, "rerank"); ok {
		callProviders.Rerank = m.Provider
	}
	if m, ok := modelOf(doc, summaryRole); ok {
		callProviders.Summary = m.Provider
	}
	if st.eng.RerankLLM == nil {
		callProviders.Rerank = embedProvider
	}
	if st.eng.SummaryLLM == nil {
		callProviders.Summary = embedProvider
	}
	st.eng.CallProviders = callProviders
	st.eng.ResolveBatcher = s.batchResolver(doc, st.eng)
	st.eng.OnCall = func(ctx context.Context, c engine.CallInfo) {
		if c.Role == "embed" {
			// a query embedding made for a chat turn (also the one started ahead of the plan, a search that
			// never came) is charged to that turn once: to its cost and to the spending its limits look at.
			// Only the call that went to the provider reports, so a search that joined it pays nothing more.
			// A rerank is counted from the search's trace
			g := chat.GuardFrom(ctx)
			g.Spend(c.CostUSD)
			g.Paid(c.CostUSD)
		}
		role := c.Role
		if role == "summary" {
			// "summary" is already used by chat and channel summaries; keep the memory-index role distinct.
			role = "memory_summary"
		}
		s.noteMemoryLLMCall(ctx, llmCall{Role: role, Model: c.Model, Provider: c.Provider, PromptTokens: c.PromptTokens,
			CompletionTokens: c.CompletionTokens, CachedTokens: c.CachedTokens, Latency: c.Latency, CostUSD: c.CostUSD, Class: c.Class})
	}
	st.eng.OnDirty = s.queueIndexRoom
	st.eng.OnRevised = func(rooms []string) {
		s.summariesRevised(rooms)
		s.scheduleResync(rooms)
	}
	if old == nil {
		// rooms the previous process captured into but did not index before it stopped or crashed
		st.eng.MarkDirty(s.queuedIndexRooms()...)
	}
	var rates engine.Rates
	if ok, err := s.kvGet("index:rates", &rates); ok && err == nil {
		st.eng.SetRates(rates)
	} else if old != nil {
		st.eng.SetRates(old.Rates())
	}
	st.rev, st.key = live.rev, key
	// Pending resyncs carry over, and while the embedding space is unchanged the loaded index keeps
	// serving until a background rebuild with the new settings replaces it.
	st.eng.Inherit(old)
	old.Retire(st.eng)
	if st.eng.Index() != nil {
		e := st.eng
		// Registration happens after releasing lifeMu below.
		s.workWG.Add(1)
		go func() { defer s.workWG.Done(); defer crash.Recover(s.Log, "memory reload"); _, _ = e.Reload() }()
	}
	return st.eng, nil
}

// migrateMemory brings the memory tables up to date when the database is opened, so every query on
// them (the console's counts, the people list) sees the current columns before the engine is built.
func migrateMemory(db *storage.Store) error {
	_, err := store.Open(db)
	return err
}

// dbBlocks exposes stored rooms as memory blocks (attachable to channel views).
func (s *Service) dbBlocks() []blocks.Block {
	eng, err := s.MemoryEngine()
	if err != nil {
		return nil
	}
	rooms, err := eng.Store.Rooms()
	if err != nil {
		return nil
	}
	var out []blocks.Block
	for _, r := range rooms {
		status := "imported"
		switch {
		case r.Embedded > 0 && r.Embedded >= r.Leaves-r.Open: // an open tail is embedded once it closes
			status = "ready"
		case r.Embedded > 0:
			status = "indexing"
		}
		label := r.Label
		if label == "" {
			label = blocks.LabelForRoom(r.Room.Room)
		}
		sem := "offset"
		if r.Platform == "kakao" {
			sem = "naive"
		}
		out = append(out, blocks.Block{
			ID: blocks.BlockID(r.Collection, r.Room.Room, eng.Settings.Recipe.Version(), eng.EmbedSpace()), Platform: r.Platform,
			Collection: r.Collection, SourceRoom: r.Room.Room, Label: label, SegmentationVersion: eng.Settings.Recipe.Version(),
			EmbeddingSpace: eng.EmbedSpace(), MessageCount: r.Messages, LeafCount: r.Leaves, Status: status, TimeSemantics: sem,
		})
	}
	return out
}

// channelRooms is the memory scope of a channel: its own room, its threads' rooms (a thread: its parent
// and the parent's other threads), plus attached blocks' rooms. Nothing else — rooms are never merged or
// shared implicitly (RAW_DATA_POLICY); a thread is part of its channel on Discord too.
func (s *Service) channelRooms(guildID, channelID string) []string {
	if guildID == "" {
		return nil
	}
	if s.threadDeleted(channelID) {
		return nil
	}
	rooms := []string{engine.RoomKey(guildID, channelID)}
	// a thread's talk belongs to its parent channel's memory, and the parent's to the thread's
	if parent := s.threadParent(channelID); parent != "" {
		rooms = append(rooms, engine.RoomKey(guildID, parent))
		channelID = parent
	}
	for _, r := range s.threadRooms(guildID, channelID) {
		if r != rooms[0] {
			rooms = append(rooms, r)
		}
	}
	pol := s.channelPolicy(guildID, channelID)
	for _, id := range pol.RAGView.Blocks {
		if b, ok := s.lookupBlock(id); ok && b.SourceRoom != "" {
			rooms = append(rooms, b.SourceRoom)
		}
	}
	return rooms
}

func (s *Service) retrieveMode() retrieval.RankingMode {
	if m, ok := retrieval.Normalize(s.Doc().Memory.Retrieve); ok {
		return m
	}
	return retrieval.Auto
}

func (s *Service) memoryActions(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	eng, err := s.MemoryEngine()
	if err != nil {
		return action.Fail(req, errcode.NotSetup, "generic.error", err.Error(), nil)
	}
	switch req.Action {
	case "messages.search":
		q := strVal(in, "query", strVal(in, "q", ""))
		if q == "" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "query required", nil)
		}
		if len([]rune(q)) > 200 {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "query must be at most 200 characters", nil)
		}
		rooms := strSlice(in, "rooms")
		if g, c := strVal(in, "guild_id", ""), strVal(in, "channel_id", ""); c != "" {
			rooms = append(rooms, s.channelRooms(g, c)...)
		}
		rooms = uniqueStrings(rooms)
		if len(rooms) == 0 {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "scope required: --channel_id (and --guild_id) or --rooms", nil)
		}
		if len(rooms) > 50 {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "at most 50 rooms may be searched", nil)
		}
		topK := min(100, max(1, intVal(in, "top_k", 20)))
		found, err := eng.Store.SearchMessages(rooms, q, topK)
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		byRoom := map[string]blocks.Block{}
		for _, b := range s.dbBlocks() {
			byRoom[b.SourceRoom] = b
		}
		hits := make([]map[string]any, 0, len(found))
		for i, h := range found {
			m := h.Message
			b := byRoom[m.Room]
			hits = append(hits, map[string]any{
				"rank": i + 1, "observation_id": m.ObservationID, "source_room_id": m.Room,
				"source_room_original": m.RoomOriginal, "block_id": b.ID, "platform": b.Platform,
				"label": b.Label, "date": m.ParsedTime, "speakers": []string{m.ActorLabel},
				"excerpt": clip(m.Content, 1200), "lexical_score": h.Score,
			})
		}
		return action.OK(req, map[string]any{"hits": hits, "rag_state": "raw_lexical", "mode": "raw_lexical", "rooms": rooms, "cost_usd": 0})
	case "memory.search":
		q := strVal(in, "query", strVal(in, "q", ""))
		if q == "" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "query required", nil)
		}
		rooms := strSlice(in, "rooms")
		if g, c := strVal(in, "guild_id", ""), strVal(in, "channel_id", ""); c != "" {
			rooms = append(rooms, s.channelRooms(g, c)...)
		}
		if len(rooms) == 0 {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "scope required: --channel_id (and --guild_id) or --rooms", nil)
		}
		if eng.Index() == nil {
			if _, err := eng.Reload(); err != nil {
				return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
			}
		}
		mode := s.retrieveMode()
		if r := strVal(in, "ranking", ""); r != "" {
			if m, ok := retrieval.Normalize(r); ok {
				mode = m
			}
		}
		it := search.Intent{Kind: "other", Source: "none"}
		if targets := strSlice(in, "targets"); len(targets) > 0 {
			it.Targets = targets
		}
		res, corrected, err := s.reviewedSearch(ctx, eng, engine.SearchRequest{Query: q, Intent: it, Rooms: rooms, Ranking: mode,
			TopK: intVal(in, "top_k", 5), RemainingUSD: parseUSD(s.Doc().Memory.TurnBudgetUSD, 0.05)})
		if err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
		}
		if corrected == nil {
			corrected = map[string]memoryReview{}
		}
		return action.OK(req, map[string]any{"hits": res.Hits, "rag_state": res.State, "rooms": rooms,
			"ranking": res.Trace["applied"], "auto_reason": res.Trace["ranking_auto_reason"], "corrected": corrected})
	case "imports.start":
		src := strVal(in, "source", "raw")
		path := strVal(in, "path", "")
		if path == "" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "path required", nil)
		}
		path, err = s.importPath(path)
		if err != nil {
			return action.Fail(req, errcode.PermissionDenied, "generic.error", err.Error(), nil)
		}
		switch src {
		case "raw":
			var rooms map[string]bool
			if rs := strSlice(in, "rooms"); len(rs) > 0 {
				rooms = map[string]bool{}
				for _, r := range rs {
					rooms[r] = true
				}
			}
			merge := map[string]string{}
			if into := strVal(in, "merge_into", ""); into != "" {
				for r := range rooms {
					merge[r] = into
				}
			}
			res, err := eng.ImportRawContext(ctx, path, rooms, merge)
			if err != nil {
				return s.importFailure(req, err, res)
			}
			return action.OK(req, map[string]any{"source": src, "result": res, "next": "memory reindex", "atomicity": "batch", "resume": "same_file"})
		case "kakao_csv":
			room := strVal(in, "room", "")
			if room == "" {
				return action.Fail(req, errcode.InvalidInput, "generic.error", "room required (e.g. kakao:friends)", nil)
			}
			res, err := eng.ImportKakaoCSVContext(ctx, path, strVal(in, "collection", strings.TrimPrefix(room, "kakao:")), room,
				strVal(in, "label", ""), strVal(in, "timezone", s.Doc().Memory.NaiveTimezone), strVal(in, "export_locale", ""))
			if err != nil {
				return s.importFailure(req, err, res)
			}
			return action.OK(req, map[string]any{"source": src, "result": res, "next": "memory reindex", "atomicity": "batch", "resume": "same_file"})
		}
		return action.Fail(req, errcode.InvalidInput, "generic.error", "source must be raw | kakao_csv", nil)
	case "memory.reindex":
		opts := engine.IndexOptions{Rooms: strSlice(in, "rooms"), DryRun: boolVal(in, "dry_run"), Limit: intVal(in, "limit", 0),
			BudgetUSD: parseUSD(strVal(in, "budget_usd", s.Doc().Memory.IndexBudgetUSD), 1.0)}
		indexCtx := memoryJobContext(ctx, "memory_reindex_")
		defer s.flushMemoryLLMCalls(indexCtx)
		rep, err := eng.IndexRooms(indexCtx, opts, nil)
		if err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
		}
		out := map[string]any{"report": rep}
		if len(rep.BatchWaiting) > 0 {
			// left out of the pass: their pending work is paid for in a batch still at the provider
			out["note"] = "rooms waiting for their batch at the provider were not indexed (report.batch_waiting_rooms)"
		}
		return action.OK(req, out)
	case "chat.respond":
		ci := ChatInput{GuildID: strVal(in, "guild_id", "0"), ChannelID: strVal(in, "channel_id", ""),
			Message: strVal(in, "message", ""), Speaker: strVal(in, "speaker", "tester"), SpeakerID: strVal(in, "speaker_id", ""), DryRun: true,
			Ranking: strVal(in, "ranking", ""), PersonaRef: strVal(in, "persona_ref", "")}
		if v, ok := in["draft_style"].(string); ok {
			ci.DraftStyle = &v
		}
		if v, ok := in["draft_rules"].(string); ok {
			if len([]rune(v)) > 2000 {
				return action.Fail(req, errcode.InvalidInput, "generic.error", "rules must be at most 2000 characters", nil)
			}
			ci.DraftRules = &v
		}
		out, err := s.ChatTurn(ctx, ci)
		if err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
		}
		res := map[string]any{"answer": out.Answer, "rag_state": out.RagState, "hits": len(out.Hits),
			"intent": out.Intent, "retrieval": out.Retrieve, "timing_ms": out.Timing, "cost_usd": out.CostUSD, "tools": out.Tools}
		if boolVal(in, "detail") {
			res["evidence"] = hitBrief(out.Hits)
			res["plan"] = out.Plan
			res["assessment"] = out.Assess
		}
		return action.OK(req, res)
	}
	return action.Fail(req, errcode.ActionUnknown, "action.unknown", s.msg(req, "action.unknown"), nil)
}

func (s *Service) importFailure(req action.Request, err error, result any) action.Result {
	key, message := "generic.error", err.Error()
	switch {
	case errors.Is(err, corpus.ErrImportTooLarge):
		key = "imports.limit.file"
	case errors.Is(err, corpus.ErrFieldTooLarge):
		key = "imports.limit.field"
	case errors.Is(err, corpus.ErrKakaoGroupTooLarge):
		key = "imports.limit.timestamp_group"
	case errors.Is(err, corpus.ErrRowTooLarge):
		key = "imports.limit.row"
	case errors.Is(err, errPreviewSpeakers):
		key = "imports.limit.speakers"
	}
	if key != "generic.error" {
		message = s.msg(req, key)
	}
	detail := map[string]any{}
	if req.Action == "imports.start" {
		detail = map[string]any{"result": result, "atomicity": "batch", "resume": "same_file"}
	}
	return action.Fail(req, errcode.InvalidInput, key, message, detail)
}

// memoryStatus is memory.blocks.list's extra section.
func (s *Service) memoryStatus() map[string]any {
	return s.memoryStatusFor(context.Background(), false, false)
}

// memoryStatusFor is memoryStatus with the message-vector section filled further: pending adds the
// waiting count, estimate the cost of turning the setting on (see messageVectorStatus). One engine lookup
// serves the whole section.
func (s *Service) memoryStatusFor(ctx context.Context, pending, estimate bool) map[string]any {
	eng, err := s.MemoryEngine()
	if err != nil {
		return map[string]any{"status": "unavailable", "reason": err.Error()}
	}
	counts, _ := eng.Store.Counts()
	out := map[string]any{"counts": counts, "embed_space": eng.EmbedSpace(), "recipe": eng.Settings.Recipe.Version(),
		"retrieve": s.Doc().Memory.Retrieve, "capture": s.Doc().Memory.Capture}
	if ix := eng.Index(); ix != nil {
		out["loaded"] = map[string]any{"leaves": len(ix.Leaves), "messages": len(ix.Messages), "at": eng.LoadedAt().UTC().Format(time.RFC3339)}
	} else {
		out["loaded"] = nil
	}
	live := -1
	if n, ok := counts["observations"]; ok {
		live = n
	}
	if mv := s.messageVectorStatus(ctx, eng, pending, estimate, live); mv != nil {
		out["message_vectors"] = mv
	}
	return out
}

// messageVectorStatus is the message-vector section of the memory status (memory.message_vectors): how
// many message vectors are stored for the current space and, with pending, how many messages of closed
// leaves still wait for one. With estimate it also previews what turning the setting on would cost, which
// the console shows before the operator confirms (it works while the setting is still off). While the
// setting is off and no estimate is asked for it returns nil and the status stays as it was.
func (s *Service) messageVectorStatus(ctx context.Context, eng *engine.Engine, pending, estimate bool, live int) map[string]any {
	on := s.Doc().Memory.MessageVectorsOn()
	if !on && !estimate {
		return nil
	}
	space := eng.MessageSpace()
	dims := eng.MessageVectorDims()
	if ix := eng.Index(); space != "" && ix != nil && ix.MessageSpace == space && ix.MsgVecs.Dims > 0 {
		dims = ix.MsgVecs.Dims
	}
	out := map[string]any{"enabled": on, "space": space, "dimensions": dims}
	_, inSpace, countErr := eng.Store.MessageVectorCounts(space)
	if countErr == nil {
		out["stored"] = inSpace
	}
	if ix := eng.Index(); ix != nil && on {
		out["loaded"] = ix.MsgVecs.Rows()
	}
	if pending && on && countErr == nil {
		if n, ok := s.pendingMessages(ctx, eng, space, inSpace, live); ok {
			out["pending"] = n
		}
	}
	if estimate {
		if est, err := eng.EstimateMessageVectors(nil); err == nil {
			out["estimate"] = est
		}
	}
	return out
}

type spendResetSnapshot struct {
	Month    string             `json:"month"`
	At       string             `json:"at"`
	ByKind   map[string]float64 `json:"by_kind"`
	Total    float64            `json:"total"`
	Day      string             `json:"day"`
	DayTotal float64            `json:"day_total"`
}

const spendResetKey = "spend_reset"

func (s *Service) readSpendReset() (spendResetSnapshot, bool) {
	var reset spendResetSnapshot
	ok, err := s.kvGet(spendResetKey, &reset)
	if err != nil || !ok || reset.Month == "" || reset.At == "" || reset.ByKind == nil {
		return spendResetSnapshot{}, false
	}
	return reset, true
}

func (s *Service) activeSpendReset(now time.Time) (spendResetSnapshot, bool) {
	reset, ok := s.readSpendReset()
	if !ok || reset.Month != now.UTC().Format("2006-01") {
		return spendResetSnapshot{}, false
	}
	return reset, true
}

func spendResetInfo(reset spendResetSnapshot) map[string]any {
	return map[string]any{"at": reset.At, "before_usd": reset.Total}
}

func (s *Service) spendAfterResetDay(day string, spent float64) float64 {
	reset, ok := s.readSpendReset()
	if ok && reset.Day == day {
		spent -= reset.DayTotal
		if spent < 0 {
			return 0
		}
	}
	return spent
}

// monthSpend is provider spend this month from memory_spend, adjusted by an optional reset snapshot.
func (s *Service) monthSpend() (map[string]float64, float64, bool) {
	eng, err := s.MemoryEngine()
	if err != nil {
		return nil, 0, false
	}
	now := s.Clock.Now().UTC()
	by, total, err := eng.Store.SpendSince(now.Format("2006-01") + "-01") // spend days are UTC (store.AddSpend)
	if err != nil {
		return nil, 0, false
	}
	reset, ok := s.activeSpendReset(now)
	if !ok {
		return by, total, true
	}
	effective := make(map[string]float64, len(by))
	total = 0
	for kind, raw := range by {
		spent := raw - reset.ByKind[kind]
		if spent < 0 {
			spent = 0
		}
		effective[kind] = spent
		total += spent
	}
	return effective, total, true
}

func (s *Service) usageReset(req action.Request) action.Result {
	in := map[string]any{}
	if len(req.Input) > 0 {
		if err := json.Unmarshal(req.Input, &in); err != nil || in == nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "input must be an object", nil)
		}
	}
	if boolVal(in, "undo") {
		reset, _ := s.readSpendReset()
		if err := s.kvDelete(spendResetKey); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		spent := "unknown"
		if _, total, ok := s.monthSpend(); ok {
			spent = fmt.Sprintf("%.4f", total)
		}
		return action.OK(req, map[string]any{
			"spent_usd":  spent,
			"before_usd": fmt.Sprintf("%.4f", reset.Total),
			"at":         reset.At,
		})
	}

	eng, err := s.MemoryEngine()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	now := s.Clock.Now().UTC()
	month := now.Format("2006-01")
	day := now.Format("2006-01-02")
	// month and today come from one read, so the two baselines can never straddle a spend written between them
	by, total, dayTotal := map[string]float64{}, 0.0, 0.0
	rows, err := eng.Store.DB.Reader().Query(`SELECT day, kind, usd FROM memory_spend WHERE day>=?`, month+"-01")
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	for rows.Next() {
		var d, kind string
		var usd float64
		if err := rows.Scan(&d, &kind, &usd); err != nil {
			rows.Close()
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		by[kind] += usd
		total += usd
		if d == day {
			dayTotal += usd
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	reset := spendResetSnapshot{Month: month, At: now.Format(time.RFC3339), ByKind: by, Total: total, Day: day, DayTotal: dayTotal}
	if err := s.kvSet(spendResetKey, reset); err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	spent := "0.0000"
	if _, total, ok := s.monthSpend(); ok {
		spent = fmt.Sprintf("%.4f", total)
	}
	return action.OK(req, map[string]any{
		"spent_usd":  spent,
		"before_usd": fmt.Sprintf("%.4f", reset.Total),
		"at":         reset.At,
	})
}

// ---------------------------------------------------------------------------
// Chat turns (Discord replies and the chat.respond Action)
// ---------------------------------------------------------------------------

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
	// readAsOf is the SettledMark taken before the turn read anything (store.SettleSources). It stays 0,
	// which excuses nothing, for a turn that came with something already read: a command's result or pictures.
	readAsOf int64
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
var chatDeadline = map[string]time.Duration{"planner": 30 * time.Second, "response": 90 * time.Second, "vision": 30 * time.Second}

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
	d, ok := chatDeadline[role]
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
		om[i] = openai.Message{Role: m.Role, Content: m.Content, Images: imageParts(m.Images)}
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

// Chat turn refusals. The Discord connector stays silent on these (no error spam in the channel).
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
func (s *Service) warmsQueries(doc config.Document, in ChatInput) bool {
	return !in.DryRun && !s.practice && doc.Providers[doc.Models["embed"].Provider].DailyLimit == 0 && s.MemoryAllowed(in.SpeakerID)
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
	if len(in.Earlier) == 0 && len(in.Images) == 0 {
		in.readAsOf = store.SettledMark(s.Store)
	}
	ctx = observability.WithTurn(ctx, in.turnID)
	sources := []string{}
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
		"memory_state": res.Assess.MemoryState, "intent_source": res.Intent.Source, "tools": res.Tools,
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
		CostUSD: res.CostUSD, LatencyMS: int(time.Since(started).Milliseconds()), Lab: in.DryRun, ReadAsOf: in.readAsOf, Body: body})
	return res, err
}

// hitBrief is the evidence a turn used, as the console shows it.
func hitBrief(hits []search.Hit) []map[string]any {
	out := make([]map[string]any, 0, len(hits))
	for _, h := range hits {
		m := map[string]any{"rank": h.Rank, "doc_id": h.DocID, "room": h.Room, "date": h.Date, "speakers": h.Speakers, "excerpt": clip(h.Excerpt, 1200),
			"summary": h.Summary, "span": h.Span, "sources": h.Sources, "dense_cos": h.DenseCos}
		m["observation_ids"] = h.ObservationIDs
		m["visible_observation_ids"] = h.VisibleObservationIDs
		m["leaf_observation_ids"] = h.LeafObservationIDs
		if h.RerankScore != nil {
			m["rerank_score"] = *h.RerankScore
		}
		// Evidence is packed without repeats: when this hit's own messages were already shown by an
		// earlier hit, its excerpt holds only their surroundings. Say where they are, so the date (the
		// hit's own messages) and the text do not look like they disagree.
		if n := shownEarlier(hits, h); n > 0 {
			m["leaf_shown_in"] = n
		}
		out = append(out, m)
	}
	return out
}

// shownEarlier is the rank of the first earlier hit whose excerpt shows h's own messages, when h's
// excerpt shows none of them (0 otherwise).
func shownEarlier(hits []search.Hit, h search.Hit) int {
	own := make(map[string]bool, len(h.LeafObservationIDs))
	for _, id := range h.LeafObservationIDs {
		own[id] = true
	}
	for _, id := range h.VisibleObservationIDs {
		if own[id] {
			return 0
		}
	}
	for _, e := range hits {
		if e.Rank >= h.Rank {
			break
		}
		for _, id := range e.VisibleObservationIDs {
			if own[id] {
				return e.Rank
			}
		}
	}
	return 0
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
	if turns, err := eng.Store.RecentChatTurns(key, 16); err == nil {
		for _, t := range turns {
			at, _ := time.Parse(time.RFC3339, t.CreatedAt) // the store writes RFC3339; a bad value shows no age
			history = append(history, chat.Turn{Role: t.Role, Speaker: t.Speaker, Content: t.Content, At: at})
			if in.sourceRefs != nil {
				*in.sourceRefs = append(*in.sourceRefs, t.SourceIDs...)
				if t.Role == "user" && t.NativeID != "" {
					*in.sourceRefs = append(*in.sourceRefs, corpus.DiscordLiveObservationID(room, t.NativeID))
				}
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
	if in.sourceRefs != nil {
		for _, m := range recent {
			*in.sourceRefs = append(*in.sourceRefs, m.ObservationID)
		}
	}
	llm := chatLLM{s: s, routes: routes}
	maxHistory := 0
	conv, attributed := s.callerConversation(ctx, in, eng, pol, voice, llm)
	if attributed {
		// the caller's conversation replaces the channel's plain history and recent lines: the other
		// conversations of the room are not shown to the planner or the answer at all
		history, recent, maxHistory = conv.history, nil, 24
		if in.sourceRefs != nil {
			*in.sourceRefs = append(*in.sourceRefs, conv.sources...)
		}
		for k, v := range conv.ledger {
			if in.context != nil {
				in.context[k] = v
			}
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
	roomMemory := s.turnRoomMemory(ctx, in, pol, voice, history, recent)
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
		MaxHistory: maxHistory, ToolMode: s.toolMode(), LoopFinal: doc.Chat.LoopFinalMode(), RoomMemory: roomMemory, Images: images, TurnStart: in.started,
	})
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
	var firstSendMS int
	var deliverEnd time.Time
	if in.Deliver != nil {
		said, firstSendMS, deliverEnd = deliverTimed(ctx, in, res)
		noteTurnTiming(&res, "first_send", firstSendMS)
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
	// a person who withdrew consent to being remembered is not kept in the chat context either
	if !in.DryRun && s.MemoryAllowed(in.SpeakerID) && kept {
		var sources []string
		if in.sourceRefs != nil {
			sources = *in.sourceRefs
		}
		sources = uniqueStrings(append(append(append([]string{}, sources...), lookedUp(res)...), in.MessageID, personRef(in.SpeakerID)))
		turns := []store.ChatTurn{{Role: "user", Speaker: in.Speaker, Content: in.Message, NativeID: in.MessageID, SpeakerID: in.SpeakerID}}
		if in.vision != nil && in.vision.Mode == "described" {
			// the description came from other messages too: deleting one of them removes this turn
			turns[0].DerivedFrom = in.vision.Sources
		}
		if !toolFailureToReplay(res.Tools) {
			turns = append(turns, store.ChatTurn{Role: "assistant", Content: said, NativeID: in.MessageID, SpeakerID: in.SpeakerID, SourceIDs: sources, ReadAsOf: in.readAsOf})
		}
		_ = eng.Store.AppendChatTurns(key, turns, 40)
	}
	s.logTurn(in, res)
	if !deliverEnd.IsZero() {
		noteTurnTiming(&res, "post_send", int(time.Since(deliverEnd).Milliseconds()))
	}
	return res, nil
}

// lookedUp lists the messages a reply retrieved (its hits and tool results): the reply is built on them as
// much as on the conversation, so deleting one takes the reply out of the chat context. Messages, not
// leaves: deleting a message tombstones the leaf that held it, the leaf's ID comes back when the room is
// cut again, and a later reply on the new leaf has read nothing deleted.
func lookedUp(res chat.Result) []string {
	raw, _ := json.Marshal(map[string]any{"hits": hitBrief(res.Hits), "tools": res.Tools})
	var body any
	_ = json.Unmarshal(raw, &body)
	var ids []string
	walkEvidence(body, func(_ map[string]any, found []string) { ids = append(ids, found...) })
	return ids
}

// toolFailureToReplay keeps a failed tool exchange from becoming an answer the bot cites again in an
// unrelated turn. The user message remains in context; the current turn's tool result is still shown.
func toolFailureToReplay(results []chat.ToolResult) bool {
	for _, result := range results {
		status := result.Status
		if status == "" {
			status, _ = result.Data["status"].(string)
		}
		switch status {
		case "unconfigured", "unavailable", "no_results", "no_area", "not_found", "error":
			return true
		}
		if !result.OK {
			return true
		}
	}
	return false
}

func rankingFor(override string, def retrieval.RankingMode) retrieval.RankingMode {
	if override != "" {
		if m, ok := retrieval.Normalize(override); ok {
			return m
		}
	}
	return def
}

// logTurn writes a body-free turn record to the traces table (operator diagnostics).
func (s *Service) logTurn(in ChatInput, res chat.Result) {
	body := map[string]any{
		"kind": "chat_turn", "channel": blocks.ChannelKey(in.GuildID, in.ChannelID), "rag_state": res.RagState,
		"turn_id": in.turnID,
		"hits":    len(res.Hits), "memory_state": res.Assess.MemoryState, "intent_source": res.Intent.Source,
		"retrieval": res.Retrieve, "timing_ms": res.Timing, "cost_usd": res.CostUSD, "tools": len(res.Tools),
	}
	addTurnSpans(body, res)
	if res.ToolLoop != nil {
		body[toolLoopKey(res.ToolLoop)] = res.ToolLoop.Brief() // calls and reasons, no arguments
	}
	if res.AnswerRetry != "" {
		body["answer_retry"] = res.AnswerRetry
	}
	if res.AnswerLoop {
		body["answer_loop"] = true
	}
	id := newID("tr_")
	raw, _ := json.Marshal(body)
	db, err := s.adminDB()
	if err != nil {
		return
	}
	var sources []string
	if in.sourceRefs != nil {
		sources = *in.sourceRefs
	}
	refsRaw, _ := json.Marshal(map[string]any{"hits": hitBrief(res.Hits), "tools": res.Tools})
	var refs any
	_ = json.Unmarshal(refsRaw, &refs)
	err = db.WithWriter(func(tx *sql.Tx) error {
		keys, err := copySources(tx, refs, sources, []string{in.MessageID, personRef(in.SpeakerID)})
		if err != nil {
			return err
		}
		blocked, err := store.SourcesBlockedAsOf(tx, keys, in.readAsOf)
		if err != nil {
			return err
		}
		if blocked {
			delete(body, "retrieval")
		}
		raw, _ = json.Marshal(body)
		if _, err := tx.Exec(`INSERT INTO traces(id,body,created_at,evidence_state) VALUES(?,?,?,1)`, id, string(raw), s.Clock.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
		// The turn already indexes every source read by this reply. A trace points to that turn
		// instead of copying its candidate/context list; redaction follows the turn -> trace link.
		if in.turnID != "" {
			// Register the turn's sources in this transaction too: the trace is written just before
			// recordTurn, and a concurrent deletion must follow the link during that interval.
			if err := indexTurnRefs(tx, in.turnID, keys, in.MessageID); err != nil {
				return err
			}
			keys = uniqueStrings([]string{in.turnID, in.MessageID, personRef(in.SpeakerID)})
		}
		return indexCopy(tx, "trace", id, keys, false)
	})
	if err != nil {
		s.Log.Debug("trace write failed", slog.String("err", err.Error()))
	}
}

// RunMemoryWorker loads the memory index and, when memory.embed is on, periodically resyncs rooms
// with newly captured messages and embeds closed leaves under memory.index_budget_usd per pass.
// It blocks until ctx is done (the daemon runs it in a goroutine).
func (s *Service) RunMemoryWorker(ctx context.Context) {
	ctx, done, ok := s.beginWork(ctx)
	if !ok {
		return
	}
	defer done()
	if s.Store == nil {
		return
	}
	eng, err := s.MemoryEngine()
	if err != nil {
		s.Log.Warn("memory: engine unavailable", "err", err)
		return
	}
	_ = crash.Run(s.Log, "memory initial load", func() {
		if info, err := eng.Reload(); err != nil {
			s.Log.Warn("memory: initial load failed", "err", err)
		} else {
			s.Log.Info("memory: index loaded", "rooms", info["rooms"], "leaves", info["leaves"], "summaries", info["summaries"])
		}
	})
	interval := time.Duration(max(1, s.Doc().Memory.IndexIntervalMin)) * time.Minute
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		// a failing pass is logged and the next one runs on schedule
		_ = crash.Run(s.Log, "memory index pass", func() { s.memoryPass(ctx) })
	}
}

// memoryPass indexes the rooms with newly captured messages (one tick of RunMemoryWorker).
func (s *Service) memoryPass(ctx context.Context) {
	roomMemoryCtx := ctx
	ctx = memoryJobContext(ctx, "memory_index_")
	defer s.flushMemoryLLMCalls(ctx)
	defer s.roomMemoryPass(roomMemoryCtx) // room memory (episodes.go) runs after indexing, with or without embeddings
	if !s.Doc().Memory.Embed {
		return
	}
	eng, err := s.MemoryEngine()
	if err != nil {
		return
	}
	// A provider-width change also leaves quiet rooms pending, without a captured-message event.
	repairRooms, err := eng.LeafWidthRepairRooms()
	if err != nil {
		s.Log.Warn("memory: leaf width check failed", "err", err)
		return
	}
	eng.MarkDirty(repairRooms...)
	rooms := []string{}
	for _, room := range eng.DirtyRooms() {
		if s.blockMeta(room).autoUpdate() {
			rooms = append(rooms, room)
		}
	}
	if len(rooms) == 0 {
		s.saveIndexQueue(eng)
		return
	}
	started := s.Clock.Now()
	rep, err := eng.IndexRooms(ctx, engine.IndexOptions{Rooms: rooms, BudgetUSD: parseUSD(s.Doc().Memory.IndexBudgetUSD, 1.0)}, nil)
	s.saveIndexRates(eng)
	// DirtyRooms took the rooms off the queue. A pass that failed or stopped early (provider error,
	// budget, storage) leaves closed leaves unembedded, and only embedded leaves are searchable: put
	// the rooms back so the next pass continues instead of waiting for someone to speak there again.
	unfinished := err != nil || rep.StoppedBy != "" || len(rep.FailedRooms) > 0
	if unfinished {
		eng.MarkDirty(rooms...)
	}
	// a room whose batch is still at the provider was left alone (its pending leaves are paid for there):
	// it stays queued and is indexed once the batch is applied
	eng.MarkDirty(rep.BatchWaiting...)
	s.saveIndexQueue(eng)
	st := scheduleState{LastRun: started.UTC().Format(time.RFC3339), OK: err == nil, DurationMS: s.Clock.Now().Sub(started).Milliseconds(),
		Detail: map[string]any{"rooms": len(rooms)}}
	switch {
	case err != nil:
		st.Error = err.Error()
	default:
		st.Detail["leaf_vectors"], st.Detail["spent_usd"] = rep.LeafVectors, rep.SpentUSD
		if rep.MessageVectors > 0 {
			st.Detail["message_vectors"] = rep.MessageVectors
		}
		if rep.StoppedBy != "" {
			st.Detail["stopped_by"] = rep.StoppedBy
		}
		if len(rep.FailedRooms) > 0 {
			st.Detail["failed_rooms"] = len(rep.FailedRooms)
		}
		// a budget stop is the per-pass limit working; anything else means memory is falling behind
		if (rep.StoppedBy != "" && rep.StoppedBy != "budget") || len(rep.FailedRooms) > 0 {
			st.OK = false
			st.Error = indexPassProblem(rep)
		}
	}
	_ = s.kvSet("schedule:index", st)
	if err != nil {
		s.Log.Warn("memory: index pass failed", "err", err)
		return
	}
	for room, e := range rep.FailedRooms {
		s.Log.Warn("memory: room resync failed", "room", room, "err", e)
	}
	s.Log.Info("memory: index pass", "rooms", len(rooms), "leaf_vectors", rep.LeafVectors, "from_cache", rep.FromCache,
		"summaries", rep.Summaries, "spent_usd", rep.SpentUSD, "stopped_by", rep.StoppedBy,
		"index_leaves", rep.Reload["leaves"], "index_reused", eng.LastReused(), "reload_ms", eng.LastReload().Milliseconds(),
		"reload_read_ms", eng.LastReloadRead().Milliseconds())
}

// indexQueueKey keeps the rooms waiting for an index pass across restarts.
const indexQueueKey = "index:queue"

// queueIndexRoom records a room that a captured message queued (Engine.OnDirty).
func (s *Service) queueIndexRoom(room string) {
	s.indexQueueMu.Lock()
	defer s.indexQueueMu.Unlock()
	var rooms []string
	_, _ = s.kvGet(indexQueueKey, &rooms)
	if slices.Contains(rooms, room) {
		return
	}
	if err := s.kvSet(indexQueueKey, append(rooms, room)); err != nil {
		s.Log.Warn("memory: could not record the index queue", "err", err)
	}
}

// saveIndexQueue replaces the recorded queue with the engine's (after a pass took rooms off it).
func (s *Service) saveIndexQueue(eng *engine.Engine) {
	s.indexQueueMu.Lock()
	defer s.indexQueueMu.Unlock()
	rooms := eng.QueuedRooms()
	if rooms == nil {
		rooms = []string{}
	}
	var recorded []string
	if ok, _ := s.kvGet(indexQueueKey, &recorded); (ok || len(rooms) == 0) && slices.Equal(recorded, rooms) {
		return
	}
	if err := s.kvSet(indexQueueKey, rooms); err != nil {
		s.Log.Warn("memory: could not record the index queue", "err", err)
	}
}

func (s *Service) queuedIndexRooms() []string {
	var rooms []string
	if s.Store != nil {
		_, _ = s.kvGet(indexQueueKey, &rooms)
	}
	return rooms
}

// indexPassProblem is the console line for a pass that finished but left work behind.
func indexPassProblem(rep engine.IndexReport) string {
	var parts []string
	if rep.StoppedBy != "" {
		parts = append(parts, "stopped_by="+rep.StoppedBy)
	}
	if n := len(rep.FailedRooms); n > 0 {
		parts = append(parts, fmt.Sprintf("%d room(s) could not be resynced", n))
	}
	return strings.Join(parts, "; ") + " (retried on the next pass)"
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
