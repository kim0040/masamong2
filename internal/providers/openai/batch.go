package openai

import "context"

// Placeholder until the host formats land: every host reports no batch support, so callers run live.

// BatchSupport reports whether this adapter's host can batch the kind (the model may still be refused
// at submit, with ErrBatchUnsupported).
func (a *Adapter) BatchSupport(kind BatchKind) bool { return false }

// BatchMaxItems is the most items one batch of the kind may carry on this host; callers split above it.
func (a *Adapter) BatchMaxItems(kind BatchKind) int { return 0 }

// SubmitBatch sends the items as one batch for model.
func (a *Adapter) SubmitBatch(ctx context.Context, kind BatchKind, model string, items []BatchItem) (BatchRef, error) {
	return BatchRef{}, ErrBatchUnsupported
}

// PollBatch reads a submitted batch's state.
func (a *Adapter) PollBatch(ctx context.Context, ref BatchRef) (BatchStatus, error) {
	return BatchStatus{}, ErrBatchUnsupported
}

// BatchResults reads the outcomes of a batch in a final state, one per item that has one.
func (a *Adapter) BatchResults(ctx context.Context, ref BatchRef) ([]BatchOutcome, error) {
	return nil, ErrBatchUnsupported
}

// CancelBatch asks the host to stop a batch (best effort; finished items are still charged).
func (a *Adapter) CancelBatch(ctx context.Context, ref BatchRef) error { return ErrBatchUnsupported }

// CleanupBatch deletes what the batch left on the host (input and output files, the stored batch) once
// its results are saved. Best effort.
func (a *Adapter) CleanupBatch(ctx context.Context, ref BatchRef) error { return nil }
