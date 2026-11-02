package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"masamong/internal/providers/decode"
)

// Inline batches (OpenRouter): the requests travel in the create call and the results come back in the
// poll, as soon as the batch is completed.

// inlineCreate is OpenRouter's create body. The order of its fields is part of the format: endpoint and
// model come before requests, so a struct fixes it.
type inlineCreate struct {
	Endpoint string          `json:"endpoint"`
	Model    string          `json:"model"`
	Requests []inlineRequest `json:"requests"`
}

type inlineRequest struct {
	CustomID string          `json:"custom_id"`
	Body     json.RawMessage `json:"body"`
}

type inlineBatch struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	RequestCounts struct {
		Total     int `json:"total"`
		Completed int `json:"completed"`
		Failed    int `json:"failed"`
	} `json:"request_counts"`
	Results   json.RawMessage `json:"results"`
	Error     json.RawMessage `json:"error"`
	ExpiresAt json.RawMessage `json:"expires_at"`
	Usage     struct {
		Cost *float64 `json:"cost"`
	} `json:"usage"`
}

func (b inlineBatch) hasResults() bool {
	r := bytes.TrimSpace(b.Results)
	return len(r) > 0 && string(r) != "null"
}

func (a *Adapter) submitInline(ctx context.Context, s batchSpec, kind BatchKind, model string, items []BatchItem) (string, error) {
	req := inlineCreate{Endpoint: batchEndpoint(kind), Model: model, Requests: make([]inlineRequest, 0, len(items))}
	for _, it := range items {
		var body []byte
		var err error
		if kind == BatchEmbed {
			body, err = batchEmbedBody(model, it)
		} else {
			body, err = a.batchChatBody(it)
		}
		if err != nil {
			return "", fmt.Errorf("openai: batch item %q: %w", it.ID, err)
		}
		req.Requests = append(req.Requests, inlineRequest{CustomID: it.ID, Body: body})
	}
	data, err := json.Marshal(req)
	if err != nil {
		return "", err
	}
	if len(data) > maxBatchBytes {
		return "", fmt.Errorf("openai: batch is over %d MB: split it", maxBatchBytes>>20)
	}
	resp, err := a.batchDo(ctx, "POST", s.base+"/batches", a.headers(), data, "application/json")
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) && ae.Status == 400 {
			// "no batch variant for this model": the caller runs the items live
			return "", fmt.Errorf("%w: %s", ErrBatchUnsupported, ae.Message)
		}
		return "", err
	}
	var b inlineBatch
	if json.Unmarshal(resp.Body, &b) != nil || b.ID == "" {
		return "", fmt.Errorf("%w: batch id missing", decode.ErrUnexpected)
	}
	return b.ID, nil
}

var inlineStates = map[string]BatchState{
	"validating":  BatchPending,
	"in_progress": BatchRunning,
	"finalizing":  BatchRunning,
	"completed":   BatchDone,
	"failed":      BatchFailed,
	"expired":     BatchExpired,
	"cancelled":   BatchCancelled,
}

func (a *Adapter) pollInline(ctx context.Context, s batchSpec, ref BatchRef) (BatchStatus, inlineBatch, error) {
	resp, err := a.batchDo(ctx, "GET", batchPath(s.base, ref.ID), a.headers(), nil, "")
	if err != nil {
		return BatchStatus{}, inlineBatch{}, err
	}
	var b inlineBatch
	if err := json.Unmarshal(resp.Body, &b); err != nil {
		return BatchStatus{}, inlineBatch{}, fmt.Errorf("%w: batch: %v", decode.ErrUnexpected, err)
	}
	state, ok := inlineStates[b.Status]
	if !ok {
		return BatchStatus{}, inlineBatch{}, fmt.Errorf("%w: batch status %q", decode.ErrUnexpected, b.Status)
	}
	if state == BatchDone && !b.hasResults() {
		state = BatchRunning // the results land a moment after "completed"
	}
	return BatchStatus{State: state, Total: b.RequestCounts.Total, Completed: b.RequestCounts.Completed,
		Failed: b.RequestCounts.Failed, Error: batchErrText(b.Error), ExpiresAt: batchTime(b.ExpiresAt)}, b, nil
}

func (a *Adapter) resultsInline(ctx context.Context, s batchSpec, ref BatchRef) ([]BatchOutcome, error) {
	st, b, err := a.pollInline(ctx, s, ref)
	if err != nil {
		return nil, err
	}
	switch {
	case !st.State.Final():
		return nil, errNotFinal(st)
	case st.State == BatchFailed:
		return nil, errBatchFailed(st)
	case !b.hasResults():
		return nil, nil // expired or cancelled with nothing finished
	}
	var rows []batchRow
	if err := json.Unmarshal(b.Results, &rows); err != nil {
		return nil, fmt.Errorf("%w: batch results: %v", decode.ErrUnexpected, err)
	}
	out, err := a.rowOutcomes(ref, rows, s.discount())
	if err != nil {
		return nil, err
	}
	if b.Usage.Cost != nil {
		shareBatchCost(out, *b.Usage.Cost)
	}
	return out, nil
}

// shareBatchCost spreads the batch's reported total over the outcomes that carry no reported cost of
// their own, by their tokens, replacing any estimate. The shares are estimates.
func shareBatchCost(out []BatchOutcome, total float64) {
	type slot struct {
		cost   **float64
		est    *bool
		weight int
	}
	var slots []slot
	weights := 0
	for i := range out {
		o := &out[i]
		switch {
		case o.Chat != nil:
			if o.Chat.CostUSD != nil && !o.Chat.CostEstimated {
				total -= *o.Chat.CostUSD
				continue
			}
			w := o.Chat.PromptTokens + o.Chat.CompletionTokens
			slots, weights = append(slots, slot{&o.Chat.CostUSD, &o.Chat.CostEstimated, w}), weights+w
		case o.Embed != nil:
			if o.Embed.CostUSD != nil && !o.Embed.CostEstimated {
				total -= *o.Embed.CostUSD
				continue
			}
			w := o.Embed.PromptTokens
			slots, weights = append(slots, slot{&o.Embed.CostUSD, &o.Embed.CostEstimated, w}), weights+w
		}
	}
	if weights <= 0 || total < 0 {
		return
	}
	for _, sl := range slots {
		v := total * float64(sl.weight) / float64(weights)
		*sl.cost, *sl.est = &v, true
	}
}

func (a *Adapter) cleanupInline(ctx context.Context, s batchSpec, ref BatchRef) error {
	// refused until the batch has finished; the caller only cleans up finished ones
	_, err := a.batchDo(ctx, "DELETE", batchPath(s.base, ref.ID), a.headers(), nil, "")
	if isNotFound(err) {
		return nil
	}
	return err
}
