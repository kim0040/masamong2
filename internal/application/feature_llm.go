package application

import (
	"cmp"
	"context"
	"fmt"
	"strings"
	"time"

	"masamong/internal/chat"
	"masamong/internal/errclass"
	"masamong/internal/providers/openai"
)

// featureText asks the model behind role for one reply to prompt (channel summaries, fortunes, notice
// digests). A missing role uses the response model. It respects the monthly model budget and records
// what it spent under kind, like chat turns do.
func (s *Service) featureText(ctx context.Context, role, kind, system, prompt string, maxTokens int, temperature float64) (string, float64, error) {
	if budget := parseUSD(s.Doc().Limits.MonthlyBudgetUSD, 0); budget > 0 {
		if _, spent, ok := s.monthSpend(); ok && spent >= budget {
			return "", 0, ErrChatBudget
		}
	}
	doc := s.Doc()
	src := role
	if _, ok := modelOf(doc, role); !ok {
		src = "response"
	}
	m, ok := modelOf(doc, src)
	if !ok {
		return "", 0, fmt.Errorf("models.%s not configured", src)
	}
	a, err := s.llmAdapter(src)
	if err != nil {
		return "", 0, err
	}
	var msgs []openai.Message
	if strings.TrimSpace(system) != "" {
		msgs = append(msgs, openai.Message{Role: "system", Content: system})
	}
	msgs = append(msgs, openai.Message{Role: "user", Content: prompt})
	t := temperature
	// the role's effort (config.Model.Effort), else low. The summary role does extraction and short
	// write-ups under a tight token cap: a thinking model spends that cap on reasoning and hands back cut
	// JSON (postmortem 0005), so it is a Quick call, asked not to think unless models.summary.effort is
	// set. A fortune is a short write-up under a 700–1800 token cap: thinking spent most of it and cut the
	// reading off mid-sentence (ops 2026-10-14: 3 of 5 ended at finish_reason=length), so it is one too.
	effort := cmp.Or(m.Effort, "low")
	quick := role == "summary" || kind == "fortune"
	if quick {
		effort = "none"
		// the job decides, not the model it falls back to: a summary on the response model thinks only
		// when the summary role's own effort is set, and so do that model's fallbacks unless one has an
		// effort of its own (models.<role>.fallbacks[].effort)
		job := quickEffort(role, doc.Models[role].Effort)
		a.RoleEffort = job
		if f := a.Failover; f != nil {
			for _, fb := range f.Fallbacks {
				if fb.Effort == "" && fb.A != nil {
					fb.A.RoleEffort = job
				}
			}
		}
	}
	began := time.Now()
	res, err := a.Chat(ctx, openai.ChatRequest{Model: m.Model, Messages: msgs, MaxTokens: maxTokens, Temperature: &t, Effort: effort, Quick: quick})
	cost := 0.0
	if res.CostUSD != nil {
		cost = *res.CostUSD
	}
	if eng, e2 := s.MemoryEngine(); e2 == nil { // a failed call is still a call (memory.go chatLLM.ask)
		_ = eng.Store.AddSpend(kind, cost, 1)
	}
	chat.GuardFrom(ctx).Spend(cost) // a tool's model call inside a chat turn (the turn noted the tool)
	s.noteProvider(m.Provider, err)
	class := errclass.Of(err)
	if err == nil && strings.TrimSpace(res.Content) == "" {
		err = fmt.Errorf("%s model returned no text (finish_reason=%s)", src, res.FinishReason)
		class = errclass.Empty
	}
	// the role is the feature (summary, fortune, notice digest), so the ledger shows what the call was for
	servedProvider, servedModel := servedBy(m.Provider, m.Model, res) // a fallback that answered is named, and flagged
	s.noteLLMCall(ctx, llmCall{Role: kind, Model: servedModel, Provider: servedProvider, FinishReason: res.FinishReason, PromptTokens: res.PromptTokens,
		CompletionTokens: res.CompletionTokens, CachedTokens: res.CachedTokens, Latency: time.Since(began), CostUSD: cost, Class: class, Fallback: res.Fallback})
	return strings.TrimSpace(res.Content), cost, err
}
