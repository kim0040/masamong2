package openai

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"masamong/internal/errclass"
	"masamong/internal/providers"
)

// shape is how one model's chat requests are written: which optional fields it refused. Hosts of one
// family still differ per model (Gemini 3.8 Flash thinks at "none" or "low" but not "minimal"; its
// Flash-Lite takes "minimal" but not "none"; OpenAI's older chat models take no reasoning field and
// its reasoning models no temperature), so instead of a table that is out of date with the next model
// a request refused for an optional field is sent again without it, and the model's shape is kept.
type shape struct {
	NoEffort      bool // the model takes no reasoning field (or refused the level asked)
	NoTemperature bool
	SwapTokens    bool // the other token limit field (max_tokens ↔ max_completion_tokens)
	NoParallel    bool // the model refused parallel_tool_calls (OpenAI's o-series does) but takes tools
	// EffortNone sends effort "none" whatever was asked: the model takes tools only without thinking
	// and thinks by default when the field is left out (OpenAI's gpt-6-luna on chat completions)
	EffortNone bool
	// NoneAsMinimal sends "minimal" when "none" is asked: the model cannot stop thinking and refuses
	// "none" (OpenRouter's Claude Sonnet 5.5: "Reasoning is mandatory for this endpoint")
	NoneAsMinimal bool
	// Headroom adds thinkingHeadroom to the output limit: the model thinks even at its lowest effort, and
	// a short limit (a 40-token classifier) went to thinking with no answer left (Claude Opus 5.5)
	Headroom bool
	// BetweenTools turns thinking off on Anthropic with {"type":"between_tools"}: the model refuses
	// "disabled" and names that type instead (Claude Sonnet 5.5)
	BetweenTools bool
	// NoThinkingOff: on Anthropic the model refused every way to turn thinking off (Claude Opus 5.5), so
	// "none" is sent as its lowest effort level instead (anthropic.go). NoEffort there means the model
	// or host takes no adaptive thinking (its default is used)
	NoThinkingOff bool
	// NoOutputConfig: the Anthropic-format host refused output_config (the effort level)
	NoOutputConfig bool
	// MaxHigh: the model refused a level above "high" (xhigh, max), so those are sent as "high"
	MaxHigh bool
}

// thinkingHeadroom is the output room added for a model that thinks before every answer. Only the
// tokens it writes are charged, so a larger limit costs nothing when it thinks little.
const thinkingHeadroom = 1024

// shapeKey keeps a tool request's shape apart from the model's plain one: OpenRouter's 404 for a field
// no endpoint takes comes only with require_parameters (tool requests), and a planner call without
// tools must keep its temperature.
func shapeKey(req ChatRequest) string {
	if len(req.Tools) > 0 {
		return req.Model + "\x00tools"
	}
	return req.Model
}

// shapeOf is the shape learned for req's kind; a tool request without one of its own starts from the
// model's plain shape.
func (a *Adapter) shapeOf(req ChatRequest) shape {
	if v, ok := a.shapes.Load(shapeKey(req)); ok {
		return v.(shape)
	}
	if v, ok := a.shapes.Load(req.Model); ok {
		return v.(shape)
	}
	return shape{}
}

// payload writes a chat-completions request for the host family and the model's known shape (an
// Anthropic-format host is written by anthropicPayload instead).
func (a *Adapter) payload(req ChatRequest, sh shape) map[string]any {
	p := map[string]any{"model": req.Model, "messages": req.Messages}
	if req.MaxTokens > 0 {
		field := "max_tokens"
		if a.API == APIOpenAI {
			field = "max_completion_tokens" // max_tokens is refused by OpenAI's reasoning models
		}
		if sh.SwapTokens {
			if field == "max_tokens" {
				field = "max_completion_tokens"
			} else {
				field = "max_tokens"
			}
		}
		p[field] = req.MaxTokens
		if sh.Headroom || req.thinkRoom {
			p[field] = req.MaxTokens + thinkingHeadroom
		}
	}
	// Anthropic refuses a temperature on its Claude 5 models ("deprecated for this model") and from API
	// version 2026-10-21 on every model, so none is sent there (a refusal would spend a shape retry)
	if req.Temperature != nil && !sh.NoTemperature && a.API != APIAnthropic {
		p["temperature"] = *req.Temperature
	}
	caps := a.knownCaps(req.Model)
	if effort := writtenEffort(a.API, req, sh, caps); effort != "" {
		if effort != req.Effort && !sh.EffortNone && !(req.Effort == "none" && sh.NoneAsMinimal) && !sh.MaxHigh {
			a.noteClamp(req.Model, req.Effort, effort)
		}
		if a.API == APIOpenRouter {
			p["reasoning"] = map[string]any{"effort": effort}
		} else {
			p["reasoning_effort"] = effort
		}
		// asked not to think, but the model has no level that does not (GPT-6 Sol, Gemini 3.x/4, an
		// OpenRouter model that must reason): its thinking gets room, or a short answer is all thinking
		if req.MaxTokens > 0 && !sh.Headroom && !req.thinkRoom && thinkingOff(req.Effort) && !thinkingOff(effort) {
			for _, f := range []string{"max_tokens", "max_completion_tokens"} {
				if n, ok := p[f].(int); ok {
					p[f] = n + thinkingHeadroom
				}
			}
		}
	}
	a.toolFields(p, req, sh)
	a.byokRoute(p, req.Model)
	return p
}

// writtenEffort is the reasoning level a chat-completions request carries ("" leaves the field out):
// the level asked, unless the model refused the field (NoEffort) or its list says it does not reason;
// clamped to the levels its list names, and to the highest level the host family takes (topEffort).
func writtenEffort(api API, req ChatRequest, sh shape, caps Caps) string {
	if sh.NoEffort || caps.NoReasoning() {
		return ""
	}
	if sh.EffortNone {
		return "none"
	}
	effort := req.Effort
	switch {
	case effort == "":
		return ""
	case effort == "none" && sh.NoneAsMinimal:
		return "minimal"
	}
	if caps.Efforts != nil {
		l, ok := clampEffort(effort, caps.Efforts)
		if !ok {
			return ""
		}
		effort = l
	}
	// the family's top level only when the list said nothing: OpenRouter lists max for some models
	if top := api.topEffort(); top != "" && caps.Efforts == nil && effortRank(effort) > effortRank(top) {
		effort = top
	}
	if sh.MaxHigh && effortRank(effort) > effortRank("high") {
		effort = "high"
	}
	return effort
}

func effortRank(effort string) int { return slices.Index(EffortLevels, effort) }

// topEffort is the highest reasoning level a host family takes; "" lets every level through (a
// compatible host, whose models differ: a refused level is learned, MaxHigh). OpenAI and OpenRouter take
// up to xhigh, Gemini's OpenAI endpoint up to high; Anthropic's levels come from each model's list entry.
func (a API) topEffort() string {
	switch a {
	case APIOpenAI, APIOpenRouter:
		return "xhigh"
	case APIGemini:
		return "high"
	}
	return ""
}

// byokRoute pins an OpenRouter request for a ByokOnly model family to its one upstream provider, with
// no fallback to others, keeping the routing fields toolFields set.
func (a *Adapter) byokRoute(p map[string]any, model string) {
	if a.API != APIOpenRouter || len(a.ByokOnly) == 0 {
		return
	}
	family, _, ok := strings.Cut(model, "/")
	upstream := a.ByokOnly[family]
	if !ok || upstream == "" {
		return
	}
	route, _ := p["provider"].(map[string]any)
	if route == nil {
		route = map[string]any{}
	}
	route["only"] = []string{upstream}
	route["allow_fallbacks"] = false
	p["provider"] = route
}

// postShaped posts a chat request, retrying at most twice when the model refuses an optional field, and
// keeps the shape that worked for the model's later requests. What a request asking effort "none" (a
// quick classifier, or the no-thinking retry in Chat, which passes learn false) learns about the effort
// field holds for "none" only: a model that refuses "none" may take "low", so that part is not kept,
// except on Anthropic, where each way of turning thinking on or off has a flag of its own. Every other
// field it learns is kept, so a refused temperature costs one extra call per model, not one per
// classifier call. An Anthropic-format host is sent the Messages format (anthropic.go).
func (a *Adapter) postShaped(ctx context.Context, req ChatRequest, learn bool) (*providers.Response, error) {
	learnEffort := learn && req.Effort != "none" || a.API == APIAnthropic
	sh := a.shapeOf(req)
	caps := a.CapsOf(ctx, req.Model)
	for attempt := 0; ; attempt++ {
		addr, headers := a.BaseURL+"/chat/completions", a.headers()
		var body []byte
		if a.API == APIAnthropic {
			p, err := a.anthropicPayload(req, sh, caps)
			if err != nil {
				return nil, err
			}
			body, _ = json.Marshal(p)
			addr, headers = a.anthropicBase()+"/messages", a.anthropicHeaders()
		} else {
			var err error
			if body, err = json.Marshal(a.payload(req, sh)); err != nil {
				return nil, err
			}
		}
		resp, err := a.Client.PostJSON(final(ctx), addr, headers, body)
		if err == nil {
			old, keep := a.shapeOf(req), sh
			if !learnEffort {
				keep.NoEffort, keep.EffortNone = old.NoEffort, old.EffortNone
				if req.Effort == "none" && sh.NoEffort && !old.NoEffort {
					a.noneOff.Store(shapeKey(req), true) // for batch items only (batchShapeOf)
				}
			}
			if keep != old {
				a.shapes.Store(shapeKey(req), keep)
			}
			if len(req.Tools) == 0 {
				a.shapeSeen.Store(req.Model, true)
			}
			return resp, nil
		}
		err = mapErr(err, resp)
		if hasImages(req) && visionRefused(err) {
			// the model takes no pictures: dropping optional fields would not help (vision.go)
			a.visionOff.Store(req.Model, now())
			return resp, &visionError{err: err}
		}
		next, ok := refusedField(a.API, err, req, sh, caps)
		if !ok || attempt >= 2 {
			if len(req.Tools) > 0 && errclass.Of(err) == errclass.Unsupported {
				a.toolsOff.Store(req.Model, true) // later tool requests for this model fail fast (tools.go)
			}
			return resp, err
		}
		sh = next
	}
}

// ShapeLearned reports whether model's plain chat shape is known: a live request for it succeeded (with
// the optional fields it refused, if any, left out from then on). A batch cannot retry a refused field the
// way a live call does, so a chat batch for a model not learned yet is preceded by one live request.
func (a *Adapter) ShapeLearned(model string) bool {
	_, ok := a.shapeSeen.Load(model)
	return ok
}

// LearnRefusal applies what a batch item's refusal says about req's optional fields (the live retry's
// rule, refusedField) to the model's shape, so the next batch leaves the field out. As in postShaped, a
// refused effort learned from an effort "none" request holds for "none" only (batchShapeOf), so a live
// request at another level keeps its effort. It reports whether anything was learned.
func (a *Adapter) LearnRefusal(req ChatRequest, err error) bool {
	sh := a.batchShapeOf(req)
	caps := a.knownCaps(req.Model)
	next, ok := refusedField(a.API, err, req, sh, caps)
	if !ok || next == sh {
		return false
	}
	if req.Effort == "none" && a.API != APIAnthropic && next.NoEffort && !sh.NoEffort {
		a.noneOff.Store(shapeKey(req), true)
		next.NoEffort = a.shapeOf(req).NoEffort
	}
	if cur := a.shapeOf(req); next != cur {
		a.shapes.Store(shapeKey(req), next)
	}
	return true
}

// batchShapeOf is the shape a batch item of req is written with: the learned shape, and for an effort
// "none" request without the effort field when the model took "none" only that way. A batch item
// cannot be retried without a refused field the way a live call is.
func (a *Adapter) batchShapeOf(req ChatRequest) shape {
	sh := a.shapeOf(req)
	if req.Effort == "none" && !sh.NoEffort {
		if _, off := a.noneOff.Load(shapeKey(req)); off {
			sh.NoEffort = true
		}
	}
	return sh
}

// refusedField decides whether err is a model refusing one optional field of req, sent to a host of
// family api, and returns the shape without it. Only a 400/422 about the request qualifies; a refused
// key never does.
func refusedField(api API, err error, req ChatRequest, sh shape, caps Caps) (shape, bool) {
	var ae *APIError
	if !errors.As(err, &ae) || errors.Is(err, ErrAuth) {
		return sh, false
	}
	m := strings.ToLower(ae.Message + " " + ae.Code)
	if api == APIAnthropic {
		if ae.Status != 400 && ae.Status != 422 {
			return sh, false
		}
		return anthropicRefusal(m, req, sh, caps)
	}
	written := writtenEffort(api, req, sh, caps)
	sentEffort := written != ""
	sentTemp := req.Temperature != nil && !sh.NoTemperature
	if ae.Status == 404 && len(req.Tools) > 0 && strings.Contains(m, "requested parameters") {
		// OpenRouter with require_parameters (tools.go) found no endpoint taking every field sent and
		// names none (seen live for openai/gpt-6-luna and anthropic/claude-haiku-5.5, which list
		// neither parallel_tool_calls nor temperature): drop those two together, then effort, so the
		// two retries postShaped allows cover every optional field
		sentParallel := req.ParallelToolCalls != nil && !sh.NoParallel
		switch {
		case sentParallel || sentTemp:
			sh.NoParallel, sh.NoTemperature = sh.NoParallel || sentParallel, sh.NoTemperature || sentTemp
		case sentEffort:
			sh.NoEffort = true
		default:
			return sh, false
		}
		return sh, true
	}
	if ae.Status != 400 && ae.Status != 422 {
		return sh, false
	}
	if sentEffort && !sh.MaxHigh && effortRank(written) > effortRank("high") && (strings.Contains(m, "reasoning") || strings.Contains(m, "effort")) {
		// a level above high (xhigh, max) the model does not have: it may take high
		sh.MaxHigh = true
		return sh, true
	}
	if len(req.Tools) > 0 {
		switch {
		case req.ParallelToolCalls != nil && !sh.NoParallel && strings.Contains(m, "parallel_tool_calls"):
			sh.NoParallel = true
			return sh, true
		case sentEffort && !sh.EffortNone && req.Effort != "none" && strings.Contains(m, "reasoning") && strings.Contains(m, "'none'"):
			// "Function tools with reasoning_effort are not supported for gpt-6-luna in
			// /v1/chat/completions ... set reasoning_effort to 'none'" (OpenAI's models behind
			// OpenGateway, 2026-10-15). Leaving the field out does not help (the default thinks);
			// read as "no tools" the model would lose them for good
			sh.EffortNone = true
			return sh, true
		case sentEffort && (strings.Contains(m, "reasoning") || strings.Contains(m, "thinking")):
			sh.NoEffort = true
			return sh, true
		case ae.ErrClass() == errclass.Unsupported:
			// the model takes no tools: dropping effort or temperature would not help, and postShaped
			// remembers the refusal
			return sh, false
		}
	}
	switch {
	case sentEffort && req.Effort == "none" && !sh.EffortNone && !sh.NoneAsMinimal && strings.Contains(m, "reasoning") &&
		(strings.Contains(m, "mandatory") || strings.Contains(m, "cannot be disabled")):
		// leaving the field out would think at the model's default; its lowest level thinks least
		sh.NoneAsMinimal = true
	case sentEffort && (strings.Contains(m, "reasoning") || strings.Contains(m, "thinking")):
		sh.NoEffort = true
	case sentTemp && strings.Contains(m, "temperature"):
		sh.NoTemperature = true
	case req.MaxTokens > 0 && !sh.SwapTokens && (strings.Contains(m, "max_tokens") || strings.Contains(m, "max_completion_tokens")):
		sh.SwapTokens = true
	case strings.Contains(m, "invalid argument") || strings.Contains(m, "unsupported") || strings.Contains(m, "unrecognized") || strings.Contains(m, "unknown name"):
		// a refusal that does not name the field: drop the optional fields one at a time
		switch {
		case sentEffort:
			sh.NoEffort = true
		case sentTemp:
			sh.NoTemperature = true
		default:
			return sh, false
		}
	default:
		return sh, false
	}
	return sh, true
}
