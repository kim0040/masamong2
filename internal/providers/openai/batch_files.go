package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"mime/multipart"
	"net/url"
	"strconv"
	"strings"
	"time"

	"masamong/internal/providers/decode"
)

// File-backed batches: OpenAI, NanoGPT (same format on its own batch host) and Gemini's OpenAI-compatible
// batch API. The input is a JSONL file of {"custom_id","method","url","body"} rows, the output the same
// with {"custom_id","response","error"} rows.
//
// Gemini keeps the batch calls on its compatible path but not the files: they go through the native Files
// API (uploadGemini, downloadFile, deleteFile). If the compatible route proves unworkable, the other way
// is native inline: POST /v1beta/models/{model}:batchGenerateContent, a style of its own beside these.

// geminiBatchMIME is the type the input file is uploaded as (the batch docs' upload sends
// application/jsonl; the SDKs take the bare "jsonl").
const geminiBatchMIME = "application/jsonl"

// orphanDeleteWait bounds the removal of an input file no batch was created from.
const orphanDeleteWait = 30 * time.Second

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

func (a *Adapter) submitFiles(ctx context.Context, s batchSpec, kind BatchKind, model string, items []BatchItem, key string) (id, inputFile string, err error) {
	data, err := a.batchJSONL(kind, model, items)
	if err != nil {
		return "", "", err
	}
	// the upload creates no batch and costs nothing: a retry that duplicates it leaves a file at worst
	if s.Style == styleGeminiFiles {
		inputFile, err = a.uploadGemini(ctx, s, data)
	} else {
		inputFile, err = a.uploadOpenAI(ctx, s, data)
	}
	if err != nil {
		return "", "", err
	}
	create := map[string]any{"input_file_id": inputFile, "endpoint": batchEndpoint(kind), "completion_window": "24h"}
	if s.Reconcile == reconcileMetadata && key != "" {
		create["metadata"] = map[string]string{submitKeyField: key}
	}
	body, _ := json.Marshal(create)
	id, _, err = a.createBatch(ctx, s.base+"/batches", a.headers(), body, batchIDOf)
	if err == nil {
		return id, inputFile, nil
	}
	var su *SubmitUnknownError
	if errors.As(err, &su) {
		// the host may have created the batch from the file: it stays, and names the batch in the list
		return "", inputFile, &SubmitUnknownError{Ref: BatchRef{InputFile: inputFile}, Err: su.Err}
	}
	// the batch was certainly not created: do not leave the uploaded file (the chat text) behind, also
	// when the caller's context has ended
	dctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), orphanDeleteWait)
	defer cancel()
	_ = a.deleteFile(dctx, s, inputFile)
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

// uploadGemini sends the input file to the native Files API the way its docs do
// (https://ai.google.dev/gemini-api/docs/batch-api, checked 2026-11-02): a resumable upload, started
// with the file's metadata, whose answer names the upload address, then the bytes in one
// "upload, finalize" request. It returns the file's name ("files/abc"), the batch's input_file_id. The
// upload address must be on the API's own origin: the file is the room's chat text.
func (a *Adapter) uploadGemini(ctx context.Context, s batchSpec, data []byte) (string, error) {
	meta, _ := json.Marshal(map[string]any{"file": map[string]string{"display_name": geminiUploadName}})
	h := a.geminiHeaders()
	h["X-Goog-Upload-Protocol"] = "resumable"
	h["X-Goog-Upload-Command"] = "start"
	h["X-Goog-Upload-Header-Content-Length"] = strconv.Itoa(len(data))
	h["X-Goog-Upload-Header-Content-Type"] = geminiBatchMIME
	resp, err := a.batchDo(ctx, "POST", s.native+"/upload/v1beta/files", h, meta, "application/json")
	if err != nil {
		return "", err
	}
	upload := ""
	if resp.Header != nil {
		upload = resp.Header.Get("X-Goog-Upload-URL")
	}
	if upload == "" || !sameOrigin(upload, s.native) {
		return "", fmt.Errorf("%w: upload address missing or on another host", decode.ErrUnexpected)
	}
	h = a.geminiHeaders()
	h["X-Goog-Upload-Offset"] = "0"
	h["X-Goog-Upload-Command"] = "upload, finalize"
	resp, err = a.batchDo(ctx, "POST", upload, h, data, geminiBatchMIME)
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
	return a.rowOutcomes(ref, rows, s.discountFor(ref.Model))
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
