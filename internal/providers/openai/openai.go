// Package openai adapts OpenAI-compatible chat APIs (OpenRouter, OpenAI, Gemini, DeepSeek…).
// "Compatible" is loose: request fields differ per host family (api.go), model lists differ in fields,
// errors differ in envelope, and some hosts wrap usage differently. Every response goes through
// strict shape validation here.
package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"masamong/internal/errclass"
	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

type Adapter struct {
	Client  *providers.Client
	BaseURL string // e.g. https://openrouter.ai/api/v1
	Cred    providers.Credential
	API     API               // host family; New detects it from BaseURL, config can name it
	Headers map[string]string // optional extra headers (OpenRouter HTTP-Referer / X-Title)
	// ByokOnly (OpenRouter) maps a model family (before "/") to the only upstream provider its chat
	// requests may use, with no fallback (config providers.<id>.byok_only).
	ByokOnly map[string]string
	// Price fills CostUSD from token counts when the host does not report usage.cost (every host but
	// OpenRouter). Nil leaves such costs unknown.
	Price Pricer
	// CachedPrice prices input tokens the host served from its prompt cache (OpenAI, Gemini and
	// DeepSeek bill them at a fraction of the input price). Nil, or unknown for a model, prices them as
	// ordinary input: an estimate errs on the side of the budget.
	CachedPrice CachedPricer
	// ContextPrice evaluates conditional rates using total prompt tokens and the request instant.
	// Price remains the conservative fallback for callers that do not provide request context.
	ContextPrice ContextPricer

	shapes sync.Map // model → shape: optional request fields that model refused (shape.go)
	// shapeSeen marks models whose plain chat request (no tools) succeeded once, so their shape is known
	// even when it refused nothing (ShapeLearned); model → true
	shapeSeen sync.Map
	// noneOff marks request kinds (shapeKey) whose effort "none" request worked only without the effort
	// field. A live request learns it again each time (the model may take "low"); a batch item cannot, so
	// its body leaves the field out (batchShapeOf); shapeKey → true
	noneOff sync.Map
	// imageAPI remembers models of a compatible host that refused a chat image request and answer on
	// /images/generations instead (NanoGPT's image models); model → true
	imageAPI sync.Map
	// toolsOff remembers models that refused a request carrying tools (tools.go); model → true
	toolsOff sync.Map
	// visionOff remembers models that refused a request carrying pictures (vision.go); model → when
	visionOff sync.Map
	// Failover, when set, sends a failed chat request for its model on to the fallbacks (failover.go).
	Failover *Failover
	// Gate, when set, is asked before every chat, embedding, rerank and image call (not before key
	// checks or model lists): the operator's daily call limit for this provider. Its error ends the
	// call; with a Failover the next model is asked.
	Gate func() error
}

func (a *Adapter) gate() error {
	if a.Gate == nil {
		return nil
	}
	return a.Gate()
}

// Pricer returns a model's price in USD per token (input, output); ok is false when unknown.
type Pricer func(model string) (in, out float64, ok bool)

// ContextPricer evaluates a model's input, output and cached-input rates for one request.
type ContextPricer func(model string, promptTokens int, at time.Time) (in, out, cached float64, ok bool)

// CachedPricer returns a model's price in USD per cached input token; ok is false when unknown.
type CachedPricer func(model string) (cached float64, ok bool)

// estimate sets *cost from tokens × price when the host reported none.
func (a *Adapter) estimate(cost **float64, estimated *bool, model string, in, out int) {
	a.estimateAt(cost, estimated, model, in, out, time.Now())
}

func (a *Adapter) estimateAt(cost **float64, estimated *bool, model string, in, out int, at time.Time) {
	a.estimateCachedAt(cost, estimated, model, in, 0, out, at)
}

// estimateCached is estimate with cached of the in tokens served from the host's prompt cache.
func (a *Adapter) estimateCached(cost **float64, estimated *bool, model string, in, cached, out int) {
	a.estimateCachedAt(cost, estimated, model, in, cached, out, time.Now())
}

func (a *Adapter) estimateCachedAt(cost **float64, estimated *bool, model string, in, cached, out int, at time.Time) {
	if *cost != nil || (a.Price == nil && a.ContextPrice == nil) || in+out == 0 {
		return
	}
	var pin, pout, pc float64
	var ok, contextual bool
	if a.ContextPrice != nil {
		pin, pout, pc, ok = a.ContextPrice(model, in, at)
		contextual = ok
	}
	if !ok {
		pin, pout, ok = a.Price(model)
		pc = pin
		if a.CachedPrice != nil {
			if p, found := a.CachedPrice(model); found {
				pc = p
			}
		}
	}
	if !ok {
		return
	}
	if contextual && pc <= 0 {
		pc = pin
	}
	cached = min(max(cached, 0), in)
	v := float64(in-cached)*pin + float64(cached)*pc + float64(out)*pout
	*cost, *estimated = &v, true
}

func New(client *providers.Client, baseURL string, cred providers.Credential) *Adapter {
	return &Adapter{Client: client, BaseURL: strings.TrimRight(baseURL, "/"), Cred: cred, API: DetectAPI(baseURL)}
}

func (a *Adapter) Configured() bool { return a.Cred.Present() && a.BaseURL != "" }

// anthropicVersion is the API version Anthropic's endpoints are asked for.
const anthropicVersion = "2023-06-01"

func (a *Adapter) headers() map[string]string {
	h := map[string]string{"Authorization": "Bearer " + a.Cred.Value()}
	if a.API == APIAnthropic {
		h["anthropic-version"] = anthropicVersion // its model list refuses a request without one
	}
	for k, v := range a.Headers {
		h[k] = v
	}
	return h
}

// ---------------------------------------------------------------------------
// Models
// ---------------------------------------------------------------------------

type Model struct {
	ID              string
	Name            string
	ContextLength   int
	PromptPrice     string // decimal string per token; "" when unknown (never "0")
	CompletionPrice string
	CachedPrice     string // per cached input token (OpenRouter pricing.input_cache_read); "" when not listed
	Kind            string // chat | embedding | unknown
	ImageOutput     bool   // writes images (OpenRouter architecture.output_modalities): the image role
	// SupportsTools is whether the host's list says the model takes tools (OpenRouter's
	// supported_parameters); nil when the list does not say.
	SupportsTools *bool
	// ImageInput is whether the host's list says the model reads pictures (OpenRouter's
	// architecture.input_modalities holds "image"); nil when the list does not say (name-based lists).
	ImageInput *bool
	Overrides  []PricingOverride
}

// PricingOverride is one OpenRouter conditional rate entry. A missing price inherits the base rate.
type PricingOverride struct {
	MinPromptTokens *int     `json:"min_prompt_tokens,omitempty"`
	UTCStart        *int     `json:"utc_start,omitempty"`
	UTCEnd          *int     `json:"utc_end,omitempty"`
	UTCDays         []string `json:"utc_days,omitempty"`
	PromptPrice     *float64 `json:"prompt,omitempty"`
	CompletionPrice *float64 `json:"completion,omitempty"`
	CachedPrice     *float64 `json:"input_cache_read,omitempty"`
	CacheWritePrice *float64 `json:"input_cache_write,omitempty"`
	ImagePrice      *float64 `json:"image_output,omitempty"`
}

// Applies reports whether all conditions on this entry match the request.
func (o PricingOverride) Applies(promptTokens int, at time.Time) bool {
	if o.MinPromptTokens != nil && promptTokens <= *o.MinPromptTokens {
		return false
	}
	if len(o.UTCDays) > 0 {
		day := strings.ToLower(at.UTC().Weekday().String())
		if !slices.Contains(o.UTCDays, day) {
			return false
		}
	}
	if o.UTCStart != nil && o.UTCEnd != nil {
		clock := at.UTC().Hour()*100 + at.UTC().Minute()
		start, end := *o.UTCStart, *o.UTCEnd
		if start <= end {
			if clock < start || clock >= end {
				return false
			}
		} else if clock < start && clock >= end {
			return false
		}
	}
	return true
}

// ErrAuth is a refused key. It is class Auth for errclass.
var ErrAuth error = classedError{msg: "openai: unauthorized", class: errclass.Auth}

func (a *Adapter) ListModels(ctx context.Context) ([]Model, error) {
	if !a.Configured() {
		return nil, providers.ErrNoCredential
	}
	url := a.BaseURL + "/models"
	switch a.API {
	case APIOpenRouter:
		url += "?output_modalities=all" // embedding and rerank models are listed only with this
	case APICompatible:
		url += "?detailed=true" // NanoGPT lists prices only then; other hosts ignore the parameter
	case APIAnthropic:
		url += "?limit=1000" // its list comes in pages of 20 by default
	}
	resp, err := a.Client.Get(ctx, url, a.headers())
	if err != nil {
		return nil, mapErr(err, resp)
	}
	models, err := ParseModels(resp.ContentType, resp.Body)
	if err != nil || a.API == APIOpenRouter {
		return models, err
	}
	// Other hosts list names only: what a model does is read from its name, and Gemini's
	// "models/" prefix is dropped so the ID is the one config and prices use.
	out := models[:0]
	for _, m := range models {
		if m.Name == m.ID {
			m.Name = a.API.trimModelID(m.ID)
		}
		m.ID = a.API.trimModelID(m.ID)
		m.Kind, m.ImageOutput = ModelKind(m.ID)
		out = append(out, m)
	}
	return out, nil
}

// CheckKey proves the credential with a request that needs it and costs nothing. OpenRouter lists its
// models without any key, so there the key's own info (GET /key) is asked for; elsewhere the model list
// needs the key.
func (a *Adapter) CheckKey(ctx context.Context) error {
	if !a.Configured() {
		return providers.ErrNoCredential
	}
	if a.API != APIOpenRouter {
		_, err := a.ListModels(ctx)
		return err
	}
	resp, err := a.Client.Get(ctx, a.BaseURL+"/key", a.headers())
	if err != nil {
		return mapErr(err, resp)
	}
	obj, err := decode.Object(resp.ContentType, resp.Body)
	if err != nil {
		return err
	}
	if e, ok := obj["error"]; ok {
		return &authError{detail: apiError(e, 0).Error()}
	}
	return nil
}

// KeyCredit is what OpenRouter's GET /key says about the key's own spending limit: Limit and Remaining
// are nil for a key without one; Reset is "" for a lifetime limit (the dashboard's default), else
// "daily", "weekly" or "monthly".
type KeyCredit struct {
	Limit     *float64 `json:"limit"`
	Remaining *float64 `json:"remaining"`
	Usage     float64  `json:"usage"`
	Reset     string   `json:"reset,omitempty"`
}

// ErrNoCredit is returned by Credit for a host that reports no key limit (every host but OpenRouter).
var ErrNoCredit = errors.New("openai: the host reports no key credit")

// Credit reads the key's spending limit. A key that runs out answers every call with 403 "Key limit
// exceeded" while the bot's own monthly budget still has room (2026-10-14: a lifetime $20 limit hit at
// night), so the console warns before that.
func (a *Adapter) Credit(ctx context.Context) (KeyCredit, error) {
	if a.API != APIOpenRouter {
		return KeyCredit{}, ErrNoCredit
	}
	if !a.Configured() {
		return KeyCredit{}, providers.ErrNoCredential
	}
	resp, err := a.Client.Get(ctx, a.BaseURL+"/key", a.headers())
	if err != nil {
		return KeyCredit{}, mapErr(err, resp)
	}
	var body struct {
		Data struct {
			Limit          *float64 `json:"limit"`
			LimitRemaining *float64 `json:"limit_remaining"`
			Usage          float64  `json:"usage"`
			LimitReset     *string  `json:"limit_reset"`
		} `json:"data"`
	}
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		return KeyCredit{}, err
	}
	c := KeyCredit{Limit: body.Data.Limit, Remaining: body.Data.LimitRemaining, Usage: body.Data.Usage}
	if body.Data.LimitReset != nil {
		c.Reset = *body.Data.LimitReset
	}
	if c.Limit == nil {
		c.Remaining = nil
	}
	return c, nil
}

// ListIsPublic reports whether the host's model list answers without a key (NanoGPT's does). There a
// listed answer proves nothing about the key; ProbeChat does.
func (a *Adapter) ListIsPublic(ctx context.Context) bool {
	h := map[string]string{}
	for k, v := range a.Headers {
		h[k] = v
	}
	resp, err := a.Client.Get(ctx, a.BaseURL+"/models", h)
	if err != nil {
		return false
	}
	_, err = ParseModels(resp.ContentType, resp.Body)
	return err == nil
}

// ProbeChat proves the key with the smallest request a chat model takes (one token, a fraction of a
// cent). Only a refused key is an error: a model that wants more tokens or other fields still showed
// that the key works.
func (a *Adapter) ProbeChat(ctx context.Context, model string) error {
	_, err := a.Chat(ctx, ChatRequest{Model: model, Messages: []Message{{Role: "user", Content: "hi"}}, MaxTokens: 1})
	if errors.Is(err, ErrAuth) || errors.Is(err, providers.ErrNoCredential) {
		return err
	}
	return nil
}

// ParseModels accepts {"data":[...]} (OpenAI/OpenRouter) and a bare list. Unknown price stays "".
func ParseModels(contentType string, body []byte) ([]Model, error) {
	var raw json.RawMessage
	if decode.Sniff(contentType, body) == decode.JSON && strings.HasPrefix(strings.TrimSpace(string(body)), "[") {
		raw = body
	} else {
		obj, err := decode.Object(contentType, body)
		if err != nil {
			return nil, err
		}
		if e, ok := obj["error"]; ok {
			return nil, apiError(e, 0)
		}
		raw = obj["data"]
		if raw == nil {
			return nil, fmt.Errorf("%w: no data list", decode.ErrUnexpected)
		}
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &items); err != nil {
		return nil, fmt.Errorf("%w: data: %v", decode.ErrUnexpected, err)
	}
	out := make([]Model, 0, len(items))
	for _, it := range items {
		id := decode.Str(it["id"])
		if id == "" {
			continue
		}
		m := Model{ID: id, Name: decode.Str(it["name"]), Kind: "unknown"}
		if m.Name == "" {
			m.Name = decode.Str(it["display_name"]) // Gemini
		}
		if m.Name == "" {
			m.Name = id
		}
		if n, ok := decode.Number(it["context_length"]); ok {
			m.ContextLength = int(n)
		} else if n, ok := decode.Number(it["max_input_tokens"]); ok {
			m.ContextLength = int(n) // Anthropic
		}
		if p, ok := it["pricing"]; ok {
			var pr map[string]json.RawMessage
			if json.Unmarshal(p, &pr) == nil {
				m.PromptPrice = priceStr(pr["prompt"])
				m.CompletionPrice = priceStr(pr["completion"])
				m.CachedPrice = priceStr(pr["input_cache_read"])
				m.Overrides = parsePricingOverrides(pr["overrides"])
				// NanoGPT's detailed list: numbers per million tokens, {"unit":"per_million_tokens"}
				if strings.Contains(strings.ToLower(decode.Str(pr["unit"])), "million") {
					m.PromptPrice, m.CompletionPrice = perToken(m.PromptPrice), perToken(m.CompletionPrice)
					for i := range m.Overrides {
						m.Overrides[i].PromptPrice = scalePrice(m.Overrides[i].PromptPrice)
						m.Overrides[i].CompletionPrice = scalePrice(m.Overrides[i].CompletionPrice)
						m.Overrides[i].CachedPrice = scalePrice(m.Overrides[i].CachedPrice)
						m.Overrides[i].CacheWritePrice = scalePrice(m.Overrides[i].CacheWritePrice)
						m.Overrides[i].ImagePrice = scalePrice(m.Overrides[i].ImagePrice)
					}
				}
				// an image model's output is mostly picture tokens, priced on their own (OpenRouter's
				// image_output: $60/M for gemini-3.1-flash-image against $3/M for its text)
				if img := priceStr(pr["image_output"]); img != "" {
					if v, err := strconv.ParseFloat(img, 64); err == nil {
						if c, err := strconv.ParseFloat(m.CompletionPrice, 64); err != nil || v > c {
							m.CompletionPrice = img
						}
					}
				}
			}
		}
		if a, ok := it["architecture"]; ok {
			var arch struct {
				Input  []string `json:"input_modalities"`
				Output []string `json:"output_modalities"`
			}
			if json.Unmarshal(a, &arch) == nil {
				m.ImageOutput = slices.Contains(arch.Output, "image")
				if arch.Input != nil { // a list that names no input modalities says nothing either way
					has := slices.Contains(arch.Input, "image")
					m.ImageInput = &has
				}
			}
		}
		if sp, ok := it["supported_parameters"]; ok {
			var params []string
			if json.Unmarshal(sp, &params) == nil {
				has := slices.Contains(params, "tools")
				m.SupportsTools = &has
			}
		}
		arch := decode.Str(it["object"])
		if strings.Contains(id, "embed") {
			m.Kind = "embedding"
		} else if strings.Contains(id, "rerank") {
			m.Kind = "rerank"
		} else if arch == "model" || m.ContextLength > 0 {
			m.Kind = "chat"
		}
		out = append(out, m)
	}
	return out, nil
}

// perToken turns a price per million tokens into one per token ("" stays unknown).
func perToken(perMillion string) string {
	v, err := strconv.ParseFloat(perMillion, 64)
	if err != nil {
		return ""
	}
	return strconv.FormatFloat(v/1e6, 'g', -1, 64)
}

func priceStr(raw json.RawMessage) string {
	s := decode.Str(raw)
	if s == "" {
		return ""
	}
	return s
}

func parsePricingOverrides(raw json.RawMessage) []PricingOverride {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var entries []json.RawMessage
	if json.Unmarshal(raw, &entries) != nil {
		return nil
	}
	known := map[string]bool{
		"min_prompt_tokens": true, "utc_start": true, "utc_end": true, "utc_days": true,
		"prompt": true, "completion": true, "input_cache_read": true, "input_cache_write": true, "image_output": true,
	}
	var out []PricingOverride
	for _, entry := range entries {
		var fields map[string]json.RawMessage
		if json.Unmarshal(entry, &fields) != nil || fields == nil {
			continue
		}
		valid := true
		for key := range fields {
			if !known[key] {
				valid = false // unknown condition fields must not make an override apply too broadly.
				break
			}
		}
		if !valid {
			continue
		}
		var override PricingOverride
		if value, ok := fields["min_prompt_tokens"]; ok {
			var n int
			if json.Unmarshal(value, &n) != nil || n < 0 {
				continue
			}
			override.MinPromptTokens = &n
		}
		for key, target := range map[string]**int{"utc_start": &override.UTCStart, "utc_end": &override.UTCEnd} {
			if value, ok := fields[key]; ok {
				var n int
				if json.Unmarshal(value, &n) != nil || !validHHMM(n) {
					valid = false
					break
				}
				*target = &n
			}
		}
		if !valid || (override.UTCStart == nil) != (override.UTCEnd == nil) {
			continue
		}
		if value, ok := fields["utc_days"]; ok {
			var days []string
			if json.Unmarshal(value, &days) != nil || len(days) == 0 {
				continue
			}
			for i, day := range days {
				days[i] = strings.ToLower(strings.TrimSpace(day))
				if !slices.Contains([]string{"monday", "tuesday", "wednesday", "thursday", "friday", "saturday", "sunday"}, days[i]) {
					valid = false
					break
				}
			}
			if !valid {
				continue
			}
			override.UTCDays = days
		}
		for key, target := range map[string]**float64{
			"prompt": &override.PromptPrice, "completion": &override.CompletionPrice,
			"input_cache_read": &override.CachedPrice, "input_cache_write": &override.CacheWritePrice, "image_output": &override.ImagePrice,
		} {
			if value, ok := fields[key]; ok {
				price, err := strconv.ParseFloat(priceStr(value), 64)
				if err != nil || price < 0 || math.IsNaN(price) || math.IsInf(price, 0) {
					valid = false
					break
				}
				*target = &price
			}
		}
		if valid {
			out = append(out, override)
		}
	}
	return out
}

func validHHMM(value int) bool {
	return value >= 0 && value <= 2359 && value%100 < 60
}

func scalePrice(price *float64) *float64 {
	if price == nil {
		return nil
	}
	value := *price / 1e6
	return &value
}

// ---------------------------------------------------------------------------
// Chat completions
// ---------------------------------------------------------------------------

type ChatRequest struct {
	Model       string
	Messages    []Message
	MaxTokens   int
	Temperature *float64
	// Effort asks a reasoning model to think little ("low") so it does not spend the completion budget
	// thinking (docs/rag §4). It is sent the way the host family expects (api.go); a model that takes
	// no such field gets the request without it (shape.go).
	Effort string
	// Tools, ToolChoice ("auto", "none", "required" or an object naming one function) and
	// ParallelToolCalls are sent only when Tools is not empty (tools.go).
	Tools             []Tool
	ToolChoice        any
	ParallelToolCalls *bool
	// EmptyOK: an answer with no text and no tool calls that the model ended on purpose (finish_reason
	// stop, or none sent) is an answer, not a failure for a fallback to retry. A tool round is one: the model needs
	// no more tools, and the reply is written by another call.
	EmptyOK bool
}

type ChatResult struct {
	Content          string
	FinishReason     string
	Model            string
	PromptTokens     int
	CompletionTokens int
	// CachedTokens is the part of PromptTokens the host served from its prompt cache
	// (usage.prompt_tokens_details.cached_tokens; DeepSeek: usage.prompt_cache_hit_tokens)
	CachedTokens int
	Elapsed      time.Duration
	// CostUSD is the provider-reported usage.cost (OpenRouter), else tokens × Price; nil when neither is known.
	CostUSD       *float64
	CostEstimated bool
	// ToolCalls are the calls the model asked for (finish reason "tool_calls"); Content may then be empty,
	// which is an answer, not an error.
	ToolCalls []ToolCall
	// Message is the exact choices[0].message the host returned. Send it back as Message{Raw: …} to
	// continue a tool loop: provider-only fields (DeepSeek reasoning_content, Gemini thought signatures)
	// must come back unchanged.
	Message json.RawMessage
	// Provider is the config provider that answered and Fallback is true when it was a fallback; both
	// are set only when the adapter has a Failover. Attempts lists every model asked, in order.
	Provider string
	Fallback bool
	Attempts []Attempt
}

// Chat asks req.Model, then its fallbacks when the adapter has a Failover for that model.
func (a *Adapter) Chat(ctx context.Context, req ChatRequest) (ChatResult, error) {
	if f := a.Failover; f != nil && len(f.Fallbacks) > 0 && req.Model == f.Model {
		return a.chatFailover(ctx, req)
	}
	return a.chat(ctx, req)
}

func (a *Adapter) chat(ctx context.Context, req ChatRequest) (ChatResult, error) {
	if !a.Configured() {
		return ChatResult{}, providers.ErrNoCredential
	}
	if req.Model == "" || len(req.Messages) == 0 {
		return ChatResult{}, fmt.Errorf("openai: model and messages required")
	}
	if err := a.gate(); err != nil {
		return ChatResult{}, err
	}
	if len(req.Tools) > 0 && a.ToolsUnsupported(req.Model) {
		return ChatResult{}, fmt.Errorf("%w: %s", ErrToolsUnsupported, req.Model)
	}
	if hasImages(req) && a.VisionUnsupported(req.Model) {
		return ChatResult{}, fmt.Errorf("%w: %s", ErrVisionUnsupported, req.Model)
	}
	out, err := a.chatOnce(ctx, req, true)
	if err != nil || !thoughtAway(out) || req.MaxTokens == 1 { // a 1-token probe (ProbeChat) wants no answer
		return out, err
	}
	// on Anthropic every level up to "low" already went out with thinking off (shape.go)
	if req.Effort != "none" && !(a.API == APIAnthropic && thinkingOff(req.Effort)) {
		// A reasoning model can spend the whole output budget thinking and return no text (DeepSeek V4.1
		// Flash thinks 120+ tokens at "low"). Ask once more without thinking; both calls are charged.
		again := req
		again.Effort = "none"
		next, err := a.chatOnce(ctx, again, false)
		if err != nil {
			out.Elapsed += next.Elapsed
			return out, err // the retry's failure (a cancel, a refused key) is the call's; out carries the first call's cost
		}
		if out = addUsage(next, out); !thoughtAway(out) {
			return out, nil
		}
		req = again
	}
	if req.MaxTokens <= 0 || a.shapeOf(req).Headroom {
		return out, nil
	}
	// It thought through the budget even when asked not to: Claude Opus 5.5 thinks about 35 tokens
	// before a 13-token classifier answer at any level, and Anthropic's own API ignores the effort
	// field. Ask once more with room past the thinking, and keep the room for the model's later calls.
	// The room is kept only when it helped, so a model that answers empty for another reason keeps the
	// output limit its callers asked for.
	sh := a.shapeOf(req)
	sh.Headroom = true
	a.shapes.Store(shapeKey(req), sh)
	next, err := a.chatOnce(ctx, req, false)
	if err != nil || thoughtAway(next) {
		sh = a.shapeOf(req)
		sh.Headroom = false
		a.shapes.Store(shapeKey(req), sh)
	}
	if err != nil {
		out.Elapsed += next.Elapsed
		return out, err
	}
	return addUsage(next, out), nil
}

// addUsage is next with out's tokens, time and cost added: both calls are charged.
func addUsage(next, out ChatResult) ChatResult {
	next.PromptTokens += out.PromptTokens
	next.CompletionTokens += out.CompletionTokens
	next.CachedTokens += out.CachedTokens
	next.Elapsed += out.Elapsed
	next.CostEstimated = next.CostEstimated || out.CostEstimated
	if out.CostUSD != nil {
		sum := *out.CostUSD
		if next.CostUSD != nil {
			sum += *next.CostUSD
		}
		next.CostUSD = &sum
	}
	return next
}

func (a *Adapter) chatOnce(ctx context.Context, req ChatRequest, learn bool) (ChatResult, error) {
	requestedAt := time.Now()
	resp, err := a.postShaped(ctx, req, learn)
	if err != nil {
		return ChatResult{}, err
	}
	out, err := ParseChat(resp.ContentType, resp.Body)
	out.Elapsed = resp.Elapsed
	if err == nil {
		a.estimateCachedAt(&out.CostUSD, &out.CostEstimated, req.Model, out.PromptTokens, out.CachedTokens, out.CompletionTokens, requestedAt)
	}
	return out, err
}

// thoughtAway is an answer with no text and no tool calls cut at the output limit: the budget went to
// reasoning, whatever effort was asked (a model writing text would have written some).
func thoughtAway(out ChatResult) bool {
	return out.FinishReason == "length" && strings.TrimSpace(out.Content) == "" && len(out.ToolCalls) == 0
}

// ParseChat validates choices[0].message.content and usage. Hosts that return an error object with
// HTTP 200 (some proxies do) are handled the same as a 4xx.
func ParseChat(contentType string, body []byte) (ChatResult, error) {
	obj, err := decode.Object(contentType, body)
	if err != nil {
		return ChatResult{}, err
	}
	if e, ok := obj["error"]; ok && string(e) != "null" {
		return ChatResult{}, apiError(e, 0)
	}
	var choices []struct {
		Message      json.RawMessage `json:"message"`
		FinishReason string          `json:"finish_reason"`
	}
	if err := json.Unmarshal(obj["choices"], &choices); err != nil || len(choices) == 0 {
		return ChatResult{}, fmt.Errorf("%w: choices missing", decode.ErrUnexpected)
	}
	out := ChatResult{FinishReason: choices[0].FinishReason, Model: decode.Str(obj["model"])}
	// content may be a string or a list of {type:"text",text:...} parts.
	var msg struct {
		Content   json.RawMessage `json:"content"`
		ToolCalls []ToolCall      `json:"tool_calls"`
	}
	if len(choices[0].Message) > 0 && string(choices[0].Message) != "null" {
		if err := json.Unmarshal(choices[0].Message, &msg); err != nil {
			return ChatResult{}, fmt.Errorf("%w: message: %v", decode.ErrUnexpected, err)
		}
		out.Message = choices[0].Message
	}
	for _, tc := range msg.ToolCalls {
		if tc.Type == "" {
			tc.Type = "function" // some hosts leave it out
		}
		out.ToolCalls = append(out.ToolCalls, tc)
	}
	c := msg.Content
	var s string
	if json.Unmarshal(c, &s) == nil {
		out.Content = s
	} else {
		var parts []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(c, &parts) == nil {
			var b strings.Builder
			for _, p := range parts {
				if p.Type == "text" || p.Type == "" {
					b.WriteString(p.Text)
				}
			}
			out.Content = b.String()
		}
	}
	if u, ok := obj["usage"]; ok {
		var usage map[string]json.RawMessage
		if json.Unmarshal(u, &usage) == nil {
			if n, ok := decode.Number(usage["prompt_tokens"]); ok {
				out.PromptTokens = int(n)
			}
			if n, ok := decode.Number(usage["completion_tokens"]); ok {
				out.CompletionTokens = int(n)
			}
			out.CachedTokens = cachedTokens(usage)
			out.CostUSD = usageCost(usage)
		}
	}
	return out, nil
}

// cachedTokens reads the prompt-cache hits: OpenAI, OpenRouter and Gemini's OpenAI endpoint report
// prompt_tokens_details.cached_tokens, DeepSeek prompt_cache_hit_tokens.
func cachedTokens(usage map[string]json.RawMessage) int {
	var details map[string]json.RawMessage
	if json.Unmarshal(usage["prompt_tokens_details"], &details) == nil {
		if n, ok := decode.Number(details["cached_tokens"]); ok && n > 0 {
			return int(n)
		}
	}
	if n, ok := decode.Number(usage["prompt_cache_hit_tokens"]); ok && n > 0 {
		return int(n)
	}
	return 0
}

// usageCost is what a call cost. OpenRouter's usage.cost is what it charged; a request it ran on
// your own provider key (is_byok) was also billed by that provider, cost_details.upstream_inference_cost,
// and the budget counts both.
func usageCost(usage map[string]json.RawMessage) *float64 {
	n, ok := decode.Number(usage["cost"])
	if !ok || n < 0 {
		return nil
	}
	var byok bool
	if json.Unmarshal(usage["is_byok"], &byok) == nil && byok {
		var details map[string]json.RawMessage
		if json.Unmarshal(usage["cost_details"], &details) == nil {
			if up, ok := decode.Number(details["upstream_inference_cost"]); ok && up > 0 {
				n += up
			}
		}
	}
	return &n
}

// ---------------------------------------------------------------------------
// Embeddings and rerank (OpenRouter-compatible endpoints)
// ---------------------------------------------------------------------------

// EmbedResult is one /embeddings call.
type EmbedResult struct {
	Vectors       [][]float32
	PromptTokens  int
	CostUSD       *float64
	CostEstimated bool
	Elapsed       time.Duration
}

// Embed calls /embeddings. Rows are returned in input order (the response `index` is honored).
func (a *Adapter) Embed(ctx context.Context, model string, inputs []string) (EmbedResult, error) {
	if !a.Configured() {
		return EmbedResult{}, providers.ErrNoCredential
	}
	if model == "" || len(inputs) == 0 {
		return EmbedResult{}, fmt.Errorf("openai: embed model and inputs required")
	}
	if err := a.gate(); err != nil {
		return EmbedResult{}, err
	}
	requestedAt := time.Now()
	body, _ := json.Marshal(map[string]any{"model": model, "input": inputs})
	resp, err := a.Client.PostJSON(ctx, a.BaseURL+"/embeddings", a.headers(), body)
	if err != nil {
		return EmbedResult{}, mapErr(err, resp)
	}
	out, err := ParseEmbeddings(resp.ContentType, resp.Body, len(inputs))
	out.Elapsed = resp.Elapsed
	if err == nil {
		tokens := out.PromptTokens
		if tokens == 0 {
			tokens = EstimateTokens(inputs...) // Gemini reports no usage for embeddings
		}
		a.estimateAt(&out.CostUSD, &out.CostEstimated, model, tokens, 0, requestedAt)
	}
	return out, err
}

// EstimateTokens is a deliberately high token count for texts a host did not count: four ASCII bytes
// or one other character (Hangul, kana, Han) per token. Costs priced from it err on the side of the
// budget.
func EstimateTokens(texts ...string) int {
	n := 0
	for _, t := range texts {
		ascii := 0
		for _, r := range t {
			if r < 0x80 {
				ascii++
			} else {
				n++
			}
		}
		n += (ascii + 3) / 4
	}
	return n
}

// ParseEmbeddings validates data[*].embedding and count.
func ParseEmbeddings(contentType string, body []byte, want int) (EmbedResult, error) {
	if want <= 0 {
		return EmbedResult{}, fmt.Errorf("%w: embeddings count must be positive", decode.ErrUnexpected)
	}
	obj, err := decode.Object(contentType, body)
	if err != nil {
		return EmbedResult{}, err
	}
	if e, ok := obj["error"]; ok && string(e) != "null" {
		return EmbedResult{}, apiError(e, 0)
	}
	var rows []struct {
		Index     *int      `json:"index"`
		Embedding []float32 `json:"embedding"`
	}
	if err := json.Unmarshal(obj["data"], &rows); err != nil || len(rows) != want {
		return EmbedResult{}, fmt.Errorf("%w: embeddings count %d want %d", decode.ErrUnexpected, len(rows), want)
	}
	out := EmbedResult{Vectors: make([][]float32, want)}
	dimensions := 0
	missingIndices := 0
	for _, r := range rows {
		if r.Index == nil {
			missingIndices++
		}
	}
	positional := missingIndices == len(rows)
	if missingIndices == 1 && rows[0].Index == nil {
		// Gemini's OpenAI-compatible endpoint omits only index 0; later rows still carry their exact
		// position. Accept that known shape as a positional batch, never as arbitrary row fallback.
		positional = true
		for i := 1; i < len(rows); i++ {
			if rows[i].Index == nil || *rows[i].Index != i {
				positional = false
				break
			}
		}
	}
	if missingIndices != 0 && !positional {
		return EmbedResult{}, fmt.Errorf("%w: incomplete embedding indices", decode.ErrUnexpected)
	}
	for i, r := range rows {
		idx := i
		if !positional {
			idx = *r.Index
		}
		if idx < 0 || idx >= want || out.Vectors[idx] != nil {
			return EmbedResult{}, fmt.Errorf("%w: duplicate or out-of-range embedding index %d", decode.ErrUnexpected, idx)
		}
		if len(r.Embedding) == 0 {
			return EmbedResult{}, fmt.Errorf("%w: empty embedding at index %d", decode.ErrUnexpected, idx)
		}
		if dimensions == 0 {
			dimensions = len(r.Embedding)
		} else if len(r.Embedding) != dimensions {
			return EmbedResult{}, fmt.Errorf("%w: embedding dimension %d at index %d, want %d", decode.ErrUnexpected, len(r.Embedding), idx, dimensions)
		}
		for _, value := range r.Embedding {
			if math.IsNaN(float64(value)) || math.IsInf(float64(value), 0) {
				return EmbedResult{}, fmt.Errorf("%w: non-finite embedding value at index %d", decode.ErrUnexpected, idx)
			}
		}
		out.Vectors[idx] = r.Embedding
	}
	for i, vector := range out.Vectors {
		if len(vector) != dimensions {
			return EmbedResult{}, fmt.Errorf("%w: missing embedding index %d", decode.ErrUnexpected, i)
		}
	}
	if u, ok := obj["usage"]; ok {
		var usage map[string]json.RawMessage
		if json.Unmarshal(u, &usage) == nil {
			out.CostUSD = usageCost(usage)
			out.PromptTokens = usageTokens(usage, "prompt_tokens")
		}
	}
	return out, nil
}

// usageTokens reads a token count, falling back to total_tokens.
func usageTokens(usage map[string]json.RawMessage, key string) int {
	if n, ok := decode.Number(usage[key]); ok && n > 0 {
		return int(n)
	}
	if n, ok := decode.Number(usage["total_tokens"]); ok && n > 0 {
		return int(n)
	}
	return 0
}

// RerankResult is one /rerank call; Indices/Scores in relevance order.
type RerankResult struct {
	Indices       []int
	Scores        []float64
	Model         string
	TotalTokens   int
	CostUSD       *float64
	CostEstimated bool
	Elapsed       time.Duration
}

// ErrUnsupported is returned for a call the host family has no endpoint for (rerank on OpenAI or Gemini).
var ErrUnsupported = errors.New("openai: not offered by this provider")

// CanRerank reports whether this provider can answer Rerank at all.
func (a *Adapter) CanRerank() bool { return RerankAt(a.API, a.BaseURL) }

// Rerank calls /rerank (OpenRouter exposes Voyage/Cohere rerankers there).
func (a *Adapter) Rerank(ctx context.Context, model, query string, docs []string, topN int) (RerankResult, error) {
	if !a.Configured() {
		return RerankResult{}, providers.ErrNoCredential
	}
	if !a.CanRerank() {
		return RerankResult{}, ErrUnsupported
	}
	if err := a.gate(); err != nil {
		return RerankResult{}, err
	}
	if model == "" || strings.TrimSpace(query) == "" || len(docs) == 0 || topN < 1 || topN > len(docs) {
		return RerankResult{}, fmt.Errorf("openai: bad rerank request")
	}
	requestedAt := time.Now()
	body, _ := json.Marshal(map[string]any{"model": model, "query": query, "documents": docs, "top_n": topN})
	resp, err := a.Client.PostJSON(ctx, a.BaseURL+"/rerank", a.headers(), body)
	if err != nil {
		return RerankResult{}, mapErr(err, resp)
	}
	obj, err := decode.Object(resp.ContentType, resp.Body)
	if err != nil {
		return RerankResult{}, err
	}
	if e, ok := obj["error"]; ok && string(e) != "null" {
		return RerankResult{}, apiError(e, 0)
	}
	var rows []struct {
		Index          int      `json:"index"`
		RelevanceScore *float64 `json:"relevance_score"`
		Score          *float64 `json:"score"`
	}
	if err := json.Unmarshal(obj["results"], &rows); err != nil || len(rows) == 0 {
		return RerankResult{}, fmt.Errorf("%w: rerank results missing", decode.ErrUnexpected)
	}
	out := RerankResult{Model: decode.Str(obj["model"]), Elapsed: resp.Elapsed}
	for _, r := range rows {
		if r.Index < 0 || r.Index >= len(docs) {
			return RerankResult{}, fmt.Errorf("%w: rerank index out of range", decode.ErrUnexpected)
		}
		sc := r.RelevanceScore
		if sc == nil {
			sc = r.Score
		}
		if sc == nil {
			return RerankResult{}, fmt.Errorf("%w: rerank score missing", decode.ErrUnexpected)
		}
		out.Indices = append(out.Indices, r.Index)
		out.Scores = append(out.Scores, *sc)
	}
	if u, ok := obj["usage"]; ok {
		var usage map[string]json.RawMessage
		if json.Unmarshal(u, &usage) == nil {
			out.CostUSD = usageCost(usage)
			out.TotalTokens = usageTokens(usage, "total_tokens")
		}
	}
	a.estimateAt(&out.CostUSD, &out.CostEstimated, model, out.TotalTokens, 0, requestedAt)
	return out, nil
}

type APIError struct {
	Status  int // HTTP status when known
	Type    string
	Code    string
	Message string
	// BYOK: OpenRouter ran the request on the operator's own provider key (error.metadata.is_byok),
	// so a refusal is about that key, not the OpenRouter one.
	BYOK bool
}

func (e *APIError) Error() string {
	return fmt.Sprintf("openai api error %s/%s: %s", e.Type, e.Code, e.Message)
}

// apiError reads an error object: OpenAI's {message,type,code} or Google's {code,message,status}.
func apiError(raw json.RawMessage, status int) error {
	var e struct {
		Message string          `json:"message"`
		Type    string          `json:"type"`
		Status  json.RawMessage `json:"status"` // Google: "INVALID_ARGUMENT"; NanoGPT: 401
		Code    json.RawMessage `json:"code"`
		Meta    struct {
			BYOK bool `json:"is_byok"`
		} `json:"metadata"`
	}
	if json.Unmarshal(raw, &e) != nil {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			return &APIError{Status: status, Message: s}
		}
		return &APIError{Status: status, Message: "unparseable error"}
	}
	if st := decode.Str(e.Status); e.Type == "" && st != "" && (st[0] < '0' || st[0] > '9') {
		e.Type = st
	}
	return &APIError{Status: status, Type: e.Type, Code: decode.Str(e.Code), Message: e.Message, BYOK: e.Meta.BYOK}
}

// errorField finds the error object in a failed response: {"error":{...}} or, from Gemini's
// OpenAI-compatible endpoint, the same wrapped in a one-element list.
func errorField(contentType string, body []byte) (json.RawMessage, bool) {
	if decode.Sniff(contentType, body) != decode.JSON {
		return nil, false
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body, &obj) != nil {
		var list []map[string]json.RawMessage
		if json.Unmarshal(body, &list) != nil || len(list) == 0 {
			return nil, false
		}
		obj = list[0]
	}
	e, ok := obj["error"]
	return e, ok && string(e) != "null"
}

// rejectedKey reports a refused credential. Most hosts answer 401/403; Gemini answers 400 with
// "API key not valid" / "Please pass a valid API key".
func rejectedKey(status int, e *APIError) bool {
	if status == 401 || status == 403 {
		return true
	}
	if status != 400 || e == nil {
		return false
	}
	m := strings.ToLower(e.Message + " " + e.Code)
	return strings.Contains(m, "api key") || strings.Contains(m, "api_key_invalid")
}

func mapErr(err error, resp *providers.Response) error {
	var pe *providers.Error
	if errors.As(err, &pe) && resp != nil && len(resp.Body) > 0 {
		if raw, ok := errorField(resp.ContentType, resp.Body); ok {
			ae, _ := apiError(raw, pe.Status).(*APIError)
			if ae != nil && ae.BYOK && rejectedKey(pe.Status, ae) {
				return &byokError{detail: ae.Error()}
			}
			if rejectedKey(pe.Status, ae) {
				return &authError{detail: ae.Error()}
			}
			return ae
		}
		if pe.Status == 401 || pe.Status == 403 {
			return ErrAuth
		}
	}
	return err
}
