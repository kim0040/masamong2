package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"masamong/internal/errclass"
	"masamong/internal/providers"
	"masamong/internal/providers/decode"
)

// Batch support. Each host family has its own wire format, named by the "style" of the host's "batch"
// entry in hosts.json:
//
//	openai_files   upload a JSONL file, create a batch from it, read the output file   (batch_files.go)
//	gemini_files   the same batch calls, with Google's native Files API for the JSONL  (batch_files.go)
//	inline         the requests travel in the create call, the results in the poll     (batch_inline.go)
//	anthropic      Anthropic's native Message Batches                                  (batch_anthropic.go)
//
// A host without an entry reports no support and callers run the same requests live.

const (
	styleOpenAIFiles = "openai_files"
	styleGeminiFiles = "gemini_files"
	styleInline      = "inline"
	styleAnthropic   = "anthropic"

	// maxBatchBytes caps a serialized batch (input file or inline body): the hosts take 200–256 MB, and
	// a caller that is told to split can.
	maxBatchBytes = 150 << 20
	// maxBatchDownload caps one downloaded result file (the client's own cap is for chat answers).
	maxBatchDownload = 512 << 20
)

// batchSpec is a host's batch entry resolved for one adapter.
type batchSpec struct {
	BatchHost
	kinds []BatchKind
	// base is where batch calls go: the adapter's base URL, or the host's dedicated batch address
	base string
	// native is the scheme and host of the adapter's base URL (Gemini's Files API lives beside the
	// OpenAI-compatible path, not under it)
	native string
}

func (s batchSpec) has(kind BatchKind) bool {
	for _, k := range s.kinds {
		if k == kind {
			return true
		}
	}
	return false
}

func (s batchSpec) discount() float64 {
	if s.Discount > 0 && s.Discount <= 1 {
		return s.Discount
	}
	return 0.5
}

// batchSpec finds the adapter's host entry: the family's own for openai/openrouter/anthropic/gemini (a
// custom address on another host still speaks the family's format), the listed host by address for the
// compatible family (CometAPI, NanoGPT, OpenGateway, anything else has no batch API).
func (a *Adapter) batchSpec() (batchSpec, bool) {
	var h Host
	var ok bool
	if a.API == APICompatible {
		for _, c := range hostsData.Hosts {
			if c.API == APICompatible && c.Batch != nil && (sameAuthority(a.BaseURL, c.BaseURL) || sameAuthority(a.BaseURL, c.Batch.BaseURL)) {
				h, ok = c, true
				break
			}
		}
	} else {
		h, ok = HostFor(a.API)
	}
	if !ok || h.Batch == nil {
		return batchSpec{}, false
	}
	s := batchSpec{BatchHost: *h.Batch, base: a.BaseURL}
	for _, k := range h.Batch.Kinds {
		s.kinds = append(s.kinds, BatchKind(k))
	}
	if h.Batch.BaseURL != "" && sameAuthority(a.BaseURL, h.BaseURL) {
		s.base = strings.TrimRight(h.Batch.BaseURL, "/")
	}
	if u, err := url.Parse(a.BaseURL); err == nil {
		s.native = u.Scheme + "://" + u.Host
	}
	return s, true
}

func sameAuthority(a, b string) bool {
	ua, errA := url.Parse(strings.TrimSpace(a))
	ub, errB := url.Parse(strings.TrimSpace(b))
	return errA == nil && errB == nil && ua.Host != "" && strings.EqualFold(ua.Host, ub.Host)
}

// sameOrigin is sameAuthority with the same scheme.
func sameOrigin(a, b string) bool {
	ua, errA := url.Parse(strings.TrimSpace(a))
	ub, errB := url.Parse(strings.TrimSpace(b))
	return errA == nil && errB == nil && strings.EqualFold(ua.Scheme, ub.Scheme) && sameAuthority(a, b)
}

// BatchSupport reports whether this adapter's host can batch the kind (the model may still be refused
// at submit, with ErrBatchUnsupported).
func (a *Adapter) BatchSupport(kind BatchKind) bool {
	s, ok := a.batchSpec()
	return ok && s.has(kind)
}

// BatchMaxItems is the most items one batch of the kind may carry on this host; callers split above it.
func (a *Adapter) BatchMaxItems(kind BatchKind) int {
	if s, ok := a.batchSpec(); ok && s.has(kind) {
		return s.MaxItems
	}
	return 0
}

var batchIDRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// validateBatch checks what is wrong with a batch before anything is sent.
func (a *Adapter) validateBatch(s batchSpec, kind BatchKind, model string, items []BatchItem) error {
	if !s.has(kind) {
		return ErrBatchUnsupported
	}
	if strings.TrimSpace(model) == "" {
		return errors.New("openai: batch needs a model")
	}
	if len(items) == 0 {
		return errors.New("openai: batch has no items")
	}
	if s.MaxItems > 0 && len(items) > s.MaxItems {
		return fmt.Errorf("openai: batch has %d items, this host takes %d: split it", len(items), s.MaxItems)
	}
	seen := make(map[string]struct{}, len(items))
	inputs := 0
	for _, it := range items {
		if !batchIDRE.MatchString(it.ID) {
			return fmt.Errorf("openai: batch item id %q must be 1-64 characters of A-Z a-z 0-9 _ -", it.ID)
		}
		if _, dup := seen[it.ID]; dup {
			return fmt.Errorf("openai: batch item id %q is used twice", it.ID)
		}
		seen[it.ID] = struct{}{}
		switch kind {
		case BatchChat:
			if err := validateBatchChat(s, model, it); err != nil {
				return err
			}
		case BatchEmbed:
			if len(it.Input) == 0 {
				return fmt.Errorf("openai: batch item %q has no inputs", it.ID)
			}
			inputs += len(it.Input)
		default:
			return fmt.Errorf("openai: unknown batch kind %q", kind)
		}
	}
	if kind == BatchEmbed && s.MaxInputs > 0 && inputs > s.MaxInputs {
		return fmt.Errorf("openai: embeddings batch has %d inputs, this host takes %d: split it", inputs, s.MaxInputs)
	}
	return nil
}

func validateBatchChat(s batchSpec, model string, it BatchItem) error {
	r := it.Chat
	switch {
	case r == nil:
		return fmt.Errorf("openai: batch item %q has no chat request", it.ID)
	case r.Model != model:
		return fmt.Errorf("openai: batch item %q asks model %q, the batch runs %q", it.ID, r.Model, model)
	case len(r.Messages) == 0:
		return fmt.Errorf("openai: batch item %q has no messages", it.ID)
	case len(r.Tools) > 0:
		return fmt.Errorf("openai: batch item %q carries tools, which a batch cannot run", it.ID)
	case hasImages(*r):
		return fmt.Errorf("openai: batch item %q carries pictures, which a batch cannot run", it.ID)
	case (s.RequireMaxTokens || s.Style == styleAnthropic) && r.MaxTokens <= 0:
		return fmt.Errorf("openai: batch item %q needs a positive output limit (MaxTokens) on this host", it.ID)
	}
	return nil
}

// SubmitBatch sends the items as one batch for model.
func (a *Adapter) SubmitBatch(ctx context.Context, kind BatchKind, model string, items []BatchItem) (BatchRef, error) {
	s, ok := a.batchSpec()
	if !ok {
		return BatchRef{}, ErrBatchUnsupported
	}
	if err := a.validateBatch(s, kind, model, items); err != nil {
		return BatchRef{}, err
	}
	if !a.Configured() {
		return BatchRef{}, providers.ErrNoCredential
	}
	if err := a.gate(); err != nil {
		return BatchRef{}, err
	}
	var id, inputFile, resultsURL string
	var err error
	switch s.Style {
	case styleOpenAIFiles, styleGeminiFiles:
		id, inputFile, err = a.submitFiles(ctx, s, kind, model, items)
	case styleInline:
		id, err = a.submitInline(ctx, s, kind, model, items)
	case styleAnthropic:
		id, resultsURL, err = a.submitAnthropic(ctx, s, model, items)
	default:
		return BatchRef{}, fmt.Errorf("openai: batch style %q is not known", s.Style)
	}
	if err != nil {
		return BatchRef{}, err
	}
	return BatchRef{API: a.API, BaseURL: a.BaseURL, ID: id, Kind: kind, Model: model, Count: len(items),
		InputFile: inputFile, ResultsURL: resultsURL, SubmittedAt: time.Now().UTC()}, nil
}

// refSpec checks that ref was submitted to this adapter's host and returns the host's batch entry.
func (a *Adapter) refSpec(ref BatchRef) (batchSpec, error) {
	s, ok := a.batchSpec()
	if !ok {
		return batchSpec{}, ErrBatchUnsupported
	}
	if strings.TrimSpace(ref.ID) == "" {
		return batchSpec{}, errors.New("openai: batch reference has no id")
	}
	if strings.TrimRight(ref.BaseURL, "/") != a.BaseURL {
		return batchSpec{}, errors.New("openai: this batch was submitted to another host")
	}
	if !a.Configured() {
		return batchSpec{}, providers.ErrNoCredential
	}
	return s, nil
}

// PollBatch reads a submitted batch's state.
func (a *Adapter) PollBatch(ctx context.Context, ref BatchRef) (BatchStatus, error) {
	s, err := a.refSpec(ref)
	if err != nil {
		return BatchStatus{}, err
	}
	switch s.Style {
	case styleOpenAIFiles, styleGeminiFiles:
		st, _, err := a.pollFiles(ctx, s, ref)
		return st, err
	case styleInline:
		st, _, err := a.pollInline(ctx, s, ref)
		return st, err
	case styleAnthropic:
		st, _, err := a.pollAnthropic(ctx, s, ref)
		return st, err
	}
	return BatchStatus{}, fmt.Errorf("openai: batch style %q is not known", s.Style)
}

// BatchResults reads the outcomes of a batch in a final state, one per item that has one. Expired and
// cancelled batches return the rows that finished; a failed one has none and returns its error. The
// outcomes come in the host's order, not the items'.
func (a *Adapter) BatchResults(ctx context.Context, ref BatchRef) ([]BatchOutcome, error) {
	s, err := a.refSpec(ref)
	if err != nil {
		return nil, err
	}
	switch s.Style {
	case styleOpenAIFiles, styleGeminiFiles:
		return a.resultsFiles(ctx, s, ref)
	case styleInline:
		return a.resultsInline(ctx, s, ref)
	case styleAnthropic:
		return a.resultsAnthropic(ctx, s, ref)
	}
	return nil, fmt.Errorf("openai: batch style %q is not known", s.Style)
}

// CancelBatch asks the host to stop a batch (best effort; finished items are still charged).
func (a *Adapter) CancelBatch(ctx context.Context, ref BatchRef) error {
	s, err := a.refSpec(ref)
	if err != nil {
		return err
	}
	switch s.Style {
	case styleOpenAIFiles, styleGeminiFiles:
		return a.cancelFiles(ctx, s, ref)
	case styleInline:
		return nil // OpenRouter has no cancel call
	case styleAnthropic:
		return a.cancelAnthropic(ctx, s, ref)
	}
	return fmt.Errorf("openai: batch style %q is not known", s.Style)
}

// CleanupBatch deletes what the batch left on the host (input and output files, the stored batch) once
// its results are saved. Best effort.
func (a *Adapter) CleanupBatch(ctx context.Context, ref BatchRef) error {
	s, err := a.refSpec(ref)
	if err != nil {
		return err
	}
	switch s.Style {
	case styleOpenAIFiles, styleGeminiFiles:
		return a.cleanupFiles(ctx, s, ref)
	case styleInline:
		return a.cleanupInline(ctx, s, ref)
	case styleAnthropic:
		return a.cleanupAnthropic(ctx, s, ref)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Shared helpers
// ---------------------------------------------------------------------------

// batchClient reads responses far larger than a chat answer (a result file).
func (a *Adapter) batchClient() *providers.Client { return a.Client.WithMaxBody(maxBatchDownload) }

// batchDo sends one batch call through the shared client, with the error mapped like every other call
// (a refused key stays ErrAuth).
func (a *Adapter) batchDo(ctx context.Context, method, rawURL string, headers map[string]string, body []byte, contentType string) (*providers.Response, error) {
	c := a.batchClient()
	var resp *providers.Response
	var err error
	switch method {
	case "GET":
		resp, err = c.Get(ctx, rawURL, headers)
	case "DELETE":
		resp, err = c.Delete(ctx, rawURL, headers)
	default:
		resp, err = c.Post(ctx, rawURL, headers, body, contentType)
	}
	if err != nil {
		return resp, mapErr(err, resp)
	}
	return resp, nil
}

// isNotFound reports a 404, which a cleanup treats as "already gone".
func isNotFound(err error) bool {
	var ae *APIError
	if errors.As(err, &ae) {
		return ae.Status == 404
	}
	var pe *providers.Error
	return errors.As(err, &pe) && pe.Status == 404
}

// errNotFinal is returned when results are asked of a batch that is still running.
func errNotFinal(st BatchStatus) error {
	return fmt.Errorf("openai: batch is not finished yet (%s)", st.State)
}

func errBatchFailed(st BatchStatus) error {
	if st.Error != "" {
		return fmt.Errorf("openai: batch failed: %s", st.Error)
	}
	return errors.New("openai: batch failed")
}

// batchErrText reads a batch's error field: a message string, {message}, {data:[{message}]} or a list.
func batchErrText(raw json.RawMessage) string {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var one struct {
		Message string          `json:"message"`
		Data    json.RawMessage `json:"data"`
	}
	var list []struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &list) != nil {
		if json.Unmarshal(raw, &one) != nil {
			return ""
		}
		if one.Message != "" {
			return one.Message
		}
		if json.Unmarshal(one.Data, &list) != nil {
			return ""
		}
	}
	var msgs []string
	for i, e := range list {
		if i == 3 {
			msgs = append(msgs, "…")
			break
		}
		if e.Message != "" {
			msgs = append(msgs, e.Message)
		}
	}
	return strings.Join(msgs, "; ")
}

// batchTime reads a deadline: unix seconds or an RFC 3339 string; zero when absent or unreadable.
func batchTime(raw json.RawMessage) time.Time {
	raw = bytes.TrimSpace(raw)
	var s string
	if json.Unmarshal(raw, &s) == nil {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UTC()
		}
		return time.Time{}
	}
	if n, ok := decode.Number(raw); ok && n > 0 {
		return time.Unix(int64(n), 0).UTC()
	}
	return time.Time{}
}

// batchRow is one line of an OpenAI-format output file, or one entry of OpenRouter's results.
type batchRow struct {
	CustomID string `json:"custom_id"`
	Response *struct {
		StatusCode int             `json:"status_code"`
		Body       json.RawMessage `json:"body"`
	} `json:"response"`
	Error json.RawMessage `json:"error"`
}

// splitJSONL cuts a JSONL body into its non-empty lines.
func splitJSONL(body []byte) [][]byte {
	var out [][]byte
	for _, l := range bytes.Split(body, []byte("\n")) {
		if l = bytes.TrimSpace(l); len(l) > 0 {
			out = append(out, l)
		}
	}
	return out
}

// rowOutcomes turns OpenAI-format rows into outcomes.
func (a *Adapter) rowOutcomes(ref BatchRef, rows []batchRow, discount float64) ([]BatchOutcome, error) {
	out := make([]BatchOutcome, 0, len(rows))
	for _, r := range rows {
		if !batchIDRE.MatchString(r.CustomID) {
			return nil, fmt.Errorf("%w: batch row without a usable custom_id", decode.ErrUnexpected)
		}
		out = append(out, a.rowOutcome(ref, r, discount))
	}
	return out, nil
}

func (a *Adapter) rowOutcome(ref BatchRef, r batchRow, discount float64) BatchOutcome {
	o := BatchOutcome{ID: r.CustomID}
	hasErr := len(bytes.TrimSpace(r.Error)) > 0 && string(bytes.TrimSpace(r.Error)) != "null"
	switch {
	case r.Response != nil && r.Response.StatusCode >= 400:
		o.Err = rowStatusError(r.Response.StatusCode, r.Response.Body)
	case hasErr:
		o.Err = apiError(r.Error, 0)
	case r.Response == nil || len(r.Response.Body) == 0:
		o.Err = fmt.Errorf("%w: batch row has neither a response nor an error", decode.ErrUnexpected)
	case ref.Kind == BatchEmbed:
		res, err := parseBatchEmbeddings(r.Response.Body)
		if err != nil {
			o.Err = err
			break
		}
		a.batchEmbedCost(&res, ref, discount)
		o.Embed = &res
	default:
		res, err := ParseChat("application/json", r.Response.Body)
		if err != nil {
			o.Err = err
			break
		}
		a.batchChatCost(&res, ref.Model, ref.SubmittedAt, discount)
		o.Chat = &res
	}
	return o
}

// rowStatusError is a row's failed HTTP answer, mapped like a live call's.
func rowStatusError(status int, body json.RawMessage) error {
	pe := &providers.Error{Kind: "status", Status: status, URL: "batch row", Attempts: 1}
	return mapErr(pe, &providers.Response{Status: status, ContentType: "application/json", Body: body})
}

// parseBatchEmbeddings reads an embeddings answer without knowing how many inputs it was asked: after a
// restart the items are gone, and the answer itself says (one vector per input).
func parseBatchEmbeddings(body []byte) (EmbedResult, error) {
	obj, err := decode.Object("application/json", body)
	if err != nil {
		return EmbedResult{}, err
	}
	var data []json.RawMessage
	if json.Unmarshal(obj["data"], &data) != nil || len(data) == 0 {
		if e, ok := obj["error"]; ok && string(e) != "null" {
			return EmbedResult{}, apiError(e, 0)
		}
		return EmbedResult{}, fmt.Errorf("%w: embeddings missing", decode.ErrUnexpected)
	}
	return ParseEmbeddings("application/json", body, len(data))
}

// batchChatCost leaves a reported cost alone and prices an unreported one at the host's batch price.
func (a *Adapter) batchChatCost(res *ChatResult, model string, at time.Time, discount float64) {
	if res.CostUSD != nil {
		return
	}
	a.estimateCachedAt(&res.CostUSD, &res.CostEstimated, model, res.PromptTokens, res.CachedTokens, res.CompletionTokens, at)
	if res.CostUSD != nil {
		v := *res.CostUSD * discount
		res.CostUSD = &v
	}
}

func (a *Adapter) batchEmbedCost(res *EmbedResult, ref BatchRef, discount float64) {
	if res.CostUSD != nil {
		return
	}
	a.estimateAt(&res.CostUSD, &res.CostEstimated, ref.Model, res.PromptTokens, 0, ref.SubmittedAt)
	if res.CostUSD != nil {
		v := *res.CostUSD * discount
		res.CostUSD = &v
	}
}

// batchItemError is an item the host gave no result for.
func batchItemError(msg string, class errclass.Class) error {
	return classedError{msg: "openai: batch item " + msg, class: class}
}

// batchChatBody is the body a live call of the item would send, shape learning included.
func (a *Adapter) batchChatBody(it BatchItem) ([]byte, error) {
	return json.Marshal(a.payload(*it.Chat, a.shapeOf(*it.Chat)))
}

// batchEmbedBody is the body of an embeddings request.
func batchEmbedBody(model string, it BatchItem) ([]byte, error) {
	return json.Marshal(map[string]any{"model": model, "input": it.Input})
}

func batchEndpoint(kind BatchKind) string {
	if kind == BatchEmbed {
		return "/v1/embeddings"
	}
	return "/v1/chat/completions"
}
