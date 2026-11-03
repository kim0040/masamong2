package openai

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

// Reasoning capabilities. A model list may say whether a model thinks and at which levels:
//
//	anthropic   capabilities.thinking {supported, types{adaptive,enabled,disabled,between_tools}}
//	            and capabilities.effort {supported, low…max} (GET /v1/models and /v1/models/{id})
//	openrouter  supported_parameters holds "reasoning" or "include_reasoning"; reasoning {mandatory,
//	            supported_efforts} (live 2026-11-03: null for models that do not reason)
//	nanogpt     capabilities.reasoning and reasoning_efforts (its detailed list, ?detailed=true)
//	gemini      thinking (its native list; the OpenAI-compatible list this adapter reads names only)
//	openai      nothing: unknown
//
// What a list says is kept per model (CapsCache) and decides the request: a model known not to think
// gets no effort or thinking field, a level it does not take is clamped to the nearest one it does, and
// on Anthropic the way thinking is turned off is the one the model takes. Nothing known leaves the
// request to the learned shapes (shape.go), which drop a refused field after one refusal.

// EffortLevels are the reasoning levels a request may ask for, least thinking first.
var EffortLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}

// Caps is what a model list says about a model's reasoning. Nil and empty differ: nil is unknown. The
// lists are stored with the price list (JSON) without omitempty, so a known-empty list ([]) comes back
// empty after a restart and an unknown one (null) comes back nil.
type Caps struct {
	// Reasoning is whether the model thinks (or takes a reasoning field); nil when the list does not say
	Reasoning *bool `json:"reasoning,omitempty"`
	// Efforts are the levels the model takes (Anthropic output_config.effort), least first; nil unknown,
	// empty when the model takes no level at all
	Efforts []string `json:"efforts"`
	// Thinking are the Anthropic thinking types the model takes ("adaptive", "enabled", "disabled",
	// "between_tools"); nil unknown
	Thinking []string `json:"thinking"`
}

// Known reports whether the list said anything about the model's reasoning.
func (c Caps) Known() bool { return c.Reasoning != nil || c.Efforts != nil || c.Thinking != nil }

// ThinkingOff is how thinking is turned off on Anthropic: "disabled", else "between_tools" (thinks only
// between tool calls), else "never" (the model cannot stop thinking); "" when unknown.
func (c Caps) ThinkingOff() string {
	switch {
	case c.Thinking == nil:
		return ""
	case slices.Contains(c.Thinking, "disabled"):
		return "disabled"
	case slices.Contains(c.Thinking, "between_tools"):
		return "between_tools"
	}
	return "never"
}

// NoReasoning reports a model the list says does not think.
func (c Caps) NoReasoning() bool { return c.Reasoning != nil && !*c.Reasoning }

// Caps is the model's reasoning capabilities as the list gave them.
func (m Model) Caps() Caps {
	return Caps{Reasoning: m.Reasoning, Efforts: m.Efforts, Thinking: m.Thinking}
}

// parseCaps reads the reasoning fields of one list item (any family; each field is read only where a
// family documents it).
func parseCaps(it map[string]json.RawMessage, m *Model) {
	if raw, ok := it["capabilities"]; ok {
		var c map[string]json.RawMessage
		if json.Unmarshal(raw, &c) == nil {
			parseAnthropicCaps(c, m)
			var b bool
			if m.Reasoning == nil && json.Unmarshal(c["reasoning"], &b) == nil { // NanoGPT's detailed list
				m.Reasoning = &b
			}
		}
	}
	if sp, ok := it["supported_parameters"]; ok { // OpenRouter
		var params []string
		if json.Unmarshal(sp, &params) == nil {
			has := slices.Contains(params, "reasoning") || slices.Contains(params, "include_reasoning")
			m.Reasoning, m.paramsNoReasoning = &has, !has
		}
	}
	if raw, ok := it["reasoning"]; ok && string(raw) != "null" { // OpenRouter's reasoning object
		var r struct {
			Mandatory bool     `json:"mandatory"`
			Efforts   []string `json:"supported_efforts"`
		}
		if json.Unmarshal(raw, &r) == nil {
			yes := true
			m.Reasoning, m.paramsNoReasoning = &yes, false
			if r.Efforts != nil {
				// a model that may stop thinking takes none even when its list leaves it out
				m.Efforts = orderedEfforts(r.Efforts, !r.Mandatory)
			}
		}
	}
	if raw, ok := it["reasoning_efforts"]; ok && string(raw) != "null" { // NanoGPT's detailed list
		var levels []string
		if json.Unmarshal(raw, &levels) == nil && levels != nil {
			m.Efforts = orderedEfforts(levels, false)
		}
	}
	var b bool
	if m.Reasoning == nil && json.Unmarshal(it["thinking"], &b) == nil { // Gemini's native model resource
		m.Reasoning = &b
	}
}

// parseAnthropicCaps reads Anthropic's capabilities object (thinking, effort, image_input).
func parseAnthropicCaps(c map[string]json.RawMessage, m *Model) {
	type flag struct {
		Supported *bool `json:"supported"`
	}
	if raw, ok := c["thinking"]; ok {
		var th struct {
			Supported *bool           `json:"supported"`
			Types     map[string]flag `json:"types"`
		}
		if json.Unmarshal(raw, &th) == nil && th.Supported != nil {
			m.Reasoning = th.Supported
			m.Thinking = []string{}
			for _, t := range []string{"adaptive", "enabled", "disabled", "between_tools"} {
				if f, ok := th.Types[t]; ok && f.Supported != nil && *f.Supported {
					m.Thinking = append(m.Thinking, t)
				}
			}
		}
	}
	if raw, ok := c["effort"]; ok {
		var levels map[string]json.RawMessage
		if json.Unmarshal(raw, &levels) == nil {
			var all flag
			_ = json.Unmarshal(raw, &all)
			if all.Supported != nil {
				m.Efforts = []string{}
				for _, l := range EffortLevels {
					var f flag
					if *all.Supported && json.Unmarshal(levels[l], &f) == nil && f.Supported != nil && *f.Supported {
						m.Efforts = append(m.Efforts, l)
					}
				}
			}
		}
	}
	if raw, ok := c["image_input"]; ok && m.ImageInput == nil {
		var f flag
		if json.Unmarshal(raw, &f) == nil && f.Supported != nil {
			m.ImageInput = f.Supported
		}
	}
}

// orderedEfforts is levels in EffortLevels order, unknown names dropped; none added when withNone.
func orderedEfforts(levels []string, withNone bool) []string {
	out := []string{}
	for _, l := range EffortLevels {
		if slices.Contains(levels, l) || (withNone && l == "none") {
			out = append(out, l)
		}
	}
	return out
}

// clampEffort is the level of levels nearest to effort, rounding down first (a model without "xhigh"
// gets "high", one without "low" its lowest); ok is false when levels is empty. A level levels holds is
// returned unchanged.
func clampEffort(effort string, levels []string) (string, bool) {
	if len(levels) == 0 {
		return "", false
	}
	if slices.Contains(levels, effort) {
		return effort, true
	}
	at := slices.Index(EffortLevels, effort)
	if at < 0 {
		return levels[0], true
	}
	for i := at - 1; i >= 0; i-- {
		if slices.Contains(levels, EffortLevels[i]) {
			return EffortLevels[i], true
		}
	}
	for i := at + 1; i < len(EffortLevels); i++ {
		if slices.Contains(levels, EffortLevels[i]) {
			return EffortLevels[i], true
		}
	}
	return levels[0], true
}

// CapsCache keeps the capabilities model lists gave, per model, and the request shapes learned from the
// models' refusals (shape.go). The application shares one per provider across the adapters it builds
// (they are made per turn), so a refusal learned in one turn holds in the next; an adapter without one
// keeps its own. A model's learned shapes are dropped when its capabilities change (Put): a refusal read
// while they were unknown or different may be wrong for what is known now.
type CapsCache struct {
	mu      sync.Mutex
	entries map[string]capsEntry
	noted   map[string]bool  // clamps already logged (model, asked, sent)
	looking map[string]bool  // Anthropic lookups in flight
	shapes  map[string]shape // shapeKey → the optional fields that model refused
}

type capsEntry struct {
	caps Caps
	at   time.Time
	miss bool // the lookup failed: unknown until missTTL has passed
}

// capsTTL is how long a looked-up model's capabilities are used before it is asked again; a failed
// lookup waits missTTL (a host without a model list answers 404 every time).
const (
	capsTTL = 24 * time.Hour
	missTTL = time.Hour
)

// NewCapsCache is an empty cache.
func NewCapsCache() *CapsCache {
	return &CapsCache{entries: map[string]capsEntry{}, noted: map[string]bool{}, looking: map[string]bool{}, shapes: map[string]shape{}}
}

// Put records what a list said about model. A list that says nothing does not erase an earlier answer.
// Capabilities other than the ones known before drop the model's learned shapes.
func (c *CapsCache) Put(model string, caps Caps) {
	if c == nil || !caps.Known() {
		return
	}
	c.mu.Lock()
	c.replace(model, caps)
	c.mu.Unlock()
}

// replace stores caps for model, dropping the model's learned shapes when they differ from the caps known
// before (an unchanged list read again keeps them: the route's refusals still hold). c.mu is held.
func (c *CapsCache) replace(model string, caps Caps) {
	if old, ok := c.entries[model]; !ok || old.miss || !sameCaps(old.caps, caps) {
		delete(c.shapes, model)
		delete(c.shapes, model+"\x00tools")
	}
	c.entries[model] = capsEntry{caps: caps, at: now()}
}

// sameCaps reports equal capabilities, an unknown (nil) list differing from a known-empty one.
func sameCaps(x, y Caps) bool {
	same := func(a, b []string) bool { return (a == nil) == (b == nil) && slices.Equal(a, b) }
	return (x.Reasoning == nil) == (y.Reasoning == nil) && (x.Reasoning == nil || *x.Reasoning == *y.Reasoning) &&
		same(x.Efforts, y.Efforts) && same(x.Thinking, y.Thinking)
}

// shape is the shape learned for key (shapeKey); ok is false when none is.
func (c *CapsCache) shape(key string) (shape, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	sh, ok := c.shapes[key]
	return sh, ok
}

// setShape keeps the shape learned for key (shapeKey).
func (c *CapsCache) setShape(key string, sh shape) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.shapes == nil {
		c.shapes = map[string]shape{}
	}
	c.shapes[key] = sh
}

// Get is what is known about model; ok is false when nothing is.
func (c *CapsCache) Get(model string) (Caps, bool) {
	if c == nil {
		return Caps{}, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.entries[model]
	return e.caps, ok && !e.miss
}

// due reports whether model should be looked up now (nothing fresh known, no lookup in flight), and
// marks the lookup as in flight when it should.
func (c *CapsCache) due(model string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.looking[model] {
		return false
	}
	if e, ok := c.entries[model]; ok {
		ttl := capsTTL
		if e.miss {
			ttl = missTTL
		}
		if now().Sub(e.at) < ttl {
			return false
		}
	}
	c.looking[model] = true
	return true
}

func (c *CapsCache) done(model string, caps Caps, miss bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.looking, model)
	if miss {
		if old, ok := c.entries[model]; ok && !old.miss {
			old.at = now() // keep a list's answer; ask again after a day
			c.entries[model] = old
			return
		}
		c.entries[model] = capsEntry{at: now(), miss: true}
		return
	}
	c.replace(model, caps)
}

// note reports whether key is seen for the first time (a clamp is logged once, not per call).
func (c *CapsCache) note(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.noted[key] {
		return false
	}
	c.noted[key] = true
	return true
}

// caps is the adapter's cache, made on first use for an adapter the application gave none.
func (a *Adapter) caps() *CapsCache {
	a.capsOnce.Do(func() {
		if a.Caps == nil {
			a.Caps = NewCapsCache()
		}
	})
	return a.Caps
}

// capsLookupTimeout bounds one model lookup (CapsOf).
const capsLookupTimeout = 5 * time.Second

// knownCaps is what is known about model without asking the host: what a list said, else the levels
// the host's docs give (hosts.json model_efforts).
func (a *Adapter) knownCaps(model string) Caps {
	if c, ok := a.caps().Get(model); ok {
		return c
	}
	if l := hostEfforts(a.BaseURL, model); l != nil {
		return Caps{Efforts: l}
	}
	return Caps{}
}

// CapsOf is what is known about model's reasoning. On Anthropic a model nothing is known about is
// looked up (GET /models/{id}, free) and the answer kept for a day; elsewhere only what a model list
// said is known. Only an operator's explicit action asks (a provider test): a chat call and a status
// read use what is known already (knownCaps) and leave the rest to the learned shapes.
func (a *Adapter) CapsOf(ctx context.Context, model string) Caps {
	cache := a.caps()
	if a.API != APIAnthropic {
		return a.knownCaps(model)
	}
	if !a.Configured() || model == "" || !cache.due(model) {
		c, _ := cache.Get(model)
		return c
	}
	lctx, cancel := context.WithTimeout(ctx, capsLookupTimeout)
	defer cancel()
	m, err := a.anthropicModel(lctx, model)
	if err != nil && ctx.Err() != nil { // our caller gave up: not an answer about the model
		cache.mu.Lock()
		delete(cache.looking, model)
		cache.mu.Unlock()
		c, _ := cache.Get(model)
		return c
	}
	cache.done(model, m.Caps(), err != nil || !m.Caps().Known())
	c, _ := cache.Get(model)
	return c
}

// anthropicModel reads one model of an Anthropic-format host (GET /models/{id}).
func (a *Adapter) anthropicModel(ctx context.Context, model string) (Model, error) {
	resp, err := a.Client.Get(ctx, a.anthropicBase()+"/models/"+url.PathEscape(model), a.anthropicHeaders())
	if err != nil {
		return Model{}, mapErr(err, resp)
	}
	obj, err := decode.Object(resp.ContentType, resp.Body)
	if err != nil {
		return Model{}, err
	}
	if decode.Str(obj["id"]) == "" {
		return Model{}, errors.New("openai: model lookup without an id")
	}
	m := Model{ID: decode.Str(obj["id"])}
	parseCaps(obj, &m)
	return m, nil
}

// ErrNoModelList is a host of the Anthropic format without a model list of that format (many
// Anthropic-compatible endpoints only take /v1/messages): the operator types the model ID instead, and a
// provider test reports the list as unavailable rather than failing.
var ErrNoModelList = errors.New("openai: this host has no model list")

// listRouteMissing reports an Anthropic-format host's answer to the list request that means "no list
// here" (noModelList). A 403 is read as a refused key everywhere else, but a gateway that serves
// /v1/messages only may refuse the list route with one: on a host other than Anthropic's own, a 403
// without a key error's words is no list (the provider test then proves the key with a chat call).
func listRouteMissing(err error, resp *providers.Response, baseURL string) bool {
	if !errors.Is(err, ErrAuth) {
		return noModelList(err)
	}
	if resp == nil || resp.Status != 403 || officialAnthropic(baseURL) {
		return false
	}
	return !hasAny(strings.ToLower(string(resp.Body)), authMarks)
}

// noModelList reports a list request a host without a list answers: 400/403/404/405 (a gateway that
// routes /v1/messages only refuses an unknown route in any of these ways), or a reply of another shape. A
// refused key is not one of these (the caller checks ErrAuth first: a 400 or 403 with a key error's words).
func noModelList(err error) bool {
	gone := func(status int) bool { return status == 400 || status == 403 || status == 404 || status == 405 }
	var pe *providers.Error
	if errors.As(err, &pe) && gone(pe.Status) {
		return true
	}
	var ae *APIError
	if errors.As(err, &ae) && gone(ae.Status) {
		return true
	}
	return errors.Is(err, decode.ErrUnexpected)
}
