package application

import (
	"context"
	"errors"
	"fmt"
	"time"

	"masamong/internal/chat"
	"masamong/internal/errclass"
	"masamong/internal/providers/openai"
)

// The chat tool loop's model port (chat.LoopLLM): native tool calling on the role's
// OpenAI-compatible model. The loop uses the planner role (the small fast model) unless chat.tool_model
// picks the response role (the precise mode).

// ToolsReady is false for a role whose model refused tools in the last toolsRefusedTTL (the adapter
// remembers it within a turn): the turn then lets the planner choose without asking the model first.
func (c chatLLM) ToolsReady(role string) bool {
	r, ok := c.routes[role]
	if !ok || r.a == nil || r.a.ToolsUnsupported(r.model) {
		return false
	}
	at, refused := c.s.toolsRefused.Load(refusedKey(c.s, r))
	return !refused || !sinceWithin(time.Now(), at.(time.Time), toolsRefusedTTL)
}

// StableTools reports a role whose model (or a fallback of it) speaks the Anthropic Messages format: its
// thinking blocks are bound to the tool list they were made with, so once such an answer is in the loop
// (LoopReply.BindsTools) the loop keeps that list unchanged (chat.StableToolsLLM).
func (c chatLLM) StableTools(role string) bool {
	r, ok := c.routes[role]
	return ok && r.a != nil && r.a.BindsThinking()
}

// toolsRefusedTTL is how long a model's refusal of tools sends turns to the planner before the model
// is asked again: a reseller can route one request to an upstream without tools (seen live on
// cometapi), and a refusal kept until restart turned the loop off for every later turn.
const toolsRefusedTTL = 30 * time.Minute

// toolsRefused remembers the models that refused a request with tools; toolsTaken the ones that
// answered one in this process, whose later refusal is one upstream's, not the model's. Adapters are
// made per turn, so their own memory of it would be gone by the next turn.
// It lives on the Service (Service.toolsRefused, "<provider>|<model>" → true): a process-wide map keyed
// by the Service's address would find a new Service at a reused address refused.
func refusedKey(_ *Service, r chatRoute) string {
	return r.provider + "|" + r.model
}

// ChatTools asks the role's model with the tools offered. Unlike Chat there is no second model to fall
// back to: the other chat model may not take tools, and a failed first call already falls back to the
// planner. The call is counted like every chat call (spend, the turn's guard, provider health).
func (c chatLLM) ChatTools(ctx context.Context, role string, msgs []chat.LoopMsg, specs []chat.ToolSpec, maxTokens int) (chat.LoopReply, error) {
	r, ok := c.routes[role]
	if !ok || r.a == nil {
		return chat.LoopReply{}, fmt.Errorf("no adapter for the %s role", role)
	}
	d := chatDeadline[role]
	if r.timeout > 0 {
		d = r.timeout
	}
	if d > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, d)
		defer cancel()
	}
	om := make([]openai.Message, len(msgs))
	for i, m := range msgs {
		om[i] = openai.Message{Role: m.Role, Content: m.Content, ToolCallID: m.CallID, Raw: m.Raw}
		for _, call := range m.Calls {
			om[i].ToolCalls = append(om[i].ToolCalls, openai.ToolCall{ID: call.ID, Type: "function",
				Function: openai.ToolCallFunction{Name: call.Name, Arguments: call.Arguments}})
		}
	}
	tools := make([]openai.Tool, len(specs))
	for i, s := range specs {
		tools[i] = openai.Tool{Type: "function", Function: openai.ToolFunction{Name: s.Name, Description: s.Description, Parameters: s.Parameters}}
	}
	t := 0.0
	req := openai.ChatRequest{Model: r.model, Messages: om, MaxTokens: maxTokens, Temperature: &t, Effort: r.callEffort(), Tools: tools, ToolChoice: "auto", EmptyOK: true}
	if r.a.API == openai.APIOpenRouter || r.a.API == openai.APIOpenAI {
		// several independent calls in one answer (weather and a quote) where the host is known to take
		// the field; elsewhere the model's default applies (the adapter drops the field if refused)
		on := true
		req.ParallelToolCalls = &on
	}
	began := time.Now()
	end := chat.GuardFrom(ctx).Begin()
	cost := 0.0
	defer func() { end(cost) }()
	res, err := r.a.Chat(ctx, req)
	if res.CostUSD != nil {
		cost = *res.CostUSD
	}
	if eng, e2 := c.s.MemoryEngine(); e2 == nil {
		_ = eng.Store.AddSpend("chat", cost, 1)
	}
	end(cost)
	// a loop round is a model call like the planner's: one ledger row, also when the model refused tools
	// or a fallback answered (the turn's cost counts it, LoopTrace.CostUSD)
	servedProvider, servedModel := servedBy(r.provider, r.model, res)
	c.s.noteLLMCall(ctx, llmCall{Role: ledgerRoleToolLoop, Model: servedModel, Provider: servedProvider, FinishReason: res.FinishReason, PromptTokens: res.PromptTokens,
		CompletionTokens: res.CompletionTokens, CachedTokens: res.CachedTokens, Latency: time.Since(began), CostUSD: cost, Class: errclass.Of(err), Fallback: res.Fallback})
	if err != nil && (errors.Is(err, openai.ErrToolsUnsupported) || errclass.Of(err) == errclass.Unsupported) {
		// a model without tools is not a provider in trouble: the planner takes over, nothing to report
		if _, took := c.s.toolsTaken.Load(refusedKey(c.s, r)); !took {
			c.s.toolsRefused.Store(refusedKey(c.s, r), time.Now())
			c.s.Log.WarnContext(ctx, "chat: model refused tools, the planner chooses them for a while", "role", role, "model", r.model, "err", err)
		} else {
			c.s.Log.WarnContext(ctx, "chat: model that takes tools refused them once, the planner chooses this turn", "role", role, "model", r.model, "err", err)
		}
		return chat.LoopReply{CostUSD: cost}, fmt.Errorf("%w: %v", chat.ErrToolsUnsupported, err)
	}
	c.s.noteProvider(r.provider, err)
	if err != nil {
		return chat.LoopReply{CostUSD: cost}, err
	}
	c.s.toolsTaken.Store(refusedKey(c.s, r), true)
	c.s.toolsRefused.Delete(refusedKey(c.s, r))
	out := chat.LoopReply{Text: res.Content, Raw: res.Message, CostUSD: cost, BindsTools: res.ThinkingBound}
	for _, tc := range res.ToolCalls {
		out.Calls = append(out.Calls, chat.LoopCall{ID: tc.ID, Name: tc.Function.Name, Arguments: tc.Function.Arguments})
	}
	return out, nil
}

// toolMode is config chat.tool_mode.
func (s *Service) toolMode() string { return s.Doc().Chat.Mode() }

// chatToolFault makes a chat tool fail in one channel, for tests of what the bot does when a tool
// has no answer (the live regression set's "FX fails → another source"). Never set in production.
// The faults live on the Service (Service.toolFaults, "<channel>|<tool>" → true), like toolsRefused.
func faultKey(_ *Service, channelID, tool string) string {
	return channelID + "|" + tool
}

func (s *Service) chatToolFault(channelID, tool string) bool {
	_, ok := s.toolFaults.Load(faultKey(s, channelID, tool))
	return ok
}

// toolLoopKey is the turn record key of the loop's trace: what it did (loop mode) or what it would
// have done (shadow mode).
func toolLoopKey(t *chat.LoopTrace) string {
	if t != nil && t.Mode == chat.ToolModeShadow {
		return "tool_shadow"
	}
	return "tool_loop"
}

// addToolLoop puts the loop's trace in a turn record body (with its arguments: the record already
// holds the message) when the turn ran one.
func addToolLoop(body map[string]any, res chat.Result) {
	if res.ToolLoop != nil {
		body[toolLoopKey(res.ToolLoop)] = res.ToolLoop
	}
}
