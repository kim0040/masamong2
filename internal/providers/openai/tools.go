package openai

import (
	"encoding/json"
	"fmt"
	"strings"

	"masamong/internal/errclass"
)

// Native tool calling (docs/plan-memory-conversation.md §5.5.4). The shapes follow the Chat Completions
// API that every host here copies; fields only some hosts add (DeepSeek reasoning_content, Gemini's
// extra_content.google.thought_signature inside a tool call) are not modelled but kept whole in
// ChatResult.Message, which Message.Raw sends back unchanged.

// Tool is one function the model may call.
type Tool struct {
	Type     string       `json:"type"` // "function"; empty is sent as "function"
	Function ToolFunction `json:"function"`
}

// ToolFunction describes a callable: Parameters is a JSON Schema object.
type ToolFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// ToolCall is one call the model asked for.
type ToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"` // "function"; empty is sent as "function"
	Function ToolCallFunction `json:"function"`
}

// ToolCallFunction is the called function. Arguments is the JSON text exactly as the API sent it: models
// emit invalid or partial JSON now and then, so the caller decides how to parse it.
type ToolCallFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// UnmarshalJSON accepts arguments as a string (every host) or as an object (some local servers answer
// that way); an object is kept as its JSON text.
func (f *ToolCallFunction) UnmarshalJSON(b []byte) error {
	var raw struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	f.Name = raw.Name
	f.Arguments = ""
	args := strings.TrimSpace(string(raw.Arguments))
	switch {
	case args == "" || args == "null":
	case args[0] == '"':
		if err := json.Unmarshal(raw.Arguments, &f.Arguments); err != nil {
			return err
		}
	default:
		f.Arguments = args
	}
	return nil
}

// Message is one chat message. A plain message is {role, content}. An assistant message that carries
// tool calls may have empty Content (sent as null). A "tool" message answers one call: ToolCallID plus
// string Content. When Raw is set the message is sent as that JSON object and every other field is
// ignored: it is how a message the API returned goes back unchanged, keeping fields only the provider
// understands.
type Message struct {
	Role       string
	Content    string
	ToolCalls  []ToolCall
	ToolCallID string
	Name       string
	Raw        json.RawMessage
	// Images are pictures sent with a user message (vision.go): the content becomes a list of parts,
	// the text first. A message without images is written exactly as before.
	Images []ImagePart
}

// wireMessage is the plain shape. For a message without tool fields it marshals to the same bytes the
// two-field struct did before tools existed.
type wireMessage struct {
	Role       string     `json:"role"`
	Content    *string    `json:"content"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	Name       string     `json:"name,omitempty"`
}

func (m Message) MarshalJSON() ([]byte, error) {
	if len(m.Raw) > 0 {
		if !json.Valid(m.Raw) {
			return nil, fmt.Errorf("openai: raw message is not valid JSON")
		}
		return m.Raw, nil
	}
	if len(m.Images) > 0 {
		return marshalWithImages(m)
	}
	w := wireMessage{Role: m.Role, ToolCallID: m.ToolCallID, Name: m.Name}
	if m.Content != "" || len(m.ToolCalls) == 0 {
		w.Content = &m.Content // an assistant message with only tool_calls gets null, which every host takes
	}
	if len(m.ToolCalls) > 0 {
		w.ToolCalls = make([]ToolCall, len(m.ToolCalls))
		for i, c := range m.ToolCalls {
			if c.Type == "" {
				c.Type = "function"
			}
			if c.Function.Arguments == "" {
				c.Function.Arguments = "{}" // a call without arguments still has to carry valid JSON text
			}
			w.ToolCalls[i] = c
		}
	}
	return json.Marshal(w)
}

func (m *Message) UnmarshalJSON(b []byte) error {
	var w struct {
		Role       string     `json:"role"`
		Content    string     `json:"content"`
		ToolCalls  []ToolCall `json:"tool_calls"`
		ToolCallID string     `json:"tool_call_id"`
		Name       string     `json:"name"`
	}
	if err := json.Unmarshal(b, &w); err != nil {
		return err
	}
	*m = Message{Role: w.Role, Content: w.Content, ToolCalls: w.ToolCalls, ToolCallID: w.ToolCallID, Name: w.Name}
	return nil
}

// ErrToolsUnsupported is returned, without a request, for a model this adapter has seen refuse tools.
var ErrToolsUnsupported error = classedError{msg: "openai: model takes no tools", class: errclass.Unsupported}

// ToolsUnsupported reports whether a request carrying Tools was refused for this model earlier in this
// process. The memory is per adapter and not persisted: a model that gains tools, or a route that was
// down, is tried again after the next start.
func (a *Adapter) ToolsUnsupported(model string) bool {
	_, ok := a.toolsOff.Load(model)
	return ok
}

// toolFields writes the tool fields of a request (none for a request without Tools).
func (a *Adapter) toolFields(p map[string]any, req ChatRequest, sh shape) {
	if len(req.Tools) == 0 {
		return
	}
	tools := make([]Tool, len(req.Tools))
	for i, t := range req.Tools {
		if t.Type == "" {
			t.Type = "function"
		}
		tools[i] = t
	}
	p["tools"] = tools
	if req.ToolChoice != nil {
		p["tool_choice"] = req.ToolChoice
	}
	if req.ParallelToolCalls != nil && !sh.NoParallel {
		p["parallel_tool_calls"] = *req.ParallelToolCalls
	}
	if a.API == APIOpenRouter {
		// route only to providers that take every parameter sent, so a tool request never lands on one
		// that silently ignores `tools`
		p["provider"] = map[string]any{"require_parameters": true}
	}
}
