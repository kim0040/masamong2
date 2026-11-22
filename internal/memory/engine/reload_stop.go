package engine

import "sync/atomic"

// StopReload makes a rebuild in progress stop at its next stage, and one not yet begun not start: the
// owner is shutting down and waits for its work. Searches, resyncs and index passes carry on.
func (e *Engine) StopReload() { e.stopping.Store(true) }

// reloadStageHook runs at each stage check of a rebuild (tests stop one midway). Never set in production.
var reloadStageHook atomic.Pointer[func(e *Engine, stage string)]

// reloadStopped reports whether a rebuild should stop at this stage: a newer engine replaced this one,
// or the owner is shutting down.
func (e *Engine) reloadStopped(stage string) bool {
	if h := reloadStageHook.Load(); h != nil {
		(*h)(e, stage)
	}
	return e.retired.Load() || e.stopping.Load()
}
