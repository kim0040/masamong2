package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/textproto"
	"net/url"
	"strings"

	"masamong/internal/providers/decode"
)

// File-backed batches: OpenAI, NanoGPT (same format on its own batch host) and Gemini's OpenAI-compatible
// batch API. The input is a JSONL file of {"custom_id","method","url","body"} rows, the output the same
// with {"custom_id","response","error"} rows.
//
// Gemini keeps the batch calls on its compatible path but not the files: they go through the native Files
// API (uploadGemini, downloadFile, deleteFile). If the compatible route proves unworkable, the other way
// is native inline: POST /v1beta/models/{model}:batchGenerateContent, a style of its own beside these.

// geminiBatchMIME is the type the input file is uploaded as (adjust here if the Files API wants another,
// e.g. the bare "jsonl" its SDKs take).
const geminiBatchMIME = "application/jsonl"

// geminiUploadName is the display name of an uploaded input file.
const geminiUploadName = "masamong-batch-input"

type fileRow struct {
	CustomID string          `json:"custom_id"`
	Method   string          `json:"method"`
	URL      string          `json:"url"`
	Body     json.RawMessage `json:"body"`
}

// batchJSONL writes the items as an input file.
func (a *Adapter) batchJSONL(kind BatchKind, model string, items []BatchItem) ([]byte, error) {
	var buf bytes.Buffer
	endpoint := batchEndpoint(kind)
	for _, it := range items {
		var body []byte
		var err error
		if kind == BatchEmbed {
			body, err = batchEmbedBody(model, it)
		} else {
			body, err = a.batchChatBody(it)
		}
		if err != nil {
			return nil, fmt.Errorf("openai: batch item %q: %w", it.ID, err)
		}
		row, err := json.Marshal(fileRow{CustomID: it.ID, Method: "POST", URL: endpoint, Body: body})
		if err != nil {
			return nil, err
		}
		buf.Write(row)
		buf.WriteByte('\n')
		if buf.Len() > maxBatchBytes {
			return nil, fmt.Errorf("openai: batch is over %d MB: split it", maxBatchBytes>>20)
		}
	}
	return buf.Bytes(), nil
}

// batchPath is the address of a stored batch: Gemini may name it "batches/abc".
func batchPath(base, id string) string {
	if strings.HasPrefix(id, "batches/") {
		return base + "/" + id
	}
	return base + "/batches/" + url.PathEscape(id)
}

// geminiHeaders are the headers of the native Files API.
func (a *Adapter) geminiHeaders() map[string]string {
	h := map[string]string{"x-goog-api-key": a.Cred.Value()}
	for k, v := range a.Headers {
		h[k] = v
	}
	return h
}

// geminiFileName is the Files API name ("files/abc") of whatever the batch calls it.
func geminiFileName(id string) string {
	if i := strings.Index(id, "files/"); i >= 0 {
		id = id[i:]
	} else {
		id = "files/" + id
	}
	id, _, _ = strings.Cut(id, ":")
	return id
}

func (a *Adapter) submitFiles(ctx context.Context, s batchSpec, kind BatchKind, model string, items []BatchItem) (id, inputFile string, err error) {
	data, err := a.batchJSONL(kind, model, items)
	if err != nil {
		return "", "", err
	}
	if s.Style == styleGeminiFiles {
		inputFile, err = a.uploadGemini(ctx, s, data)
	} else {
		inputFile, err = a.uploadOpenAI(ctx, s, data)
	}
	if err != nil {
		return "", "", err
	}
	body, _ := json.Marshal(map[string]string{"input_file_id": inputFile, "endpoint": batchEndpoint(kind), "completion_window": "24h"})
	resp, err := a.batchDo(ctx, "POST", s.base+"/batches", a.headers(), body, "application/json")
	if err == nil {
		var b struct {
			ID string `json:"id"`
		}
		if err = json.Unmarshal(resp.Body, &b); err != nil || b.ID == "" {
			err = fmt.Errorf("%w: batch id missing", decode.ErrUnexpected)
		} else {
			return b.ID, inputFile, nil
		}
	}
	// the batch was not created: do not leave the uploaded file behind
	_ = a.deleteFile(ctx, s, inputFile)
	return "", "", err
}

func (a *Adapter) uploadOpenAI(ctx context.Context, s batchSpec, data []byte) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	_ = w.WriteField("purpose", "batch")
	fw, err := w.CreateFormFile("file", "batch.jsonl")
	if err != nil {
		return "", err
	}
	_, _ = fw.Write(data)
	if err := w.Close(); err != nil {
		return "", err
	}
	resp, err := a.batchDo(ctx, "POST", s.base+"/files", a.headers(), buf.Bytes(), w.FormDataContentType())
	if err != nil {
		return "", err
	}
	var f struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(resp.Body, &f) != nil || f.ID == "" {
		return "", fmt.Errorf("%w: uploaded file id missing", decode.ErrUnexpected)
	}
	return f.ID, nil
}

// uploadGemini sends the input file to the native Files API in one multipart/related request (a metadata
// part, then the file), and returns the file's name ("files/abc"), which is the batch's input_file_id.
// Not verified against the live service: see the report.
func (a *Adapter) uploadGemini(ctx context.Context, s batchSpec, data []byte) (string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	meta, _ := json.Marshal(map[string]any{"file": map[string]string{"display_name": geminiUploadName}})
	for _, part := range []struct {
		contentType string
		body        []byte
	}{{"application/json; charset=UTF-8", meta}, {geminiBatchMIME, data}} {
		pw, err := w.CreatePart(textproto.MIMEHeader{"Content-Type": {part.contentType}})
		if err != nil {
			return "", err
		}
		_, _ = pw.Write(part.body)
	}
	if err := w.Close(); err != nil {
		return "", err
	}
	h := a.geminiHeaders()
	h["X-Goog-Upload-Protocol"] = "multipart"
	resp, err := a.batchDo(ctx, "POST", s.native+"/upload/v1beta/files", h, buf.Bytes(), "multipart/related; boundary="+w.Boundary())
	if err != nil {
		return "", err
	}
	var f struct {
		File struct {
			Name string `json:"name"`
		} `json:"file"`
		Name string `json:"name"`
	}
	if json.Unmarshal(resp.Body, &f) != nil {
		return "", fmt.Errorf("%w: upload answer", decode.ErrUnexpected)
	}
	name := f.File.Name
	if name == "" {
		name = f.Name
	}
	if name == "" {
		return "", fmt.Errorf("%w: uploaded file name missing", decode.ErrUnexpected)
	}
	return geminiFileName(name), nil
}

// fileBatch is a stored batch in the OpenAI shape.
type fileBatch struct {
	ID            string `json:"id"`
	Status        string `json:"status"`
	RequestCounts struct {
		Total     int `json:"total"`
		Completed int `json:"completed"`
		Failed    int `json:"failed"`
	} `json:"request_counts"`
	OutputFileID string          `json:"output_file_id"`
	ErrorFileID  string          `json:"error_file_id"`
	Errors       json.RawMessage `json:"errors"`
	ExpiresAt    json.RawMessage `json:"expires_at"`
}

var fileBatchStates = map[string]BatchState{
	"validating":  BatchPending,
	"in_progress": BatchRunning,
	"finalizing":  BatchRunning,
	"cancelling":  BatchRunning,
	"completed":   BatchDone,
	"failed":      BatchFailed,
	"expired":     BatchExpired,
	"cancelled":   BatchCancelled,
}

func (a *Adapter) pollFiles(ctx context.Context, s batchSpec, ref BatchRef) (BatchStatus, fileBatch, error) {
	resp, err := a.batchDo(ctx, "GET", batchPath(s.base, ref.ID), a.headers(), nil, "")
	if err != nil {
		return BatchStatus{}, fileBatch{}, err
	}
	var b fileBatch
	if err := json.Unmarshal(resp.Body, &b); err != nil {
		return BatchStatus{}, fileBatch{}, fmt.Errorf("%w: batch: %v", decode.ErrUnexpected, err)
	}
	state, ok := fileBatchStates[b.Status]
	if !ok {
		return BatchStatus{}, fileBatch{}, fmt.Errorf("%w: batch status %q", decode.ErrUnexpected, b.Status)
	}
	return BatchStatus{State: state, Total: b.RequestCounts.Total, Completed: b.RequestCounts.Completed,
		Failed: b.RequestCounts.Failed, Error: batchErrText(b.Errors), ExpiresAt: batchTime(b.ExpiresAt)}, b, nil
}

func (a *Adapter) resultsFiles(ctx context.Context, s batchSpec, ref BatchRef) ([]BatchOutcome, error) {
	st, b, err := a.pollFiles(ctx, s, ref)
	if err != nil {
		return nil, err
	}
	switch {
	case !st.State.Final():
		return nil, errNotFinal(st)
	case st.State == BatchFailed:
		return nil, errBatchFailed(st)
	}
	var rows []batchRow
	files := 0
	for _, id := range []string{b.OutputFileID, b.ErrorFileID} {
		if id == "" {
			continue
		}
		files++
		body, err := a.downloadFile(ctx, s, id)
		if err != nil {
			return nil, err
		}
		for _, line := range splitJSONL(body) {
			var r batchRow
			if err := json.Unmarshal(line, &r); err != nil {
				return nil, fmt.Errorf("%w: batch output row", decode.ErrUnexpected)
			}
			rows = append(rows, r)
		}
	}
	if files == 0 && st.State == BatchDone && st.Total > 0 {
		return nil, fmt.Errorf("%w: batch finished with no output file", decode.ErrUnexpected)
	}
	return a.rowOutcomes(ref, rows, s.discount())
}

func (a *Adapter) downloadFile(ctx context.Context, s batchSpec, id string) ([]byte, error) {
	if s.Style == styleGeminiFiles {
		u := s.native + "/download/v1beta/" + geminiFileName(id) + ":download?alt=media"
		r, err := a.batchDo(ctx, "GET", u, a.geminiHeaders(), nil, "")
		if err != nil {
			return nil, err
		}
		return r.Body, nil
	}
	r, err := a.batchDo(ctx, "GET", s.base+"/files/"+url.PathEscape(id)+"/content", a.headers(), nil, "")
	if err != nil {
		return nil, err
	}
	return r.Body, nil
}

func (a *Adapter) deleteFile(ctx context.Context, s batchSpec, id string) error {
	if id == "" {
		return nil
	}
	var err error
	if s.Style == styleGeminiFiles {
		_, err = a.batchDo(ctx, "DELETE", s.native+"/v1beta/"+geminiFileName(id), a.geminiHeaders(), nil, "")
	} else {
		_, err = a.batchDo(ctx, "DELETE", s.base+"/files/"+url.PathEscape(id), a.headers(), nil, "")
	}
	if isNotFound(err) {
		return nil
	}
	return err
}

func (a *Adapter) cancelFiles(ctx context.Context, s batchSpec, ref BatchRef) error {
	_, err := a.batchDo(ctx, "POST", batchPath(s.base, ref.ID)+"/cancel", a.headers(), nil, "")
	return err
}

func (a *Adapter) cleanupFiles(ctx context.Context, s batchSpec, ref BatchRef) error {
	ids := []string{ref.InputFile}
	if _, b, err := a.pollFiles(ctx, s, ref); err == nil {
		ids = append(ids, b.OutputFileID, b.ErrorFileID)
	}
	var errs []error
	for _, id := range ids {
		if err := a.deleteFile(ctx, s, id); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
