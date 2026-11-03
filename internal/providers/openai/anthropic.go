package openai

import (
	"encoding/json"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"masamong/internal/errclass"
	"masamong/internal/providers/decode"
)

// The Anthropic Messages API (POST {base}/v1/messages), spoken to every host of the anthropic family:
// Anthropic itself and the third-party endpoints that copy its format. Anthropic's OpenAI-compatible
// endpoint cannot turn on adaptive thinking ("not available via the OpenAI compatibility endpoint") and
// ignores reasoning_effort, so Claude 5 models could not think there at all; the native format can.
//
// A chat request is translated here and nowhere else: system messages become the top-level system,
// the rest alternate user/assistant (consecutive turns of one role merged, as the API expects), pictures
// become image blocks, tool calls tool_use blocks and tool answers tool_result blocks. The answer comes
// back as a ChatResult like any host's; its Message keeps the native content blocks (thinking blocks
// with their signatures included), which a tool-loop continuation must send back unchanged.

// anthropicBase is the API root of an Anthropic-format host: the configured address without a trailing
// "/messages", with "/v1" added when it does not end in it. Anthropic's own address is configured with
// /v1 (https://api.anthropic.com/v1); third-party hosts document the root the SDKs append /v1/messages
// to (https://host/anthropic), and an operator may paste the full endpoint (https://host/v1/messages).
func (a *Adapter) anthropicBase() string {
	b := strings.TrimSuffix(strings.TrimRight(a.BaseURL, "/"), "/messages")
	if !strings.HasSuffix(b, "/v1") {
		b += "/v1"
	}
	return b
}

// officialAnthropic reports Anthropic's own API host.
func officialAnthropic(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	return err == nil && strings.EqualFold(u.Hostname(), "api.anthropic.com")
}

// anthropicHeaders authenticate every request of the format (messages, model list and lookup, batches):
// x-api-key and anthropic-version on every host. A third-party host also gets the key as a bearer token:
// those hosts document either header (the SDKs send x-api-key for an API key and Authorization for an
// auth token) and some read only the bearer one. Anthropic's own host gets x-api-key alone, as its docs
// show.
func (a *Adapter) anthropicHeaders() map[string]string {
	h := map[string]string{"x-api-key": a.Cred.Value(), "anthropic-version": anthropicVersion}
	if !officialAnthropic(a.BaseURL) {
		h["Authorization"] = "Bearer " + a.Cred.Value()
	}
	for k, v := range a.Headers {
		h[k] = v
	}
	return h
}

// anthropicRequest is one Messages request (also a batch item's params).
type anthropicRequest struct {
	Model        string          `json:"model"`
	MaxTokens    int             `json:"max_tokens"`
	System       string          `json:"system,omitempty"`
	Messages     []anthropicTurn `json:"messages"`
	Tools        []anthropicTool `json:"tools,omitempty"`
	ToolChoice   map[string]any  `json:"tool_choice,omitempty"`
	Thinking     map[string]any  `json:"thinking,omitempty"`
	OutputConfig map[string]any  `json:"output_config,omitempty"`
}

type anthropicTurn struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicDefaultMaxTokens is the output limit of a request that names none (the API requires one).
const anthropicDefaultMaxTokens = 4096

// thinkingRoom is the output room added for thinking at an effort level: thinking is written as output
// tokens, and a short limit (an 80-token planner call) would otherwise go to thinking with no answer
// left. Only the tokens written are charged, so the room costs nothing a call does not use. On a model
// that thinks with a fixed budget (thinking type "enabled") the room is that budget.
func thinkingRoom(effort string) int {
	switch effort {
	case "medium":
		return 2048
	case "high":
		return 4096
	case "xhigh":
		return 8192
	case "max":
		return 16384
	}
	return thinkingHeadroom // none, minimal, low: the least room (1024, also the smallest budget the API takes)
}

// anthropicEffort is how one effort level is written for an Anthropic model: the thinking field (nil:
// left out), the output_config effort ("" left out), and whether the model may think on the call (it
// then gets thinkingRoom). It follows the model's capabilities when known, else the shape learned from
// refusals:
//
//	level            capabilities known                              unknown (learned shape)
//	none, minimal    thinking disabled; else between_tools; else     disabled (between_tools once named by a
//	                 no thinking field and the lowest effort level   refusal); else the effort field at low
//	low … max        thinking adaptive (else enabled with a budget)  adaptive + the effort level
//	                 + the effort clamped to the levels it takes
//
// A model the list says does not think gets neither field. "" (a key probe) leaves both out.
func anthropicEffort(effort string, caps Caps, sh shape, tools bool) (thinking map[string]any, level string, thinks bool) {
	if effort == "" {
		return nil, "", caps.ThinkingOff() == "never"
	}
	if caps.NoReasoning() {
		return nil, "", false
	}
	known := caps.Thinking != nil
	if thinkingOff(effort) {
		off := ""
		switch {
		case known:
			off = caps.ThinkingOff()
		case sh.NoThinkingOff:
			off = "never"
		case sh.BetweenTools:
			off = "between_tools"
		default:
			off = "disabled"
		}
		if off != "never" {
			return map[string]any{"type": off}, "", off == "between_tools" && tools
		}
		// the model cannot stop thinking: its lowest level thinks least
		switch {
		case caps.Efforts != nil:
			level, _ = clampEffort("low", caps.Efforts)
		case !sh.NoOutputConfig:
			level = "low"
		}
		return nil, level, true
	}
	switch {
	case caps.Efforts != nil:
		level, _ = clampEffort(effort, caps.Efforts)
	case !sh.NoOutputConfig:
		level = effort
		if sh.MaxHigh && slices.Index(EffortLevels, effort) > slices.Index(EffortLevels, "high") {
			level = "high"
		}
	}
	switch {
	case known && slices.Contains(caps.Thinking, "adaptive"):
		thinking = map[string]any{"type": "adaptive"}
	case known && slices.Contains(caps.Thinking, "enabled"):
		thinking = map[string]any{"type": "enabled", "budget_tokens": thinkingRoom(effort)}
	case !known && !sh.NoEffort:
		thinking = map[string]any{"type": "adaptive"}
	}
	return thinking, level, true
}

// thinkingOff reports the levels that ask for no thinking.
func thinkingOff(effort string) bool { return effort == "none" || effort == "minimal" }

// anthropicPayload writes req in the Messages format for a model with caps and learned shape sh.
func (a *Adapter) anthropicPayload(req ChatRequest, sh shape, caps Caps) (anthropicRequest, error) {
	p := anthropicRequest{Model: req.Model, MaxTokens: req.MaxTokens}
	if p.MaxTokens <= 0 {
		p.MaxTokens = anthropicDefaultMaxTokens
	}
	system, turns, err := anthropicMessages(req.Messages)
	if err != nil {
		return p, err
	}
	p.System, p.Messages = system, turns
	thinking, level, thinks := anthropicEffort(req.Effort, caps, sh, len(req.Tools) > 0)
	p.Thinking = thinking
	if level != "" {
		p.OutputConfig = map[string]any{"effort": level}
		if !thinkingOff(req.Effort) && level != req.Effort {
			a.noteClamp(req.Model, req.Effort, level)
		}
	}
	switch {
	case thinks && req.MaxTokens != 1: // a 1-token key probe wants no answer
		room := thinkingRoom(req.Effort)
		if b, ok := thinking["budget_tokens"].(int); ok {
			room = b
		}
		p.MaxTokens += room
	case sh.Headroom || req.thinkRoom:
		p.MaxTokens += thinkingHeadroom
	}
	if len(req.Tools) > 0 {
		for _, t := range req.Tools {
			schema := t.Function.Parameters
			if len(schema) == 0 || string(schema) == "null" {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			p.Tools = append(p.Tools, anthropicTool{Name: t.Function.Name, Description: t.Function.Description, InputSchema: schema})
		}
		// auto or none only: several Claude 5 models refuse a forced choice, and none of them takes one
		// with thinking on, so "required" and a named function are sent as auto
		choice := map[string]any{"type": "auto"}
		if s, ok := req.ToolChoice.(string); ok && s == "none" {
			choice["type"] = "none"
		}
		if req.ParallelToolCalls != nil && !*req.ParallelToolCalls && choice["type"] == "auto" && !sh.NoParallel {
			choice["disable_parallel_tool_use"] = true
		}
		if req.ToolChoice != nil || choice["disable_parallel_tool_use"] != nil {
			p.ToolChoice = choice
		}
	}
	return p, nil
}

// noteClamp logs, once per model and level, that a level the model does not take was sent as another.
func (a *Adapter) noteClamp(model, asked, sent string) {
	if !a.caps().note(model + "\x00" + asked + "\x00" + sent) {
		return
	}
	if a.Client != nil && a.Client.Log != nil {
		a.Client.Log.Info("model effort clamped to a level it takes", "model", model, "asked", asked, "sent", sent)
	}
}

// anthropicMessages splits a chat into the top-level system text and alternating turns.
func anthropicMessages(msgs []Message) (string, []anthropicTurn, error) {
	var system []string
	var turns []anthropicTurn
	add := func(role string, blocks []json.RawMessage) {
		if len(blocks) == 0 {
			return
		}
		if n := len(turns); n > 0 && turns[n-1].Role == role {
			turns[n-1].Content = append(turns[n-1].Content, blocks...)
			return
		}
		turns = append(turns, anthropicTurn{Role: role, Content: blocks})
	}
	for i, m := range msgs {
		if len(m.Raw) > 0 {
			raw, err := rawMessage(m.Raw)
			if err != nil {
				return "", nil, fmt.Errorf("openai: message %d: %w", i, err)
			}
			if raw.native != nil {
				add(anthropicRole(raw.msg.Role), raw.native)
				continue
			}
			m = raw.msg
		}
		switch m.Role {
		case "system", "developer":
			if strings.TrimSpace(m.Content) != "" {
				system = append(system, m.Content)
			}
		case "assistant":
			var blocks []json.RawMessage
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, textBlock(m.Content))
			}
			for _, c := range m.ToolCalls {
				blocks = append(blocks, toolUseBlock(c))
			}
			add("assistant", blocks)
		case "tool":
			block := map[string]any{"type": "tool_result", "tool_use_id": m.ToolCallID}
			if m.Content != "" {
				block["content"] = m.Content
			}
			b, _ := json.Marshal(block)
			add("user", []json.RawMessage{b})
		default: // user
			var blocks []json.RawMessage
			for _, img := range m.Images {
				if img.MIME == "" || len(img.Data) == 0 {
					return "", nil, fmt.Errorf("openai: image part without data")
				}
				b, _ := json.Marshal(map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": img.MIME, "data": img.base64()}})
				blocks = append(blocks, b)
			}
			if strings.TrimSpace(m.Content) != "" {
				blocks = append(blocks, textBlock(m.Content))
			}
			add("user", blocks)
		}
	}
	// a tool_result must come first in its user turn
	for i := range turns {
		if turns[i].Role == "user" {
			slices.SortStableFunc(turns[i].Content, func(x, y json.RawMessage) int {
				return boolRank(blockType(y) == "tool_result") - boolRank(blockType(x) == "tool_result")
			})
		}
	}
	return strings.Join(system, "\n\n"), turns, nil
}

func boolRank(b bool) int {
	if b {
		return 1
	}
	return 0
}

func anthropicRole(role string) string {
	if role == "assistant" {
		return "assistant"
	}
	return "user"
}

func textBlock(s string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"type": "text", "text": s})
	return b
}

func toolUseBlock(c ToolCall) json.RawMessage {
	input := json.RawMessage(strings.TrimSpace(c.Function.Arguments))
	var obj map[string]json.RawMessage
	if len(input) == 0 || json.Unmarshal(input, &obj) != nil || obj == nil {
		input = json.RawMessage(`{}`) // the API takes only an object; a model's broken arguments become none
	}
	b, _ := json.Marshal(map[string]any{"type": "tool_use", "id": c.ID, "name": c.Function.Name, "input": input})
	return b
}

func blockType(b json.RawMessage) string {
	var t struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal(b, &t)
	return t.Type
}

// nativeBlockTypes are content block types only the Messages format has: content holding one of them
// is an answer of that format, sent back as it came.
var nativeBlockTypes = []string{"tool_use", "thinking", "redacted_thinking", "tool_result", "server_tool_use"}

// parsedRaw is a message sent back as the API returned it: native content blocks (an Anthropic answer)
// or a chat-completions message (an answer of another host, e.g. before a failover in a tool loop).
type parsedRaw struct {
	native []json.RawMessage
	msg    Message
}

func rawMessage(raw json.RawMessage) (parsedRaw, error) {
	var w struct {
		Role       string          `json:"role"`
		Content    json.RawMessage `json:"content"`
		ToolCalls  []ToolCall      `json:"tool_calls"`
		ToolCallID string          `json:"tool_call_id"`
		Name       string          `json:"name"`
	}
	if err := json.Unmarshal(raw, &w); err != nil {
		return parsedRaw{}, fmt.Errorf("raw message is not valid JSON: %w", err)
	}
	out := parsedRaw{msg: Message{Role: w.Role, ToolCalls: w.ToolCalls, ToolCallID: w.ToolCallID, Name: w.Name}}
	var blocks []json.RawMessage
	if json.Unmarshal(w.Content, &blocks) == nil {
		native := false
		var text strings.Builder
		for _, b := range blocks {
			t := blockType(b)
			native = native || slices.Contains(nativeBlockTypes, t)
			if t == "text" {
				var p struct {
					Text string `json:"text"`
				}
				_ = json.Unmarshal(b, &p)
				text.WriteString(p.Text)
			}
		}
		if native {
			out.native = blocks
			return out, nil
		}
		out.msg.Content = text.String()
		return out, nil
	}
	_ = json.Unmarshal(w.Content, &out.msg.Content)
	return out, nil
}

// chatFromNative is an Anthropic answer's message in the chat-completions shape, for a host of another
// family (a fallback after a failover in a tool loop): its text and tool calls; thinking is Anthropic's.
func chatFromNative(blocks []json.RawMessage, role string) ([]byte, error) {
	m := Message{Role: role}
	var text strings.Builder
	for _, b := range blocks {
		var p struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if json.Unmarshal(b, &p) != nil {
			continue
		}
		switch p.Type {
		case "text":
			text.WriteString(p.Text)
		case "tool_use":
			args := string(p.Input)
			if args == "" || args == "null" {
				args = "{}"
			}
			m.ToolCalls = append(m.ToolCalls, ToolCall{ID: p.ID, Type: "function", Function: ToolCallFunction{Name: p.Name, Arguments: args}})
		}
	}
	m.Content = text.String()
	return json.Marshal(m)
}

// ErrRefused is an answer the model declined to give (stop_reason "refusal"). It is class Empty, so the
// role's fallback model is asked, as for an empty answer.
var ErrRefused error = classedError{msg: "openai: the model declined to answer (stop_reason=refusal)", class: errclass.Empty}

// anthropicFinish maps a stop_reason to the chat-completions finish reasons the callers read.
func anthropicFinish(stop string) string {
	switch stop {
	case "max_tokens", "model_context_window_exceeded":
		return "length"
	case "end_turn", "stop_sequence", "pause_turn":
		return "stop"
	case "tool_use":
		return "tool_calls"
	case "refusal":
		return "content_filter"
	}
	return stop
}

// ParseAnthropicChat validates one Messages answer. An error envelope (also with HTTP 200, as some
// proxies send) is the same error as a 4xx.
func ParseAnthropicChat(contentType string, body []byte) (ChatResult, error) {
	obj, err := decode.Object(contentType, body)
	if err != nil {
		return ChatResult{}, err
	}
	if e, ok := obj["error"]; ok && string(e) != "null" {
		ae, _ := apiError(e, 0).(*APIError)
		if ae != nil && ae.Status == 0 {
			ae.Status = anthropicStatus(ae.Type)
		}
		return ChatResult{}, ae
	}
	var content []json.RawMessage
	if err := json.Unmarshal(obj["content"], &content); err != nil || obj["content"] == nil {
		return ChatResult{}, fmt.Errorf("%w: content missing", decode.ErrUnexpected)
	}
	stop := decode.Str(obj["stop_reason"])
	out := ChatResult{FinishReason: anthropicFinish(stop), Model: decode.Str(obj["model"])}
	var text strings.Builder
	for _, b := range content {
		var p struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		}
		if err := json.Unmarshal(b, &p); err != nil {
			return ChatResult{}, fmt.Errorf("%w: content block: %v", decode.ErrUnexpected, err)
		}
		switch p.Type {
		case "text":
			text.WriteString(p.Text)
		case "tool_use":
			args := strings.TrimSpace(string(p.Input))
			if args == "" || args == "null" {
				args = "{}"
			}
			out.ToolCalls = append(out.ToolCalls, ToolCall{ID: p.ID, Type: "function", Function: ToolCallFunction{Name: p.Name, Arguments: args}})
		}
	}
	out.Content = text.String()
	out.Message, _ = json.Marshal(map[string]any{"role": "assistant", "content": content})
	var u struct {
		Input         int `json:"input_tokens"`
		Output        int `json:"output_tokens"`
		CacheRead     int `json:"cache_read_input_tokens"`
		CacheCreation int `json:"cache_creation_input_tokens"`
		Details       struct {
			Thinking int `json:"thinking_tokens"`
		} `json:"output_tokens_details"`
	}
	if raw, ok := obj["usage"]; ok && json.Unmarshal(raw, &u) == nil {
		out.PromptTokens = u.Input + u.CacheRead + u.CacheCreation
		out.CompletionTokens = u.Output
		out.CachedTokens = u.CacheRead
		out.CacheWriteTokens = u.CacheCreation
		out.ReasoningTokens = u.Details.Thinking
	}
	if stop == "refusal" {
		return out, ErrRefused
	}
	return out, nil
}

// anthropicRefusal is refusedField for an Anthropic-format host: what a 400 says about the optional
// fields anthropicEffort wrote (the thinking type, the output_config effort) and the shape without it.
func anthropicRefusal(m string, req ChatRequest, sh shape, caps Caps) (shape, bool) {
	thinking, level, _ := anthropicEffort(req.Effort, caps, sh, len(req.Tools) > 0)
	typ, _ := thinking["type"].(string)
	sentOff := typ == "disabled" || typ == "between_tools"
	sentOn := thinking != nil && !sentOff
	high := slices.Index(EffortLevels, level) > slices.Index(EffortLevels, "high")
	switch {
	case typ == "disabled" && !sh.BetweenTools && strings.Contains(m, "between_tools"):
		// 'To turn thinking off on this model, send "thinking": {"type": "between_tools"} instead of
		// {"type": "disabled"}' (Claude Sonnet 5.5)
		sh.BetweenTools = true
	case sentOff && strings.Contains(m, "thinking"):
		// '"thinking.type.disabled" is not supported for this model. Use "thinking.type.adaptive" and
		// "output_config.effort" to control thinking behavior' (Claude Opus 5.5): it cannot stop thinking
		sh.NoThinkingOff = true
	case level != "" && high && !sh.MaxHigh && strings.Contains(m, "effort"):
		sh.MaxHigh = true // a level above high refused: high is the most it takes
	case level != "" && (strings.Contains(m, "output_config") || strings.Contains(m, "effort") && !strings.Contains(m, "thinking")):
		sh.NoOutputConfig = true
	case sentOn && (strings.Contains(m, "thinking") || strings.Contains(m, "adaptive")):
		sh.NoEffort = true // the model or host takes no adaptive thinking: its default
	case strings.Contains(m, "invalid argument") || strings.Contains(m, "unsupported") || strings.Contains(m, "unrecognized") ||
		strings.Contains(m, "unknown field") || strings.Contains(m, "extra inputs"):
		// a host that does not name the field: drop the optional ones one at a time
		switch {
		case level != "":
			sh.NoOutputConfig = true
		case sentOn:
			sh.NoEffort = true
		case sentOff:
			sh.NoThinkingOff = true
		default:
			return sh, false
		}
	default:
		return sh, false
	}
	return sh, true
}
