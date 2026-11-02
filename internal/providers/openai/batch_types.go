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
