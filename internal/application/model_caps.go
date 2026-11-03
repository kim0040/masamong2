package application

import (
	"context"
	"slices"
	"sync"
	"time"

	"masamong/internal/config"
	"masamong/internal/providers/openai"
)

// Model reasoning capabilities (internal/providers/openai/caps.go). Adapters are made per turn, so what
// a model list said about the models (whether they think, the effort levels they take, how Anthropic
// turns their thinking off) is kept here, one cache per provider shared by all of them. It starts from
// the stored model list (storePrices keeps the capabilities next to the prices) and is filled by every
// list request and, on Anthropic-format hosts, by a one-time lookup of a model on its first call.

type capsSlot struct {
	key   string // the provider's address and family: a provider pointed elsewhere starts empty
	cache *openai.CapsCache
}

type capsState struct {
	mu    sync.Mutex
	slots map[string]*capsSlot
}

var capsStates sync.Map // *Service → *capsState

// modelCaps is provider's capability cache.
func (s *Service) modelCaps(provider string, p config.Provider) *openai.CapsCache {
	key := p.BaseURL + "\x00" + string(openai.ResolveAPI(p.API, p.BaseURL))
	v, _ := capsStates.LoadOrStore(s, &capsState{slots: map[string]*capsSlot{}})
	st := v.(*capsState)
	st.mu.Lock()
	slot := st.slots[provider]
	if slot != nil && slot.key == key {
		st.mu.Unlock()
		return slot.cache
	}
	slot = &capsSlot{key: key, cache: openai.NewCapsCache()}
	st.slots[provider] = slot
	st.mu.Unlock()
	if s.Store != nil {
		var stored priceList
		if ok, err := s.kvGet("prices:"+provider, &stored); ok && err == nil {
			for model, c := range stored.Caps {
				slot.cache.Put(model, c)
			}
		}
	}
	return slot.cache
}

// shareCaps gives an adapter its provider's capability cache.
func (s *Service) shareCaps(a *openai.Adapter, provider string, p config.Provider) {
	if a != nil {
		a.Caps = s.modelCaps(provider, p)
	}
}

// reasoningRow is what the console shows about a model's reasoning: reasoning true/false (absent when
// unknown), the effort levels it takes (absent when unknown) and, on Anthropic, how a thinking model's
// thinking is turned off ("disabled", "between_tools" or "never").
func reasoningRow(c openai.Caps) map[string]any {
	row := map[string]any{}
	if c.Reasoning != nil {
		row["reasoning"] = *c.Reasoning
	}
	if c.Efforts != nil {
		levels := []string{}
		for _, l := range config.ModelEfforts {
			// none and minimal are thinking off, which every model has one way or another
			if l == "none" || l == "minimal" || slices.Contains(c.Efforts, l) {
				levels = append(levels, l)
			}
		}
		row["efforts"] = levels
	}
	if off := c.ThinkingOff(); off != "" && !c.NoReasoning() {
		row["thinking_off"] = off
	}
	return row
}

// modelReasoning is the reasoning row of every chat role's model and fallback, keyed "provider/model",
// for the console's Models table. A model of an Anthropic-format host nothing is known about yet is
// looked up in the background (free, once a day), so the next refresh can show it.
func (s *Service) modelReasoning() map[string]any {
	doc := s.Doc()
	out := map[string]any{}
	var lookups []func(context.Context)
	add := func(provider, model string) {
		p, ok := doc.Providers[provider]
		if !ok || p.Kind != config.KindOpenAI || model == "" {
			return
		}
		cache := s.modelCaps(provider, p)
		c, known := cache.Get(model)
		out[provider+"/"+model] = reasoningRow(c)
		if !known && openai.ResolveAPI(p.API, p.BaseURL) == openai.APIAnthropic && !s.practice && s.Registry() != nil {
			// the registry's adapter already shares this cache (priceAdapters)
			if a := s.Registry().LLM[provider]; a != nil && a.Configured() {
				lookups = append(lookups, func(ctx context.Context) { a.CapsOf(ctx, model) })
			}
		}
	}
	for _, role := range config.FallbackRoles {
		m, ok := doc.Models[role]
		if !ok {
			continue
		}
		add(m.Provider, m.Model)
		for _, f := range m.Fallbacks {
			add(f.Provider, f.Model)
		}
	}
	if len(lookups) > 0 {
		s.ownedGo("model capability lookup", func(ctx context.Context) {
			ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			for _, look := range lookups {
				look(ctx)
			}
		})
	}
	return out
}
