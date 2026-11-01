package application

import (
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
	// the summary role does extraction and short write-ups under a tight token cap: a thinking model spends
	// that cap on reasoning and hands back cut JSON (postmortem 0005), so it is asked not to think
	effort := "low"
	if role == "summary" { // the job decides, not the model it falls back to
		effort = "none"
	}
	if kind == "fortune" {
		// a fortune is a short write-up under a 700–1800 token cap: thinking spent most of it and cut the
		// reading off mid-sentence (ops 2026-10-14: 3 of 5 ended at finish_reason=length)
		effort = "none"
	}
	began := time.Now()
	res, err := a.Chat(ctx, openai.ChatRequest{Model: m.Model, Messages: msgs, MaxTokens: maxTokens, Temperature: &t, Effort: effort})
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
	s.noteLLMCall(ctx, llmCall{Role: kind, Model: m.Model, Provider: m.Provider, FinishReason: res.FinishReason, PromptTokens: res.PromptTokens,
		CompletionTokens: res.CompletionTokens, CachedTokens: res.CachedTokens, Latency: time.Since(began), CostUSD: cost, Class: class})
	return strings.TrimSpace(res.Content), cost, err
}
