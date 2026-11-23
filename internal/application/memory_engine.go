package application

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"masamong/internal/chat"
	"masamong/internal/config"
	"masamong/internal/crash"
	"masamong/internal/errclass"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
	"masamong/internal/storage"
)

// memoryState caches the engine per config revision (models/settings come from config; Service.mem).
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

func (s *Service) memState() *memoryState { return &s.mem }

// stopReload asks a running rebuild of the engine to stop at its next stage (Close).
func (st *memoryState) stopReload() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.eng != nil {
		st.eng.StopReload()
	}
}

// close closes the engine; it is built again only by a service that is not closing (MemoryEngine).
func (st *memoryState) close() {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.eng != nil {
		st.eng.Close()
		st.eng = nil
	}
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
func (s *Service) roleTimeout(role string, seconds int) time.Duration {
	if seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if d, ok := s.roleDeadline(role); ok {
		return d
	}
	return time.Minute
}

// failoverFor builds role's fallback chain. A fallback whose provider cannot be reached now (no key, not
// an openai_compat host, disabled) is left out rather than failing the role.
func (s *Service) failoverFor(doc config.Document, role string, m config.Model) *openai.Failover {
	f := &openai.Failover{Provider: m.Provider, Model: m.Model, Timeout: s.roleTimeout(role, m.TimeoutSeconds), Breaker: s.breaker(),
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
		f.Fallbacks = append(f.Fallbacks, openai.Fallback{A: a, Provider: fb.Provider, Model: fb.Model, Timeout: s.roleTimeout(role, fb.TimeoutSeconds), Effort: fb.Effort})
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
	ms.Now = s.Clock.Now
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
	st.eng.Now = s.Clock.Now
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
		// registered under lifeMu, after the closing check: Close cannot wait before this reload counts
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
