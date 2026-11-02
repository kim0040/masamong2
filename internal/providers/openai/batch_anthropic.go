package openai

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"masamong/internal/errclass"
	"masamong/internal/providers/decode"
)

// Anthropic's native Message Batches. The adapter otherwise talks to Anthropic's OpenAI-compatible
// /chat/completions; batches exist only natively, so the chat request is translated here: system
// messages become the top-level system, user and assistant messages the messages, and the optional
// fields (thinking, temperature, output limit) are the ones the live payload would send.

type anthropicBatchCreate struct {
	Requests []anthropicBatchRequest `json:"requests"`
}

type anthropicBatchRequest struct {
	CustomID string          `json:"custom_id"`
	Params   anthropicParams `json:"params"`
}

type anthropicParams struct {
	Model       string          `json:"model"`
	MaxTokens   int             `json:"max_tokens"`
	Messages    []anthropicMsg  `json:"messages"`
	System      string          `json:"system,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	Thinking    json.RawMessage `json:"thinking,omitempty"`
}

type anthropicMsg struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

// anthropicHeaders authenticate a native call: x-api-key, not a bearer token.
func (a *Adapter) anthropicHeaders() map[string]string {
	h := map[string]string{"x-api-key": a.Cred.Value(), "anthropic-version": anthropicVersion}
	for k, v := range a.Headers {
		h[k] = v
	}
	return h
}

// anthropicParamsOf translates one chat request.
func (a *Adapter) anthropicParamsOf(it BatchItem) (anthropicParams, error) {
	req := *it.Chat
	p := anthropicParams{Model: req.Model}
	var system []string
	for _, m := range req.Messages {
		if len(m.Raw) > 0 {
			return p, fmt.Errorf("openai: batch item %q carries a raw message, which a native Anthropic batch cannot take", it.ID)
		}
		if strings.TrimSpace(m.Content) == "" {
			return p, fmt.Errorf("openai: batch item %q has an empty %s message", it.ID, m.Role)
		}
		switch m.Role {
		case "system", "developer":
			system = append(system, m.Content)
		case "user", "assistant":
			p.Messages = append(p.Messages, anthropicMsg{Role: m.Role, Content: m.Content})
		default:
			return p, fmt.Errorf("openai: batch item %q has a %q message, which a native Anthropic batch cannot take", it.ID, m.Role)
		}
	}
	if len(p.Messages) == 0 || p.Messages[0].Role != "user" {
		return p, fmt.Errorf("openai: batch item %q must start with a user message", it.ID)
	}
	p.System = strings.Join(system, "\n\n")
	// the live payload decides the optional fields, learned shape included
	live := a.payload(req, a.shapeOf(req))
	for _, k := range []string{"max_tokens", "max_completion_tokens"} {
		if n, ok := live[k].(int); ok {
			p.MaxTokens = n
		}
	}
	if t, ok := live["temperature"].(float64); ok {
		p.Temperature = &t
	}
	if th, ok := live["thinking"]; ok {
		p.Thinking, _ = json.Marshal(th)
	}
	return p, nil
}

func (a *Adapter) submitAnthropic(ctx context.Context, s batchSpec, model string, items []BatchItem) (id, resultsURL string, err error) {
	req := anthropicBatchCreate{Requests: make([]anthropicBatchRequest, 0, len(items))}
	for _, it := range items {
		p, err := a.anthropicParamsOf(it)
		if err != nil {
			return "", "", err
		}
		req.Requests = append(req.Requests, anthropicBatchRequest{CustomID: it.ID, Params: p})
	}
	data, err := json.Marshal(req)
	if err != nil {
		return "", "", err
	}
	if len(data) > maxBatchBytes {
		return "", "", fmt.Errorf("openai: batch is over %d MB: split it", maxBatchBytes>>20)
	}
	var b anthropicBatch
	id, _, err = a.createBatch(ctx, s.base+"/messages/batches", a.anthropicHeaders(), data, func(body []byte) string {
		if json.Unmarshal(body, &b) != nil {
			return ""
		}
		return b.ID
	})
	if err != nil {
		return "", "", err
	}
	return id, b.resultsURL(), nil
}

type anthropicBatch struct {
	ID               string  `json:"id"`
	ProcessingStatus string  `json:"processing_status"`
	ResultsURL       *string `json:"results_url"`
	ExpiresAt        string  `json:"expires_at"`
	CancelInitiated  *string `json:"cancel_initiated_at"`
	RequestCounts    struct {
		Processing int `json:"processing"`
		Succeeded  int `json:"succeeded"`
		Errored    int `json:"errored"`
		Canceled   int `json:"canceled"`
		Expired    int `json:"expired"`
	} `json:"request_counts"`
}

func (b anthropicBatch) resultsURL() string {
	if b.ResultsURL != nil {
		return *b.ResultsURL
	}
	return ""
}

func (a *Adapter) pollAnthropic(ctx context.Context, s batchSpec, ref BatchRef) (BatchStatus, anthropicBatch, error) {
	resp, err := a.batchDo(ctx, "GET", s.base+"/messages/batches/"+url.PathEscape(ref.ID), a.anthropicHeaders(), nil, "")
	if err != nil {
		return BatchStatus{}, anthropicBatch{}, err
	}
	var b anthropicBatch
	if err := json.Unmarshal(resp.Body, &b); err != nil {
		return BatchStatus{}, anthropicBatch{}, fmt.Errorf("%w: batch: %v", decode.ErrUnexpected, err)
	}
	c := b.RequestCounts
	st := BatchStatus{Total: c.Processing + c.Succeeded + c.Errored + c.Canceled + c.Expired, Completed: c.Succeeded,
		Failed: c.Errored + c.Canceled + c.Expired, ExpiresAt: parseRFC3339(b.ExpiresAt)}
	switch b.ProcessingStatus {
	case "in_progress", "canceling":
		st.State = BatchRunning
	case "ended":
		switch {
		case b.CancelInitiated != nil && *b.CancelInitiated != "":
			st.State = BatchCancelled
		case c.Expired > 0:
			st.State = BatchExpired
		default:
			st.State = BatchDone
		}
	default:
		return BatchStatus{}, anthropicBatch{}, fmt.Errorf("%w: batch status %q", decode.ErrUnexpected, b.ProcessingStatus)
	}
	return st, b, nil
}

func parseRFC3339(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		return time.Time{}
	}
	return t.UTC()
}

type anthropicRow struct {
	CustomID string `json:"custom_id"`
	Result   struct {
		Type    string `json:"type"`
		Message *struct {
			Model      string `json:"model"`
			StopReason string `json:"stop_reason"`
			Content    []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Usage struct {
				InputTokens         int `json:"input_tokens"`
				OutputTokens        int `json:"output_tokens"`
				CacheCreationTokens int `json:"cache_creation_input_tokens"`
				CacheReadTokens     int `json:"cache_read_input_tokens"`
			} `json:"usage"`
		} `json:"message"`
		Error *struct {
			Type    string `json:"type"`
			Message string `json:"message"`
			Error   struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		} `json:"error"`
	} `json:"result"`
}

// anthropicStatus is the HTTP status an error type stands for, so the error classes match a live call's.
func anthropicStatus(errType string) int {
	switch errType {
	case "invalid_request_error":
		return 400
	case "authentication_error":
		return 401
	case "permission_error":
		return 403
	case "not_found_error":
		return 404
	case "request_too_large":
		return 413
	case "rate_limit_error":
		return 429
	case "overloaded_error":
		return 529
	}
	return 500
}

func anthropicFinish(stop string) string {
	switch stop {
	case "max_tokens":
		return "length"
	case "end_turn", "stop_sequence":
		return "stop"
	case "refusal":
		return "content_filter"
	}
	return stop
}

func (a *Adapter) anthropicOutcome(ref BatchRef, r anthropicRow, discount float64) BatchOutcome {
	o := BatchOutcome{ID: r.CustomID}
	switch r.Result.Type {
	case "succeeded":
		m := r.Result.Message
		if m == nil {
			o.Err = fmt.Errorf("%w: batch row without a message", decode.ErrUnexpected)
			break
		}
		var text strings.Builder
		for _, c := range m.Content {
			if c.Type == "text" {
				text.WriteString(c.Text)
			}
		}
		msg, _ := json.Marshal(map[string]string{"role": "assistant", "content": text.String()})
		u := m.Usage
		res := ChatResult{Content: text.String(), FinishReason: anthropicFinish(m.StopReason), Model: m.Model,
			PromptTokens: u.InputTokens + u.CacheCreationTokens + u.CacheReadTokens, CompletionTokens: u.OutputTokens,
			CachedTokens: u.CacheReadTokens, Message: msg}
		a.batchChatCost(&res, ref.Model, ref.SubmittedAt, discount)
		o.Chat = &res
	case "errored":
		e := r.Result.Error
		if e == nil {
			o.Err = errors.New("openai: batch item errored")
			break
		}
		typ, msg := e.Error.Type, e.Error.Message
		if typ == "" {
			typ, msg = e.Type, e.Message
		}
		o.Err = &APIError{Status: anthropicStatus(typ), Type: typ, Message: msg}
	case "canceled":
		o.Err = batchItemError("was canceled", errclass.Canceled)
	case "expired":
		o.Err = batchItemError("expired before it ran", errclass.Timeout)
	default:
		o.Err = fmt.Errorf("%w: batch result type %q", decode.ErrUnexpected, r.Result.Type)
	}
	return o
}

func (a *Adapter) resultsAnthropic(ctx context.Context, s batchSpec, ref BatchRef) ([]BatchOutcome, error) {
	st, b, err := a.pollAnthropic(ctx, s, ref)
	if err != nil {
		return nil, err
	}
	if !st.State.Final() {
		return nil, errNotFinal(st)
	}
	// the results address comes from the host; follow it only on the host this adapter talks to, since
	// the call carries the key
	u := b.resultsURL()
	if u == "" {
		u = ref.ResultsURL
	}
	if !sameOrigin(u, a.BaseURL) {
		u = s.base + "/messages/batches/" + url.PathEscape(ref.ID) + "/results"
	}
	resp, err := a.batchDo(ctx, "GET", u, a.anthropicHeaders(), nil, "")
	if err != nil {
		return nil, err
	}
	var out []BatchOutcome
	for _, line := range splitJSONL(resp.Body) {
		var r anthropicRow
		if err := json.Unmarshal(line, &r); err != nil {
			return nil, fmt.Errorf("%w: batch result row", decode.ErrUnexpected)
		}
		if !batchIDRE.MatchString(r.CustomID) {
			return nil, fmt.Errorf("%w: batch row without a usable custom_id", decode.ErrUnexpected)
		}
		out = append(out, a.anthropicOutcome(ref, r, s.discountFor(ref.Model)))
	}
	return out, nil
}

func (a *Adapter) cancelAnthropic(ctx context.Context, s batchSpec, ref BatchRef) error {
	_, err := a.batchDo(ctx, "POST", s.base+"/messages/batches/"+url.PathEscape(ref.ID)+"/cancel", a.anthropicHeaders(), nil, "")
	return err
}

func (a *Adapter) cleanupAnthropic(ctx context.Context, s batchSpec, ref BatchRef) error {
	_, err := a.batchDo(ctx, "DELETE", s.base+"/messages/batches/"+url.PathEscape(ref.ID), a.anthropicHeaders(), nil, "")
	if isNotFound(err) {
		return nil
	}
	return err
}
