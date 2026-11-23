package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// The idle poll of the pending-event worker backs off from min to max; an enqueue wakes it at once, so
// the poll is only the safety net for an event file a crash left or a failed replay. A test shrinks the
// intervals of its own service (serviceHooks).
const (
	pendingEventPollMin = time.Second
	pendingEventPollMax = 10 * time.Second
)

// oldEditsDelay keeps the pass over old edits (settleOldEdits) out of the busy minute after a start; a
// test skips it (serviceHooks.oldEditsAtOnce).
const oldEditsDelay = 30 * time.Second

// nextPendingEventPoll is the wait after a pass: lo with work pending, else twice the last wait, up to hi.
func nextPendingEventPoll(current time.Duration, pending bool, lo, hi time.Duration) time.Duration {
	if pending || current <= 0 {
		return lo
	}
	return min(current*2, hi)
}

func (s *Service) startEventWorker() {
	if !s.eventWorker.CompareAndSwap(false, true) {
		return
	}
	s.ownedGo("pending memory events", func(ctx context.Context) {
		defer s.eventWorker.Store(false)
		lo := durationOr(s.hooks.pendingPollMin, pendingEventPollMin)
		hi := durationOr(s.hooks.pendingPollMax, pendingEventPollMax)
		wait := lo
		wake := s.Store.EventWake()
		var lastWarn string
		started, oldEditsDone := time.Now(), false
		for {
			if s.hooks.pendingStep != nil {
				s.hooks.pendingStep()
			}
			// retryEvents replays everything in both directories; when it returns without an error nothing
			// is left to wait for (an event enqueued meanwhile has already signalled wake), so the
			// directories are not listed a second time just to find that out
			more, err := s.retryEvents(ctx)
			pending := more || err != nil
			if err != nil && ctx.Err() == nil {
				s.Log.Warn("memory: pending events retained for retry", "err", err)
			}
			var one int
			switch err := s.Store.Reader().QueryRowContext(ctx, `SELECT 1 FROM redaction_jobs LIMIT 1`).Scan(&one); {
			case err == nil:
				pending = true
				s.kickRedaction()
			case errors.Is(err, sql.ErrNoRows):
				if oldEditsDone || (!s.hooks.oldEditsAtOnce && time.Since(started) < oldEditsDelay) {
					break
				}
				// the queue is empty: edits finished by an earlier release, a batch at a time
				done, err := s.settleOldEdits(ctx)
				oldEditsDone = done && err == nil
				pending = pending || !done
				if err != nil && ctx.Err() == nil {
					if msg := err.Error(); msg != lastWarn {
						lastWarn = msg
						s.Log.Warn("memory: settling old edits failed; it is retried", "err", err)
					}
				}
			case strings.Contains(err.Error(), "no such table"): // TiDB has no redaction queue
			case ctx.Err() == nil:
				if msg := err.Error(); msg != lastWarn {
					lastWarn = msg // once per distinct failure, not once a second
					s.Log.Warn("memory: redaction queue check failed", "err", err)
				}
				pending = true
			}
			next := nextPendingEventPoll(wait, pending, lo, hi)
			timer := time.NewTimer(wait)
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-wake:
				timer.Stop()
				wait = lo
			case <-timer.C:
				wait = next
			}
		}
	})
}

// pendingEventBatch is how many files Store.PendingEvents returns per call; a full list means more may wait.
const pendingEventBatch = 200

// retryEvents replays the pending capture and source-change events. more reports that a directory
// held a full batch, so the worker should come back soon instead of backing off.
func (s *Service) retryEvents(ctx context.Context) (more bool, err error) {
	captures, err := s.Store.PendingEvents("capture")
	if err != nil {
		return false, err
	}
	more = len(captures) >= pendingEventBatch
	if len(captures) > 0 {
		eng, err := s.MemoryEngine()
		if err != nil {
			return more, err
		}
		if err := eng.RetryCaptures(ctx); err != nil {
			return more, err
		}
	}
	events, err := s.Store.PendingEvents("source-change")
	if err != nil {
		return more, err
	}
	more = more || len(events) >= pendingEventBatch
	for _, event := range events {
		var j redactionJob
		if err := json.Unmarshal(event.Payload, &j); err != nil {
			return more, err
		}
		at, err := time.Parse(time.RFC3339Nano, j.At)
		if err != nil {
			return more, err
		}
		if err := s.Store.ApplyEvent(ctx, "source-change", event.ID, func() error {
			return s.commitSourceChange(ctx, j.Guild, j.Channel, j.Messages, j.Operation, j.Content, at)
		}); err != nil {
			return more, err
		}
	}
	return more, nil
}
