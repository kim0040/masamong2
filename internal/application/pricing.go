package application

import (
	"context"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"masamong/internal/config"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
)

// Model prices price the calls a provider does not report a cost for (every host but OpenRouter) and
// the estimates shown before a paid run. Order of precedence:
//
//  1. models.<role>.price_in_per_mtok / price_out_per_mtok in config (the operator's number)
//  2. the provider's own model list, fetched at most once a day and kept in admin_state (OpenRouter's
//     is the only list with prices)
//  3. the host's published price (internal/providers/openai/hosts.json, with the date it was read)
//  4. a reference price: the same model on OpenRouter's public list, matched by name (hosts such as
//     CometAPI list no prices and bill a share of the official one, so this errs on the safe side)
//  5. unknown — callers fall back to their per-call estimates and say so
//
// A provider-reported usage.cost always wins over all of these (the adapter only prices a call that
// came back without one).
//
// Input served from the host's prompt cache has its own price, looked up the same way: the operator's
// price_cached_in_per_mtok, the model list's (OpenRouter input_cache_read, also on the reference list),
// the published table's third column. Without one it is priced as ordinary input.

type modelPrice struct {
	In        float64                  `json:"in"`               // USD per input token
	Out       float64                  `json:"out"`              // USD per output token
	Cached    float64                  `json:"cached,omitempty"` // USD per cached input token; 0 = unknown
	Overrides []openai.PricingOverride `json:"overrides,omitempty"`
}

func (p modelPrice) forRequest(promptTokens int, at time.Time) modelPrice {
	for _, o := range p.Overrides {
		if !o.Applies(promptTokens, at) {
			continue
		}
		if o.PromptPrice != nil {
			p.In = *o.PromptPrice
		}
		if o.CompletionPrice != nil {
			p.Out = *o.CompletionPrice
		}
		if o.CachedPrice != nil {
			p.Cached = *o.CachedPrice
		}
	}
	return p
}

// conservative returns the highest listed rate per token for estimates without request context.
func (p modelPrice) conservative() modelPrice {
	for _, o := range p.Overrides {
		if o.PromptPrice != nil && *o.PromptPrice > p.In {
			p.In = *o.PromptPrice
		}
		if o.CompletionPrice != nil && *o.CompletionPrice > p.Out {
			p.Out = *o.CompletionPrice
		}
		if o.CachedPrice != nil && *o.CachedPrice > p.Cached {
			p.Cached = *o.CachedPrice
		}
	}
	return p
}

type priceList struct {
	FetchedAt string                `json:"fetched_at"`
	Models    map[string]modelPrice `json:"models"`
	// Caps is what the list said about the models' reasoning (model_caps.go seeds a provider's cache from
	// it after a restart); only models it said something about
	Caps map[string]openai.Caps `json:"caps,omitempty"`
	// Until is the first time a listed price stops applying (OpenGateway's scheduled promotions): the
	// list is read again then, not a day later
	Until string `json:"until,omitempty"`
}

// stale reports a list older than a day or past a listed price's end.
func (l *priceList) stale(now time.Time) bool {
	at, err := time.Parse(time.RFC3339, l.FetchedAt)
	if err != nil || now.Sub(at) > priceListTTL {
		return true
	}
	until, err := time.Parse(time.RFC3339, l.Until)
	// a price that ended is fetched again, but not more than once in priceListMinAge: a host whose list
	// always ends a promotion a few minutes ahead would otherwise be asked again after every end
	return err == nil && !now.Before(until) && now.Sub(at) >= priceListMinAge
}

const (
	priceListTTL    = 24 * time.Hour
	priceListMinAge = time.Hour
)

type priceState struct {
	mu         sync.Mutex
	lists      map[string]*priceList
	refreshing map[string]bool
}

var priceStates sync.Map // *Service → *priceState

func (s *Service) prices() *priceState {
	v, _ := priceStates.LoadOrStore(s, &priceState{lists: map[string]*priceList{}, refreshing: map[string]bool{}})
	return v.(*priceState)
}

// pricer is the adapter hook for one provider.
func (s *Service) pricer(provider string) openai.Pricer {
	return func(model string) (float64, float64, bool) {
		p, _, ok := s.modelPrice(provider, model)
		p = p.conservative()
		return p.In, p.Out, ok
	}
}

func (s *Service) contextPricer(provider string) openai.ContextPricer {
	return func(model string, promptTokens int, at time.Time) (float64, float64, float64, bool) {
		p, source, ok := s.modelPrice(provider, model)
		if source == "config" && p.Cached == 0 {
			if listed, _, found := s.listedPrice(provider, model); found {
				p.Cached = listed.Cached
			}
		}
		p = p.forRequest(promptTokens, at)
		return p.In, p.Out, p.Cached, ok
	}
}

// cachedPricer is the adapter hook for prompt-cache prices: the operator's, else the first source of
// modelPrice's order that lists one.
func (s *Service) cachedPricer(provider string) openai.CachedPricer {
	return func(model string) (float64, bool) {
		for _, m := range configPrices(s.Doc()) {
			if m.Provider == provider && m.Model == model && m.PriceCachedInPerMTok > 0 {
				return m.PriceCachedInPerMTok / 1e6, true
			}
		}
		p, _, ok := s.listedPrice(provider, model)
		p = p.conservative()
		return p.Cached, ok && p.Cached > 0
	}
}

// modelPrice returns the price per token and where it came from (config | list | published).
// configPrices are the operator's prices: every role's model and its fallbacks (a fallback is a model
// with its own price, here as a Model).
func configPrices(doc config.Document) []config.Model {
	out := make([]config.Model, 0, len(doc.Models))
	for _, m := range doc.Models {
		out = append(out, m)
	}
	for _, m := range doc.Models {
		for _, f := range m.Fallbacks {
			out = append(out, config.Model{Provider: f.Provider, Model: f.Model, PriceInPerMTok: f.PriceInPerMTok,
				PriceOutPerMTok: f.PriceOutPerMTok, PriceCachedInPerMTok: f.PriceCachedInPerMTok})
		}
	}
	return out
}

func (s *Service) modelPrice(provider, model string) (modelPrice, string, bool) {
	for _, m := range configPrices(s.Doc()) {
		if m.Provider == provider && m.Model == model && (m.PriceInPerMTok > 0 || m.PriceOutPerMTok > 0) {
			return modelPrice{In: m.PriceInPerMTok / 1e6, Out: m.PriceOutPerMTok / 1e6, Cached: m.PriceCachedInPerMTok / 1e6}, "config", true
		}
	}
	return s.listedPrice(provider, model)
}

// listedPrice is modelPrice without the operator's numbers: the model list, the published table, the
// reference list.
func (s *Service) listedPrice(provider, model string) (modelPrice, string, bool) {
	if l := s.priceList(provider); l != nil {
		if p, ok := l.Models[model]; ok {
			return p, "list", true
		}
	}
	if p, ok := s.Doc().Providers[provider]; ok && p.Kind == config.KindOpenAI {
		if pub, ok := openai.Published(openai.ResolveAPI(p.API, p.BaseURL), model); ok {
			return modelPrice{In: pub.In, Out: pub.Out, Cached: pub.Cached}, "published", true
		}
		if l := s.priceList(referenceList); l != nil {
			if p, ok := l.Models[openai.ReferenceName(model)]; ok {
				return p, "reference", true
			}
		}
	}
	return modelPrice{}, "", false
}

func (s *Service) referencePricesURL() string {
	if s.referenceURL != "" {
		return s.referenceURL
	}
	url, _ := openai.ReferencePrices()
	return url
}

// referenceList is the price list key of the public reference list (openai.ReferencePrices), kept like
// a provider's list: fetched at most once a day, keyed by openai.ReferenceName.
const referenceList = "@reference"

// priceList returns the cached list for provider, loading it from the database and refreshing it in
// the background when it is missing or older than a day.
func (s *Service) priceList(provider string) *priceList {
	st := s.prices()
	st.mu.Lock()
	l := st.lists[provider]
	if l == nil && s.Store != nil {
		var stored priceList
		if ok, err := s.kvGet("prices:"+provider, &stored); ok && err == nil {
			l = &stored
			st.lists[provider] = l
		}
	}
	stale := l == nil || l.stale(s.Clock.Now())
	if stale && !st.refreshing[provider] && s.canListModels(provider) {
		st.refreshing[provider] = true
		s.ownedGo("model price refresh", func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			if err := s.refreshPrices(ctx, provider); err != nil {
				s.Log.Debug("model prices not refreshed", "provider", provider, "err", err)
			}
			st.mu.Lock()
			st.refreshing[provider] = false
			st.mu.Unlock()
		})
	}
	st.mu.Unlock()
	return l
}

func (s *Service) canListModels(provider string) bool {
	if s.practice || s.Registry() == nil {
		return false
	}
	if provider == referenceList {
		return s.referencePricesURL() != "" && (s.referenceURL != "" || os.Getenv("MASAMONG_OFFLINE") != "1")
	}
	p, ok := s.Doc().Providers[provider]
	if !ok || p.Kind != config.KindOpenAI || !openai.ResolveAPI(p.API, p.BaseURL).PricesInList() {
		return false // only OpenRouter's list carries prices; others' are names only
	}
	a := s.Registry().LLM[provider]
	return a != nil && a.Configured()
}

// refreshPrices fetches the provider's model list and stores the prices it contains.
func (s *Service) refreshPrices(ctx context.Context, provider string) error {
	if provider == referenceList {
		url := s.referencePricesURL()
		c := providers.NewClient(30*time.Second, s.Log)
		c.MaxBody = 32 << 20
		resp, err := c.Get(ctx, url, nil) // a public list: no key is sent
		if err != nil {
			return err
		}
		models, err := openai.ParseModels(resp.ContentType, resp.Body)
		if err != nil {
			return err
		}
		for i := range models {
			models[i].ID = openai.ReferenceName(models[i].ID)
		}
		s.storePrices(provider, models)
		return nil
	}
	a := s.Registry().LLM[provider]
	if a == nil {
		return nil
	}
	models, err := a.ListModels(ctx)
	if err != nil {
		return err
	}
	s.storePrices(provider, models)
	return nil
}

// storePrices keeps the priced models of a list. A paid model listed at 0/0 (OpenRouter lists its
// rerankers that way) is left unknown rather than treated as free.
func (s *Service) storePrices(provider string, models []openai.Model) {
	l := &priceList{FetchedAt: s.Clock.Now().UTC().Format(time.RFC3339), Models: map[string]modelPrice{}}
	for _, m := range models {
		in, errIn := strconv.ParseFloat(m.PromptPrice, 64)
		out, errOut := strconv.ParseFloat(m.CompletionPrice, 64)
		if errIn != nil || in < 0 {
			continue
		}
		if errOut != nil || out < 0 {
			out = 0
		}
		if in == 0 && out == 0 && !strings.HasSuffix(m.ID, ":free") {
			continue
		}
		if old, dup := l.Models[m.ID]; dup && old.In+old.Out >= in+out {
			continue // two listings under one name (the reference list): keep the dearer, for budgets
		}
		mp := modelPrice{In: in, Out: out, Overrides: append([]openai.PricingOverride(nil), m.Overrides...)}
		if c, err := strconv.ParseFloat(m.CachedPrice, 64); err == nil && c > 0 && c <= in {
			mp.Cached = c
		}
		l.Models[m.ID] = mp
		if u := m.PriceUntil; !u.IsZero() && u.After(s.Clock.Now()) {
			if old, err := time.Parse(time.RFC3339, l.Until); err != nil || u.Before(old) {
				l.Until = u.UTC().Format(time.RFC3339)
			}
		}
	}
	if provider != referenceList {
		for _, m := range models {
			if c := m.Caps(); c.Known() {
				if l.Caps == nil {
					l.Caps = map[string]openai.Caps{}
				}
				l.Caps[m.ID] = c
			}
		}
	}
	// a list without prices (CometAPI) is kept too, empty: its date stops a refetch on every lookup
	st := s.prices()
	st.mu.Lock()
	st.lists[provider] = l
	st.mu.Unlock()
	if s.Store != nil {
		_ = s.kvSet("prices:"+provider, l)
	}
}

// priceAdapters installs the pricer on every LLM adapter of the current registry and warms the price
// lists, so the first estimate after a start already uses real prices.
func (s *Service) priceAdapters(reg *Registry, doc config.Document) {
	reference := false
	for id, a := range reg.LLM {
		if p, ok := doc.Providers[id]; ok && p.Kind == config.KindOpenAI {
			a.Price, a.CachedPrice, a.ContextPrice = s.pricer(id), s.cachedPricer(id), s.contextPricer(id)
			s.shareCaps(a, id, p)
			s.priceList(id) // loads the stored list; refreshes it in the background when stale
			reference = reference || openai.ResolveAPI(p.API, p.BaseURL) == openai.APICompatible
		}
	}
	if reference {
		s.priceList(referenceList) // hosts that may list no prices get the reference ready too
	}
}

// unpricedRoles are the model roles in use whose price is unknown: their calls count as free, so the
// monthly budget cannot stop them. The dashboard says so rather than letting the budget look safe.
func (s *Service) unpricedRoles() []string {
	if s.practice {
		return nil
	}
	var out []string
	for _, role := range config.AllModelRoles() {
		m, ok := s.Doc().Models[role]
		if !ok || m.Model == "" || role == "rerank" {
			continue
		}
		if a := s.Registry().LLM[m.Provider]; a == nil || !a.Configured() {
			continue // not in use without a key; the key is its own alert
		}
		if _, _, ok := s.modelPrice(m.Provider, m.Model); !ok {
			out = append(out, role)
		}
	}
	return out
}
