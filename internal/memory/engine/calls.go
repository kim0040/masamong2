package engine

import (
	"context"
	"time"

	"masamong/internal/errclass"
)

// CallProviders names the configured provider for each memory model role.
type CallProviders struct {
	Embed, Rerank, Summary string
}

// CallInfo is a body-free account of one memory model request.
type CallInfo struct {
	Role             string
	Model            string
	Provider         string
	CostUSD          float64
	Latency          time.Duration
	Class            errclass.Class
	PromptTokens     int
	CompletionTokens int
	CachedTokens     int
}

// recordCall reports a provider request to the optional application ledger hook. The caller invokes
// it only after crossing the adapter's preflight checks, so cache hits and rejected local inputs do not
// look like provider calls. Cost fallback values match the corresponding existing memory_spend path.
func (e *Engine) recordCall(ctx context.Context, role, model, provider string, cost *float64, fallback float64,
	latency time.Duration, promptTokens, completionTokens, cachedTokens int, err error) {
	if e.OnCall == nil {
		return
	}
	v := fallback
	if cost != nil {
		v = *cost
	}
	e.OnCall(ctx, CallInfo{Role: role, Model: model, Provider: provider, CostUSD: v, Latency: latency,
		Class: errclass.Of(err), PromptTokens: promptTokens, CompletionTokens: completionTokens, CachedTokens: cachedTokens})
}

// emptyReply marks, in the call log, a call that succeeded without text (a reasoning model that spent the
// whole output budget thinking), so the dashboard shows it as empty instead of fine.
type emptyReply struct{ finish string }

func (e emptyReply) Error() string          { return "model returned no text (finish_reason=" + e.finish + ")" }
func (emptyReply) ErrClass() errclass.Class { return errclass.Empty }
