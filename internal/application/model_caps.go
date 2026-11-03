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
// list request and, on Anthropic-format hosts, by a lookup of the role models a provider test runs
// (lookupModelCaps, in the background after the test answers). A chat call or a status read never asks
// the host: what is not known stays unknown, and the request follows the shape learned from refusals
// (kept in the same cache, so every adapter of the provider uses it).

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
// unknown), the effort levels the console offers for it ("efforts", absent when unknown), the levels the
// model's own effort field takes ("native_efforts", Anthropic output_config.effort; absent when unknown)
// and, on Anthropic, how a thinking model's thinking is turned off ("disabled", "between_tools" or
// "never"). The offered levels are what the application can send, not only the native field: a model that
// thinks with a fixed budget (Anthropic thinking type "enabled" without "adaptive", Claude Haiku 4.5)
// takes no effort field, yet every level from low up is a budget of its own (anthropic.go thinkingRoom).
func reasoningRow(c openai.Caps) map[string]any {
	row := map[string]any{}
	if c.Reasoning != nil {
		row["reasoning"] = *c.Reasoning
	}
	budgeted := slices.Contains(c.Thinking, "enabled") && !slices.Contains(c.Thinking, "adaptive") && !c.NoReasoning()
	if c.Efforts != nil || budgeted {
		levels := []string{}
		for _, l := range config.ModelEfforts {
			// on Anthropic none and minimal are thinking off, which its models have one way or another;
			// elsewhere the list says (an OpenRouter model that must think, GPT-6 Sol, Gemini 3.x/4 take
			// no none: it is sent as their lowest level)
			off := l == "none" || l == "minimal"
			if (off && c.Thinking != nil) || (!off && budgeted) || slices.Contains(c.Efforts, l) {
				levels = append(levels, l)
			}
		}
		row["efforts"] = levels
	}
	if c.Efforts != nil {
		row["native_efforts"] = append([]string{}, c.Efforts...)
	}
	if off := c.ThinkingOff(); off != "" && !c.NoReasoning() {
		row["thinking_off"] = off
	}
	return row
}

// modelReasoning is the reasoning row of every chat role's model and fallback, keyed "provider/model",
// for the console's Models table: what is known now, without asking any host.
func (s *Service) modelReasoning() map[string]any {
	doc := s.Doc()
	out := map[string]any{}
	add := func(provider, model string) {
		p, ok := doc.Providers[provider]
		if !ok || p.Kind != config.KindOpenAI || model == "" {
			return
		}
		c, known := s.modelCaps(provider, p).Get(model)
		if l := openai.HostEfforts(p.BaseURL, model); !known && l != nil {
			c = openai.Caps{Efforts: l} // what the host's docs give (GPT-6 Sol takes no none)
		}
		out[provider+"/"+model] = reasoningRow(c)
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
	return out
}

// lookupModelCaps asks an Anthropic-format provider about the role models on it nothing is known about
// yet (one GET /models/{id} each, free), when the operator tests the provider (in the background, after
// the test's answer): a host whose list the test read already answered for its listed models, and a host
// without a list may still answer per model.
func (s *Service) lookupModelCaps(ctx context.Context, provider string) {
	doc := s.Doc()
	p, ok := doc.Providers[provider]
	if !ok || p.Kind != config.KindOpenAI || openai.ResolveAPI(p.API, p.BaseURL) != openai.APIAnthropic || s.practice || s.Registry() == nil {
		return
	}
	a := s.Registry().LLM[provider] // shares the provider's cache (priceAdapters)
	if a == nil || !a.Configured() {
		return
	}
	cache := s.modelCaps(provider, p)
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	seen := map[string]bool{}
	for _, role := range config.FallbackRoles {
		m, ok := doc.Models[role]
		if !ok {
			continue
		}
		models := []string{}
		if m.Provider == provider {
			models = append(models, m.Model)
		}
		for _, f := range m.Fallbacks {
			if f.Provider == provider {
				models = append(models, f.Model)
			}
		}
		for _, model := range models {
			if _, known := cache.Get(model); known || model == "" || seen[model] {
				continue
			}
			seen[model] = true
			a.CapsOf(ctx, model)
		}
	}
}
