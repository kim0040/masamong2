package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"masamong/internal/providers"
	"masamong/internal/providers/openai"
)

// Poll pacing and the bounds of the waits. Variables so tests can shorten them.
var (
	batchPollFirst     = time.Minute
	batchPollMax       = 5 * time.Minute
	batchGiveUp        = 97 * time.Hour // without a host deadline: past the longest one (96 h) the wait ends
	batchDeadlineSlack = time.Hour      // polled past the host's own deadline: the batch is given up
	batchCancelWait    = 2 * time.Minute
	batchCancelPoll    = 10 * time.Second
	batchResultRetry   = 10 * time.Second // the first wait before reading results again
	batchResultGiveUp  = 6 * time.Hour    // results that stay unreadable this long end the run (the plan stays)
	batchCleanupWait   = time.Minute
	// a submission is looked up in the host's list no sooner than this after its create call, so a batch
	// the host has just stored is listed by then
	batchReconcileSettle = 2 * time.Minute
	// the plan write after an accepted batch is tried this often, waiting from batchSaveRetry (doubling)
	batchSaveRetry = time.Second
	batchSaveTries = 4
)

// ShortBatchWaitsForTest shortens the batch waits (polls, the cancel wait, result retries, the reconcile
// settle time) for a test, also of another package, and returns what restores them. Tests only: call it
// before anything that waits starts, and restore after it stopped.
func ShortBatchWaitsForTest() (restore func()) {
	first, mx, cw, cp, rr, rs, sr := batchPollFirst, batchPollMax, batchCancelWait, batchCancelPoll, batchResultRetry, batchReconcileSettle, batchSaveRetry
	batchPollFirst, batchPollMax, batchCancelWait, batchCancelPoll, batchResultRetry = time.Millisecond, 5*time.Millisecond, 200*time.Millisecond, time.Millisecond, time.Millisecond
	batchReconcileSettle, batchSaveRetry = 0, time.Millisecond
	return func() {
		batchPollFirst, batchPollMax, batchCancelWait, batchCancelPoll, batchResultRetry, batchReconcileSettle, batchSaveRetry = first, mx, cw, cp, rr, rs, sr
	}
}

// ErrBatchSubmitUnknown: a submission's outcome could not be settled (openai.ErrBatchSubmitUnknown). The
// plan stays (the room held) until the host's list settles it or the operator discards it.
var ErrBatchSubmitUnknown = openai.ErrBatchSubmitUnknown

// ErrBatchSubmitRefused: the host refused a batch (a 4xx other than "this model cannot batch", a rate
// limit, the daily gate), a submission turned out never to have reached it, or the summary model's live
// shape probe failed before anything was sent. Nothing of it runs live; what was sent before is collected.
var ErrBatchSubmitRefused = errors.New("memory: the provider refused the batch")

// ErrBatchProviderGone: the provider a batch was submitted to is no longer configured as it was (removed,
// another address or family, no key): its batches cannot be polled or read. The plan stays.
var ErrBatchProviderGone = errors.New("memory: the provider the batch was sent to is no longer configured")

// ErrBatchDraining: the plan is draining (cancelled, or its submission stopped early); it is only
// collected, never finished live.
var ErrBatchDraining = errors.New("memory: the batch plan is draining")

// BatchStatusFunc receives the combined poll state of a plan: the least advanced state of its open
// batches and the requests finished out of all.
type BatchStatusFunc func(state openai.BatchState, done, total int)

// ReconcilePlan settles the plan's submissions that have no answer (a lost answer, or an intent a restart
// found): each is looked up in its host's batch list no sooner than batchReconcileSettle after its call.
// Found, it is a normal batch. Certainly never created, it is dropped, and the plan (whose submission
// was then cut short) drains: nothing of it runs live. When the host cannot tell, the plan is marked
// submit_unknown, the room stays held and ErrBatchSubmitUnknown says what the operator can do. No lock is
// held while waiting.
func (e *Engine) ReconcilePlan(ctx context.Context, plan *BatchPlan) error {
	if !hasOpenSubmit(plan) {
		return e.cutToDraining(plan)
	}
	unknown := ""
	dropped := false
	for i := 0; i < len(plan.Groups); i++ {
		g := &plan.Groups[i]
		if g.Submit == "" {
			continue
		}
		if wait := batchReconcileSettle - time.Since(g.Ref.SubmittedAt); wait > 0 {
			if err := sleepCtx(ctx, wait); err != nil {
				return err
			}
		}
		client, err := e.groupBatcher(g)
		var ref openai.BatchRef
		if err == nil {
			ref, err = client.ReconcileBatch(ctx, g.Ref)
		}
		switch {
		case err == nil && ref.ID != "":
			if g.Submit == submitPending {
				plan.BatchItems += g.Units // counted at the answer, which never came
			}
			key := g.Ref.SubmitKey
			g.Ref, g.Submit, g.State, g.Error = ref, "", openai.BatchPending, ""
			if g.Ref.SubmitKey == "" {
				g.Ref.SubmitKey = key
			}
			slog.Info("memory: a batch whose submission was unanswered was found at the host", "room", plan.Room, "purpose", g.Purpose)
		case errors.Is(err, openai.ErrBatchNotSubmitted):
			if g.Submit == submitUnknown {
				plan.BatchItems -= g.Units
			}
			plan.Groups = append(plan.Groups[:i], plan.Groups[i+1:]...)
			i--
			dropped = true
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			g.Submit = submitUnknown
			if err != nil {
				g.Error = clipText(err.Error(), 200)
			}
			if unknown == "" {
				unknown = fmt.Sprintf("a %s batch sent to %s at %s got no answer, and the host cannot be asked whether it took it, so it may be running and billed",
					g.Purpose, hostName(g.Provider), g.Ref.SubmittedAt.UTC().Format(time.RFC3339))
			}
		}
	}
	if unknown != "" {
		plan.State = BatchPlanSubmitUnknown
		plan.Reason = clipText(unknown+"; check the provider's batch list (its console), then discard the plan (memory.blocks.update discard_batch) to release the room", 400)
		if err := e.saveBatchPlan(plan); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s", ErrBatchSubmitUnknown, plan.Reason)
	}
	if plan.State == BatchPlanSubmitUnknown {
		plan.State, plan.Reason = BatchPlanWaiting, ""
		if plan.Drain {
			plan.State = BatchPlanDraining
		}
	}
	if dropped {
		plan.Cut = true
	}
	return e.cutToDraining(plan)
}

// cutToDraining turns a plan whose submission was cut short into a draining one (collected, nothing live),
// or deletes it when nothing was sent; it returns ErrBatchSubmitRefused then. A plan that was not cut is
// only saved.
func (e *Engine) cutToDraining(plan *BatchPlan) error {
	if !plan.Cut || plan.Drain {
		if len(plan.Groups) == 0 {
			return e.deletePlan(plan, "reconcile")
		}
		return e.saveBatchPlan(plan)
	}
	why := "a batch submission did not reach the host"
	if len(plan.Groups) == 0 {
		if err := e.deletePlan(plan, "reconcile"); err != nil {
			return err
		}
		return fmt.Errorf("%w: %s; nothing is running; prepare again", ErrBatchSubmitRefused, why)
	}
	plan.State, plan.Drain = BatchPlanDraining, true
	plan.Reason = clipText("submission stopped: "+why+"; the batches sent are collected, the rest is not sent; prepare again for it", 300)
	if err := e.saveBatchPlan(plan); err != nil {
		return err
	}
	return fmt.Errorf("%w: %s", ErrBatchSubmitRefused, plan.Reason)
}

func hostName(p BatchProvider) string {
	switch {
	case p.ID != "" && p.BaseURL != "":
		return p.ID + " (" + p.BaseURL + ")"
	case p.ID != "":
		return p.ID
	case p.BaseURL != "":
		return p.BaseURL
	}
	return "the provider"
}

// ---------------------------------------------------------------------------
// wait
// ---------------------------------------------------------------------------

// WaitBatch polls the plan's batches until every one has ended (or was given up past its deadline),
// every minute at first and backing off to five. A submission still unanswered is settled first
// (ReconcilePlan, whose errors it returns). It holds no lock and writes nothing else: the plan is saved
// when its results are applied, and a restart polls again. It returns ctx's error when ctx ends first,
// ErrBatchProviderGone at once when a batch's provider is no longer configured or cannot be polled, and
// ErrBatchPlanGone when the plan was deleted (discarded) or replaced meanwhile: its batches are no
// longer this run's to wait for.
func (e *Engine) WaitBatch(ctx context.Context, plan *BatchPlan, status BatchStatusFunc) error {
	if hasOpenSubmit(plan) {
		if err := e.ReconcilePlan(ctx, plan); err != nil {
			return err
		}
	}
	delay := batchPollFirst
	for {
		if _, err := e.storedCurrent(plan, "wait"); errors.Is(err, ErrBatchPlanGone) {
			return err
		}
		open, err := e.pollPlan(ctx, plan)
		if status != nil {
			status(plan.pollState())
		}
		if err != nil {
			return err
		}
		if !open {
			return nil
		}
		if err := sleepCtx(ctx, delay); err != nil {
			return err
		}
		delay = min(2*delay, batchPollMax)
	}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (g *BatchGroup) deadline(created time.Time) time.Time {
	if !g.ExpiresAt.IsZero() {
		return g.ExpiresAt.Add(batchDeadlineSlack)
	}
	base := g.Ref.SubmittedAt
	if base.IsZero() {
		base = created
	}
	return base.Add(batchGiveUp)
}

// pollPlan polls every open batch once and reports whether any is still open. A group whose provider is
// gone, or that another host's adapter would have to read, ends the round with ErrBatchProviderGone.
func (e *Engine) pollPlan(ctx context.Context, plan *BatchPlan) (bool, error) {
	open := false
	now := e.nowT()
	for i := range plan.Groups {
		g := &plan.Groups[i]
		if g.Applied || g.State.Final() || g.Submit != "" {
			continue
		}
		client, err := e.groupBatcher(g)
		if err != nil {
			return open, err
		}
		st, err := client.PollBatch(ctx, g.Ref)
		if err != nil {
			if ctx.Err() != nil {
				return true, nil
			}
			if permanentClientErr(err) {
				return open, fmt.Errorf("%w (%s batch): %v; restore it, or discard the batch (memory.blocks.update discard_batch)", ErrBatchProviderGone, g.Purpose, err)
			}
			if now.After(g.deadline(plan.CreatedAt)) {
				g.State, g.Error, g.Abandoned = openai.BatchExpired, clipText("poll: "+err.Error(), 200), true
				continue
			}
			open = true // a network or host hiccup: the next round asks again
			continue
		}
		g.State, g.Error, g.done, g.total = st.State, clipText(st.Error, 200), st.Completed+st.Failed, st.Total
		if !st.ExpiresAt.IsZero() {
			g.ExpiresAt = st.ExpiresAt
		}
		if !st.State.Final() {
			if now.After(g.deadline(plan.CreatedAt)) {
				// a host still running a batch long past its own deadline: stop waiting for it
				_ = client.CancelBatch(ctx, g.Ref)
				g.State, g.Error, g.Abandoned = openai.BatchExpired, "still "+string(st.State)+" past the host deadline", true
				continue
			}
			open = true
		}
	}
	return open, nil
}

// permanentClientErr is a client that can never read the batch: another host's reference, no key, or a
// host that has no batch API (any more). Polling it until the deadline would only hold the room.
func permanentClientErr(err error) bool {
	return errors.Is(err, openai.ErrBatchOtherHost) || errors.Is(err, providers.ErrNoCredential) || errors.Is(err, openai.ErrBatchUnsupported)
}

// pollState is the least advanced state of the open batches (pending before running; the final state
// once all ended) and the requests finished out of all.
func (p *BatchPlan) pollState() (openai.BatchState, int, int) {
	state := openai.BatchDone
	done, total := 0, 0
	for _, g := range p.Groups {
		n := max(g.total, len(g.Items))
		total += n
		switch {
		case g.Applied || g.State.Final():
			done += n
		default:
			done += min(g.done, n)
			if g.State == openai.BatchPending || g.State == "" {
				state = openai.BatchPending
			} else if state != openai.BatchPending {
				state = openai.BatchRunning
			}
		}
	}
	return state, done, total
}

// PollState is pollState for callers outside the engine (a job's progress).
func (p *BatchPlan) PollState() (openai.BatchState, int, int) { return p.pollState() }

// settled reports whether every group's results are applied (nothing at a host is still to be read).
func (p *BatchPlan) settled() bool {
	for _, g := range p.Groups {
		if !g.Applied {
			return false
		}
	}
	return true
}

// cleanupPlan deletes what the plan's batches left on the hosts (best effort; a short wait of its own, so
// a cancelled run still tidies up). Only a batch whose results were read (or that has none) is cleaned:
// results nobody read stay at the host.
func (e *Engine) cleanupPlan(ctx context.Context, plan *BatchPlan) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), batchCleanupWait)
	defer cancel()
	for i := range plan.Groups {
		g := &plan.Groups[i]
		if g.Ref.ID == "" || !g.Applied {
			continue
		}
		client, err := e.groupBatcher(g)
		if err != nil {
			slog.Warn("memory: batch cleanup skipped", "room", plan.Room, "purpose", g.Purpose, "err", err)
			continue
		}
		if err := client.CleanupBatch(cctx, g.Ref); err != nil && !errors.Is(err, openai.ErrBatchUnsupported) {
			slog.Warn("memory: batch cleanup failed", "room", plan.Room, "purpose", g.Purpose, "err", err)
		}
	}
}

// CancelPlan stops a plan the operator cancelled. The plan turns draining (the room stays held), the
// hosts are asked to cancel the open batches (best effort; OpenRouter cannot, its batch runs to its end),
// and it waits a little for them to end and applies what finished. When every batch has ended and its
// results are applied, the plan is cleaned up and deleted (done); otherwise it stays draining for
// DrainBatch, which the caller runs in the background. ctx should outlive the cancelled job
// (context.WithoutCancel with a timeout). Nothing runs live.
func (e *Engine) CancelPlan(ctx context.Context, plan *BatchPlan) (BatchApplied, bool, error) {
	var applied BatchApplied
	if err := e.setDraining(plan, "cancelled by the operator; the batches still at the host are collected to their end"); err != nil {
		if errors.Is(err, ErrBatchPlanGone) {
			return applied, true, nil
		}
		return applied, false, err
	}
	for i := range plan.Groups {
		g := &plan.Groups[i]
		if g.Applied || g.State.Final() || g.Ref.ID == "" {
			continue
		}
		client, err := e.groupBatcher(g)
		if err != nil {
			slog.Warn("memory: batch not cancelled", "room", plan.Room, "purpose", g.Purpose, "err", err)
			continue
		}
		if err := client.CancelBatch(ctx, g.Ref); err != nil && !errors.Is(err, openai.ErrBatchUnsupported) {
			slog.Warn("memory: batch cancel failed", "room", plan.Room, "purpose", g.Purpose, "err", err)
		}
	}
	deadline := time.Now().Add(batchCancelWait)
	for time.Now().Before(deadline) {
		open, err := e.pollPlan(ctx, plan)
		if err != nil || !open {
			break
		}
		if sleepCtx(ctx, batchCancelPoll) != nil {
			break
		}
	}
	a, err := e.ApplyBatch(ctx, plan)
	applied.add(a)
	if errors.Is(err, ErrBatchPlanGone) {
		return applied, true, nil
	}
	if err != nil || !plan.settled() {
		return applied, false, err
	}
	err = e.finishDrain(ctx, plan, applied)
	return applied, err == nil, err
}

// setDraining marks the stored plan draining with reason (kept when it already drains).
func (e *Engine) setDraining(plan *BatchPlan, reason string) error {
	e.indexMu.Lock()
	defer e.indexMu.Unlock()
	stored, err := e.storedCurrent(plan, "cancel")
	if err != nil {
		return err
	}
	mergePolled(&stored, plan)
	if stored.State != BatchPlanDraining || !stored.Drain {
		stored.Drain = true
		if stored.State != BatchPlanSubmitUnknown {
			stored.State = BatchPlanDraining
		}
		if stored.Reason == "" || stored.State == BatchPlanDraining {
			stored.Reason = clipText(reason, 300)
		}
		if err := e.saveBatchPlan(&stored); err != nil {
			return err
		}
	}
	*plan = stored
	return nil
}

// DrainBatch collects a draining plan: it settles unanswered submissions, waits for every batch to end at
// its host, applies the results (retrying unreadable ones for a while), cleans up and deletes the plan.
// Nothing runs live. It returns when the plan is gone, or with the reason it could not finish (the plan
// then stays draining, the room held, for the next run or the operator's discard).
func (e *Engine) DrainBatch(ctx context.Context, plan *BatchPlan) (BatchApplied, error) {
	var total BatchApplied
	if hasOpenSubmit(plan) {
		err := e.ReconcilePlan(ctx, plan)
		if errors.Is(err, ErrBatchPlanGone) {
			return total, nil // discarded meanwhile: nothing is stored again
		}
		if err != nil && !errors.Is(err, ErrBatchSubmitRefused) {
			return total, err
		}
		if len(plan.Groups) == 0 {
			return total, nil // nothing reached a host; the plan is gone
		}
	}
	if err := e.WaitBatch(ctx, plan, nil); err != nil {
		if errors.Is(err, ErrBatchPlanGone) {
			return total, nil
		}
		return total, err
	}
	a, err := e.CollectBatch(ctx, plan)
	total.add(a)
	if errors.Is(err, ErrBatchPlanGone) {
		return total, nil
	}
	if err != nil {
		return total, err
	}
	return total, e.finishDrain(ctx, plan, total)
}

// finishDrain cleans up and deletes a settled draining plan, and loads what it applied.
func (e *Engine) finishDrain(ctx context.Context, plan *BatchPlan, applied BatchApplied) error {
	if _, err := e.storedCurrent(plan, "drain"); err != nil {
		if errors.Is(err, ErrBatchPlanGone) {
			return nil
		}
		return err
	}
	e.cleanupPlan(ctx, plan)
	if err := e.deletePlan(plan, "drain"); err != nil {
		return err
	}
	if applied.Saved > 0 || plan.LeafVectors+plan.Summaries+plan.MessageVectors > 0 {
		if _, err := e.Reload(); err != nil && !errors.Is(err, errRetired) {
			return err
		}
	}
	return nil
}

// DiscardBatch drops the room's plan without applying it (the operator's discard, or the room is being
// deleted): the open batches are cancelled and every submitted batch is cleaned up, best effort, and the
// plan is deleted. A submission whose outcome is unknown cannot be reached: the operator checks the
// provider's console for it.
func (e *Engine) DiscardBatch(ctx context.Context, room string) error {
	plan, ok, err := e.LoadBatchPlan(room)
	if err != nil || !ok {
		return err
	}
	for i := range plan.Groups {
		g := &plan.Groups[i]
		if g.Ref.ID == "" {
			continue
		}
		client, err := e.groupBatcher(g)
		if err != nil {
			slog.Warn("memory: a discarded batch could not be reached", "room", room, "purpose", g.Purpose, "err", err)
			continue
		}
		if !g.Applied && !g.State.Final() {
			_ = client.CancelBatch(ctx, g.Ref)
		}
		g.Applied = true // its results are not wanted: cleanupPlan may remove them
	}
	e.cleanupPlan(ctx, &plan)
	return e.deletePlan(&plan, "discard")
}
