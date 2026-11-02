package openai

import (
	"errors"
	"time"
)

// A batch is many independent requests sent to a host as one job that finishes within about a day, at
// about half the live price (memory preparation in batch mode). Each host family has its own wire format
// (batch.go). A host without a batch API, or a model the host cannot batch, reports
// ErrBatchUnsupported, and the caller runs the same requests live.

// BatchKind is what every item of one batch asks for.
type BatchKind string

const (
	BatchChat  BatchKind = "chat"  // one chat completion per item
	BatchEmbed BatchKind = "embed" // one embeddings request (several inputs) per item
)

// BatchItem is one request of a batch. ID is unique within the batch and is sent as the provider's
// custom_id: 1–64 characters of [A-Za-z0-9_-].
type BatchItem struct {
	ID    string
	Chat  *ChatRequest // BatchChat: Model equals the batch's model; no Tools, no images
	Input []string     // BatchEmbed: the inputs of one embeddings request
}

// BatchRef names a submitted batch. Callers persist it as JSON and keep polling it after a restart; only
// an adapter for the same host (BaseURL) can read it.
type BatchRef struct {
	API         API       `json:"api"`
	BaseURL     string    `json:"base_url"`
	ID          string    `json:"id"`
	Kind        BatchKind `json:"kind"`
	Model       string    `json:"model"`
	Count       int       `json:"count"`
	InputFile   string    `json:"input_file,omitempty"`  // an uploaded input file to delete afterwards
	ResultsURL  string    `json:"results_url,omitempty"` // where a host that names one puts the results
	SubmittedAt time.Time `json:"submitted_at"`
	// SubmitKey is the caller's stable key for this submission. A host that keeps metadata gets it with
	// the batch, so a submission whose answer was lost can be found again (ReconcileBatch).
	SubmitKey string `json:"submit_key,omitempty"`
}

// BatchState is a batch's lifecycle, the same words for every host.
type BatchState string

const (
	BatchPending   BatchState = "pending"   // accepted, being validated or queued
	BatchRunning   BatchState = "running"   // in progress or finalizing
	BatchDone      BatchState = "done"      // ended; results are readable (single items may still have failed)
	BatchFailed    BatchState = "failed"    // the whole batch was rejected (Error says why); no results
	BatchExpired   BatchState = "expired"   // the host gave up at its deadline; finished items may be readable
	BatchCancelled BatchState = "cancelled" // cancelled; finished items may be readable
)

// Final reports whether the state can no longer change.
func (s BatchState) Final() bool {
	switch s {
	case BatchDone, BatchFailed, BatchExpired, BatchCancelled:
		return true
	}
	return false
}

// BatchStatus is one poll's answer.
type BatchStatus struct {
	State     BatchState
	Total     int
	Completed int
	Failed    int
	Error     string    // why the batch failed, when the host says
	ExpiresAt time.Time // the host's deadline; zero when unknown
}

// BatchOutcome is one item's result: exactly one of Chat, Embed and Err is set. Costs are the
// provider-reported ones, or estimated at the host's batch price.
type BatchOutcome struct {
	ID    string
	Chat  *ChatResult
	Embed *EmbedResult
	Err   error
}

// ErrBatchUnsupported means the host, or the model on it, has no batch API for the kind: run the
// items live instead.
var ErrBatchUnsupported = errors.New("openai: batch not supported for this host or model")

// ErrBatchOtherHost means a batch reference names another host than the adapter's: only an adapter for
// the host it was submitted to can read it.
var ErrBatchOtherHost = errors.New("openai: this batch was submitted to another host")

// ErrBatchNotFinal means results were asked of a batch that is still running.
var ErrBatchNotFinal = errors.New("openai: batch is not finished yet")

// ErrBatchSubmitUnknown means a batch create call ended without a clear answer (a timeout, a connection
// lost after the request went out, a server error, an answer without an id): the host may have accepted
// and be running the batch, and charge for it. It is never a reason to run the items live. The error is a
// *SubmitUnknownError, which carries what is known of the submission for ReconcileBatch.
var ErrBatchSubmitUnknown = errors.New("openai: batch submission outcome unknown")

// ErrBatchNotSubmitted is ReconcileBatch's definite answer that the host has no batch for the
// submission: nothing of it runs or is billed there.
var ErrBatchNotSubmitted = errors.New("openai: the host has no batch for this submission")

// SubmitUnknownError is a submission whose outcome is unknown (ErrBatchSubmitUnknown). Ref names what is
// known of it: the host, kind, model, count, key, submit time and the uploaded input file, without an id.
type SubmitUnknownError struct {
	Ref BatchRef
	Err error
}

func (e *SubmitUnknownError) Error() string {
	return "openai: batch submission outcome unknown (the host may be running it): " + e.Err.Error()
}

func (e *SubmitUnknownError) Unwrap() []error { return []error{ErrBatchSubmitUnknown, e.Err} }
