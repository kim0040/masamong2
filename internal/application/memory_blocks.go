package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/crash"
	"masamong/internal/errcode"
	"masamong/internal/jobs"
	"masamong/internal/memory/blocks"
	"masamong/internal/memory/engine"
	"masamong/internal/memory/store"
	"masamong/internal/observability"
	"masamong/internal/providers/openai"
)

// A memory block is one source room: an imported KakaoTalk chat or one Discord channel's captured
// history (contracts/memory/blocks.json). The console manages blocks as the unit people think in:
// import a file → it becomes a block → prepare it for RAG (a background job with an estimate, budget,
// progress and cancel) → rename or annotate it → connect it to any number of channels.

// blockMeta is what the operator says about a block beyond its label (which lives on the room row).
type blockMeta struct {
	Note string `json:"note,omitempty"`
	// AutoUpdate indexes new captured messages in the background (Discord rooms; default on).
	AutoUpdate *bool  `json:"auto_update,omitempty"`
	BudgetUSD  string `json:"budget_usd,omitempty"`
	// Mode is the block's default prepare mode ("" or live, batch), the way BudgetUSD is its default budget.
	Mode string `json:"mode,omitempty"`
}

func (m blockMeta) autoUpdate() bool { return m.AutoUpdate == nil || *m.AutoUpdate }

func blockMetaKey(room string) string { return "block:" + room }

func (s *Service) blockMeta(room string) blockMeta {
	var m blockMeta
	if s.Store != nil {
		_, _ = s.kvGet(blockMetaKey(room), &m)
	}
	return m
}

const (
	jobPrepare        = "memory.prepare"
	maxBlockLabel     = 80
	maxBlockNote      = 500
	jobProgressPeriod = 700 * time.Millisecond
	// a run renews its lease every quarter of it, so a prepare longer than the lease still records its end
	jobLeaseTTL    = 24 * time.Hour
	jobWatchPeriod = time.Second

	// prepare modes (memory.blocks.prepare mode): live calls, or provider batch jobs at about half the
	// price with results within about a day (engine/batch.go)
	prepareLive  = "live"
	prepareBatch = "batch"
	// a cancelled batch preparation asks the hosts to stop and applies what finished within this time
	batchCancelTimeout = 3 * time.Minute
	batchModeNote      = "batch mode: the provider returns the results within about 24 hours (at most its own deadline) at about half the price; the job waits for them, then anything the host could not batch runs live"
)

func validPrepareMode(m string) bool { return m == prepareLive || m == prepareBatch }

func (s *Service) memoryBlocks(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	switch req.Action {
	case "memory.blocks.list":
		// message_vectors_estimate=true adds what turning memory.message_vectors on would cost (the
		// console's confirmation); the pending count is part of the section while the setting is on.
		// With the setting off and no estimate asked for, the section is absent and nothing extra runs.
		lap := s.laps("memory.blocks.list")
		defer lap.done()
		mem := s.memoryStatusFor(ctx, true, boolVal(in, "message_vectors_estimate"))
		lap.mark("memory_status")
		blocks := s.mergedBlocks()
		lap.mark("blocks")
		channels := s.channelDirectory()
		lap.mark("channels")
		return action.OK(req, map[string]any{
			"blocks":   blocks,
			"memory":   mem,
			"channels": channels,
			"note":     "one block is one source room; attaching a block to a channel never copies or re-embeds it",
		})
	case "memory.blocks.show":
		id := strVal(in, "block_id", strVal(in, "id", ""))
		room := strVal(in, "source_room", "")
		if id == "" && room == "" {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "block_id or source_room required", nil)
		}
		if id != "" {
			if b, ok := s.lookupBlock(id); ok {
				room = b.SourceRoom
			}
		}
		for _, b := range s.mergedBlocks() {
			if (id != "" && b["block_id"] == id) || (room != "" && b["source_room"] == room) {
				return action.OK(req, map[string]any{"block": b})
			}
		}
		return action.Fail(req, errcode.InvalidInput, "generic.error", "block not found", nil)
	case "memory.blocks.prepare":
		return s.prepareMemoryBlock(ctx, req, in)
	case "memory.blocks.update":
		return s.updateMemoryBlock(req, in)
	case "memory.blocks.delete":
		return s.deleteMemoryBlock(req, in)
	}
	return action.Fail(req, errcode.ActionUnknown, "action.unknown", s.msg(req, "action.unknown"), nil)
}

// ---------------------------------------------------------------------------
// prepare: estimate, run now (CLI) or as a background job (console)
// ---------------------------------------------------------------------------

func (s *Service) resolveBlockRoom(in map[string]any) (blocks.Block, bool) {
	if id := strVal(in, "block_id", strVal(in, "id", "")); id != "" {
		return s.lookupBlock(id)
	}
	if source := strVal(in, "source_room", strVal(in, "source", "")); source != "" {
		return s.blockBySource(source)
	}
	return blocks.Block{}, false
}

type preparePayload struct {
	SourceRoom string  `json:"source_room"`
	BudgetUSD  float64 `json:"budget_usd"`
	Limit      int     `json:"limit,omitempty"`
	Mode       string  `json:"mode,omitempty"` // "" = live
}

func (p preparePayload) mode() string {
	if p.Mode == "" {
		return prepareLive
	}
	return p.Mode
}

// batchPlanOf is the room's stored batch plan, if it has one (a waiting or applied, unfinished batch).
func batchPlanOf(eng *engine.Engine, room string) (engine.BatchPlanInfo, bool) {
	plans, err := eng.BatchPlans()
	if err != nil {
		return engine.BatchPlanInfo{}, false
	}
	for _, p := range plans {
		if p.Room == room {
			return p, true
		}
	}
	return engine.BatchPlanInfo{}, false
}

func (s *Service) prepareMemoryBlock(ctx context.Context, req action.Request, in map[string]any) action.Result {
	eng, err := s.MemoryEngine()
	if err != nil {
		return action.Fail(req, errcode.NotSetup, "generic.error", err.Error(), nil)
	}
	candidate, ok := s.resolveBlockRoom(in)
	if !ok || candidate.SourceRoom == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "block_id or known source_room required", nil)
	}
	if n := s.roomMessages(eng, candidate.SourceRoom); n == 0 {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "source has no imported messages", map[string]any{"source_room": candidate.SourceRoom})
	}
	meta := s.blockMeta(candidate.SourceRoom)
	budgetDefault := s.Doc().Memory.IndexBudgetUSD
	if meta.BudgetUSD != "" {
		budgetDefault = meta.BudgetUSD
	}
	budget := parseUSD(strVal(in, "budget_usd", budgetDefault), 1.0)
	if budget > 1000 {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "budget_usd must be at most 1000", nil)
	}
	limit := intVal(in, "limit", 0)
	if limit < 0 {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "limit must be 0 (all) or more", nil)
	}
	mode := strings.TrimSpace(strVal(in, "mode", meta.Mode))
	if mode == "" {
		mode = prepareLive
	}
	if !validPrepareMode(mode) {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "mode must be live or batch", nil)
	}
	if boolVal(in, "dry_run") {
		report, err := eng.IndexRooms(ctx, engine.IndexOptions{Rooms: []string{candidate.SourceRoom}, DryRun: true, Limit: limit}, nil)
		if err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
		}
		if mode == prepareBatch && report.Estimate != nil {
			est := eng.BatchEstimate(*report.Estimate)
			report.Estimate, report.Mode = &est, prepareBatch
		}
		prepared := candidate
		prepared.ID = blocks.BlockID(candidate.Collection, candidate.SourceRoom, eng.Settings.Recipe.Version(), eng.EmbedSpace())
		out := map[string]any{"dry_run": true, "would_prepare": true, "block": prepared, "report": report,
			"budget_usd": budget, "cost_usd": 0, "llm_configured": eng.LLM != nil && eng.LLM.Configured(), "mode": mode}
		if mode == prepareBatch {
			out["note"] = batchModeNote
		}
		return action.OK(req, out)
	}
	// a batch already in flight for the room is resumed, never paid again live: the job waits for it
	resumed := false
	if _, ok := batchPlanOf(eng, candidate.SourceRoom); ok && mode == prepareLive {
		mode, resumed = prepareBatch, true
	}
	if eng.LLM == nil || !eng.LLM.Configured() {
		return action.Fail(req, errcode.ProviderUnconfigured, "provider.unconfigured", s.msg(req, "provider.unconfigured", "provider", s.Doc().Models["embed"].Provider),
			map[string]any{"source_room": candidate.SourceRoom})
	}
	payload := preparePayload{SourceRoom: candidate.SourceRoom, BudgetUSD: budget, Limit: limit, Mode: mode}
	// a batch waits up to a day for its results: it always runs as a background job
	if boolVal(in, "background") || mode == prepareBatch {
		j, reused, err := s.startPrepareJob(payload)
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		var running preparePayload
		if json.Unmarshal([]byte(j.Payload), &running) == nil && running.SourceRoom != "" {
			mode = running.mode() // a job already running keeps its own mode
		}
		out := map[string]any{"job": jobJSON(j), "already_running": reused, "source_room": candidate.SourceRoom, "mode": mode, "background": true}
		if mode == prepareBatch {
			out["note"] = batchModeNote
		}
		if resumed {
			out["batch_resumed"] = true
		}
		res := action.OK(req, out)
		res.JobID = j.ID
		return res
	}
	prepared, report, err := s.prepareRoom(ctx, candidate, payload, nil, nil)
	if err != nil {
		code := errcode.InvalidInput
		if errors.Is(err, errProviderMissing) {
			code = errcode.ProviderUnconfigured
		}
		return action.Fail(req, code, "generic.error", err.Error(), map[string]any{"block": prepared, "report": report})
	}
	return action.OK(req, map[string]any{"dry_run": false, "block": prepared, "report": report})
}

var errProviderMissing = errors.New("provider_not_configured")

func (s *Service) roomMessages(eng *engine.Engine, room string) int {
	rooms, err := eng.Store.Rooms()
	if err != nil {
		return 0
	}
	for _, r := range rooms {
		if r.Room.Room == room {
			return r.Messages
		}
	}
	return 0
}

// prepareRoom indexes one room and records the outcome in the prepared-block catalog.
// owned, for a background job, says whether this run still owns its job; a run that lost it (retried and
// taken by a newer run) writes no block status, so it cannot overwrite the newer run's. It is asked
// right before the write, not left to the cancel watcher's one-second tick.
func (s *Service) prepareRoom(ctx context.Context, candidate blocks.Block, p preparePayload, progress engine.Progress, owned func() bool) (blocks.Block, engine.IndexReport, error) {
	eng, prepared, err := s.beginPrepare(candidate)
	if err != nil {
		return prepared, engine.IndexReport{}, err
	}
	report, runErr := eng.IndexRooms(ctx, engine.IndexOptions{Rooms: []string{prepared.SourceRoom}, BudgetUSD: p.BudgetUSD, Limit: p.Limit}, progress)
	return s.endPrepare(ctx, eng, prepared, report, runErr, owned)
}

// prepareRoomBatch is prepareRoom in batch mode: the same block bookkeeping around runBatchPrepare.
func (s *Service) prepareRoomBatch(ctx context.Context, candidate blocks.Block, p preparePayload, progress engine.Progress, status engine.BatchStatusFunc, owned func() bool) (blocks.Block, engine.IndexReport, error) {
	eng, prepared, err := s.beginPrepare(candidate)
	if err != nil {
		return prepared, engine.IndexReport{}, err
	}
	report, runErr := s.runBatchPrepare(ctx, eng, p, progress, status)
	if current, err := s.MemoryEngine(); err == nil {
		eng = current // the batch may have outlasted the engine it started on
	}
	return s.endPrepare(ctx, eng, prepared, report, runErr, owned)
}

// runBatchPrepare submits the room's work as provider batches (or resumes the stored plan), waits for
// the results without holding the index, applies them and runs the live remainder. A run stopped by
// the daemon or taken by a newer run leaves the plan for the next one; a cancel from the console stops
// the batches, keeps what finished and leaves the rest to drain in the background. Only work the host
// cannot batch (ErrBatchUnsupported) runs live: a refused or unsettled submission, a provider that is
// gone or results that cannot be read end the job with the reason, and run nothing at full price.
func (s *Service) runBatchPrepare(ctx context.Context, eng *engine.Engine, p preparePayload, progress engine.Progress, status engine.BatchStatusFunc) (engine.IndexReport, error) {
	// a plan still draining (an earlier cancel or a stopped submission) is collected before a new one
	if err := s.awaitDrain(ctx, p.SourceRoom, status); err != nil {
		return engine.IndexReport{Mode: prepareBatch}, err
	}
	if current, err := s.MemoryEngine(); err == nil {
		eng = current
	}
	plan, rep, err := eng.PrepareBatch(ctx, p.SourceRoom, engine.BatchOptions{BudgetUSD: p.BudgetUSD, Limit: p.Limit}, progress)
	if err != nil {
		if len(plan.Groups) > 0 && plan.Drain {
			s.goDrain(p.SourceRoom) // what was sent before the refusal is collected
		}
		return rep, err
	}
	if len(plan.Groups) == 0 {
		if rep.StoppedBy != "" || len(rep.FailedRooms) > 0 {
			return rep, nil
		}
		// nothing went to a batch (no paid work, no host can batch it, or the summary probe took the only
		// summary): the pass runs live now, within the budget less what the preparation already spent
		live, err := eng.RunUnbatched(ctx, p.SourceRoom, engine.BatchOptions{BudgetUSD: p.BudgetUSD, Limit: p.Limit}, plan, progress)
		live.FromCache += rep.FromCache
		live.Adapted += rep.Adapted
		return live, err
	}
	if plan.Drain {
		// a plan that drains (resumed after a refusal) is only collected
		s.goDrain(p.SourceRoom)
		return rep, fmt.Errorf("%w: %s", engine.ErrBatchDraining, plan.Reason)
	}
	if err := eng.WaitBatch(ctx, &plan, status); err != nil {
		if ctx.Err() != nil {
			return s.stopBatch(ctx, eng, &plan, rep, err)
		}
		if plan.Drain && len(plan.Groups) > 0 {
			s.goDrain(p.SourceRoom)
		}
		return rep, err // the plan stays stored (unless nothing reached a host); the error says what to do
	}
	var final engine.IndexReport
	for attempt := 0; ; attempt++ {
		// a config change during the wait made a new engine: the results go to the current one
		if current, err := s.MemoryEngine(); err == nil {
			eng = current
		}
		if _, err = eng.CollectBatch(ctx, &plan); err == nil {
			final, err = eng.FinishBatch(ctx, &plan, progress)
		}
		if errors.Is(err, engine.ErrRetired) && attempt < 2 {
			continue
		}
		break
	}
	if err != nil {
		if ctx.Err() != nil {
			return s.stopBatch(ctx, eng, &plan, rep, err)
		}
		return rep, err // the plan stays stored; a retry or the next start finishes it
	}
	final.FromCache += rep.FromCache
	final.Adapted += rep.Adapted
	return final, nil
}

// stopBatch ends a batch preparation whose context ended. Only the operator's cancel stops the
// batches; the daemon stopping or a newer run taking the job leaves the plan to be resumed. A cancelled
// plan drains: the hosts are asked to stop, what finished is applied, and a batch that keeps running
// (OpenRouter cannot cancel) is collected in the background while the room stays held; nothing runs live.
func (s *Service) stopBatch(ctx context.Context, eng *engine.Engine, plan *engine.BatchPlan, rep engine.IndexReport, err error) (engine.IndexReport, error) {
	if ctx.Err() == nil || (s.jobCtx != nil && s.jobCtx.Err() != nil) || errors.Is(context.Cause(ctx), errJobTaken) {
		return rep, err
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), batchCancelTimeout)
	defer cancel()
	if current, e := s.MemoryEngine(); e == nil {
		eng = current
	}
	_, done, cerr := eng.CancelPlan(cctx, plan)
	if cerr != nil {
		s.Log.Warn("memory: cancelling a batch left work behind", "room", plan.Room, "err", cerr)
	}
	if !done {
		s.goDrain(plan.Room)
	}
	rep.StoppedBy = "cancelled"
	rep.LeafVectors, rep.Summaries, rep.MessageVectors = plan.LeafVectors, plan.Summaries, plan.MessageVectors
	rep.SpentUSD, rep.BatchItems, rep.BatchFailed, rep.LiveItems = plan.SpentUSD, plan.BatchItems, plan.BatchFailed, plan.LiveItems
	return rep, nil
}

// batchDrain is a room's background drain: the generation of the plan it collects, what stops it, and a
// channel closed when it has ended.
type batchDrain struct {
	gen    string
	cancel context.CancelFunc
	done   chan struct{}
}

// batchDrains are the background drains (*batchDrain), by service and room: at most one per room.
var batchDrains sync.Map

func drainKey(s *Service, room string) string { return fmt.Sprintf("%p|%s", s, room) }

// goDrain collects the room's stored draining plan in the background (engine.DrainBatch): its batches
// run to their end at the hosts, their results are applied and the plan is deleted. One drain per room
// and plan generation: a drain still registered for a plan that is gone (discarded) is stopped. It stops
// with the service and resumes at the next start (ResumeJobs), and when the plan is discarded
// (retireDrain).
func (s *Service) goDrain(room string) {
	eng, err := s.MemoryEngine()
	if err != nil {
		return
	}
	plan, ok, err := eng.LoadBatchPlan(room)
	if err != nil || !ok || !plan.Drain {
		return // only a stored draining plan is collected
	}
	ctx, workDone, ok := s.beginWork(s.jobCtx)
	if !ok {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	key := drainKey(s, room)
	d := &batchDrain{gen: plan.Generation, cancel: cancel, done: make(chan struct{})}
	for {
		v, loaded := batchDrains.LoadOrStore(key, d)
		if !loaded {
			break
		}
		old := v.(*batchDrain)
		if old.gen == d.gen {
			cancel() // already being collected
			workDone()
			return
		}
		old.cancel() // a drain of a plan that is gone
		batchDrains.CompareAndDelete(key, old)
	}
	log := s.Log
	go func() {
		defer workDone()
		defer func() { batchDrains.CompareAndDelete(key, d); cancel(); close(d.done) }()
		defer crash.Recover(log, "batch drain "+room)
		for attempt := 0; attempt < 3; attempt++ {
			eng, err := s.MemoryEngine()
			if err != nil {
				return
			}
			plan, ok, err := eng.LoadBatchPlan(room)
			if err != nil || !ok || plan.Generation != d.gen {
				return // discarded or replaced meanwhile
			}
			_, err = eng.DrainBatch(ctx, &plan)
			if errors.Is(err, engine.ErrRetired) {
				continue // a config change: the current engine collects it
			}
			if err != nil && ctx.Err() == nil && !errors.Is(err, engine.ErrBatchPlanGone) {
				log.Warn("memory: a draining batch was not collected; the room stays held", "room", room, "err", err)
			}
			if err == nil && ctx.Err() == nil {
				s.emit("memory", "", map[string]any{"source_room": room, "batch_drained": true})
			}
			return
		}
	}()
}

// retireDrain stops the room's background drain of generation gen (the plan being discarded) and waits
// for it to end, at most until ctx ends.
func (s *Service) retireDrain(ctx context.Context, room, gen string) {
	v, ok := batchDrains.Load(drainKey(s, room))
	if !ok {
		return
	}
	d := v.(*batchDrain)
	if d.gen != gen {
		return
	}
	d.cancel()
	select {
	case <-d.done:
	case <-ctx.Done():
	}
}

// discardBatch drops the room's plan (memory.blocks.update discard_batch, a deleted block): its background
// drain is stopped first, then the plan is discarded (engine.DiscardBatch).
func (s *Service) discardBatch(ctx context.Context, eng *engine.Engine, room string) error {
	plan, ok, err := eng.LoadBatchPlan(room)
	if err != nil || !ok {
		return err
	}
	s.retireDrain(ctx, room, plan.Generation)
	return eng.DiscardBatch(ctx, room)
}

// awaitDrain waits for the room's draining plan: a background drain if one runs for the stored plan, else
// the drain itself. A drain of a plan that is gone (discarded) is stopped, not waited for. A plan that
// drains is only collected (nothing of it runs live); the caller then starts afresh.
func (s *Service) awaitDrain(ctx context.Context, room string, status engine.BatchStatusFunc) error {
	eng, err := s.MemoryEngine()
	if err != nil {
		return err
	}
	plan, ok, err := eng.LoadBatchPlan(room)
	if err != nil {
		return err
	}
	if v, running := batchDrains.Load(drainKey(s, room)); running {
		d := v.(*batchDrain)
		if !ok || d.gen != plan.Generation {
			d.cancel()
		} else {
			if status != nil {
				status(openai.BatchRunning, 0, 0)
			}
			select {
			case <-d.done:
			case <-ctx.Done():
				return ctx.Err()
			}
			if eng, err = s.MemoryEngine(); err != nil {
				return err
			}
			if plan, ok, err = eng.LoadBatchPlan(room); err != nil {
				return err
			}
		}
	}
	if !ok || !plan.Drain {
		return nil
	}
	_, err = eng.DrainBatch(ctx, &plan)
	return err
}

// beginPrepare marks the block preparing in the catalog, with its current counts and identity.
func (s *Service) beginPrepare(candidate blocks.Block) (*engine.Engine, blocks.Block, error) {
	eng, err := s.MemoryEngine()
	if err != nil {
		return nil, candidate, err
	}
	rooms, err := eng.Store.Rooms()
	if err != nil {
		return nil, candidate, err
	}
	prepared := candidate
	for _, room := range rooms {
		if room.Room.Room == candidate.SourceRoom {
			prepared.MessageCount = room.Messages
			if prepared.Collection == "" {
				prepared.Collection = room.Collection
			}
			if prepared.Platform == "" {
				prepared.Platform = room.Platform
			}
			if room.Label != "" {
				prepared.Label = room.Label
			}
			break
		}
	}
	prepared.ID = blocks.BlockID(prepared.Collection, prepared.SourceRoom, eng.Settings.Recipe.Version(), eng.EmbedSpace())
	prepared.SegmentationVersion = eng.Settings.Recipe.Version()
	prepared.EmbeddingSpace = eng.EmbedSpace()
	prepared.Status, prepared.Error = "preparing", ""
	if err := s.savePreparedBlock(prepared); err != nil {
		return nil, prepared, err
	}
	return eng, prepared, nil
}

// endPrepare records a preparation's outcome in the catalog.
func (s *Service) endPrepare(ctx context.Context, eng *engine.Engine, prepared blocks.Block, report engine.IndexReport, runErr error, owned func() bool) (blocks.Block, engine.IndexReport, error) {
	s.saveIndexRates(eng)
	prepared.PreparedAt = s.Clock.Now().UTC().Format(time.RFC3339)
	prepared.CostUSD = report.SpentUSD
	if owned != nil && !owned() {
		return prepared, report, errJobTaken
	}
	// a cancel during the last batch wave makes the in-flight embedding fail, which the index reports as
	// a provider error; it was the cancel
	if report.StoppedBy == "provider_error" && ctx.Err() != nil && (s.jobCtx == nil || s.jobCtx.Err() == nil) {
		report.StoppedBy = "cancelled"
	}
	if runErr != nil {
		prepared.Status, prepared.Error = "failed", clip(runErr.Error(), 500)
		switch {
		case s.jobCtx != nil && s.jobCtx.Err() != nil:
			prepared.Error = "interrupted"
		case errors.Is(runErr, context.Canceled):
			prepared.Error = "cancelled"
		}
		_ = s.savePreparedBlock(prepared)
		return prepared, report, runErr
	}
	if current, found := s.blockBySource(prepared.SourceRoom); found && current.SegmentationVersion == prepared.SegmentationVersion {
		prepared.MessageCount, prepared.LeafCount, prepared.Status = current.MessageCount, current.LeafCount, current.Status
	}
	switch {
	case report.StoppedBy == "provider_not_configured":
		prepared.Status, prepared.Error = "failed", report.StoppedBy
	case report.StoppedBy != "" && s.jobCtx != nil && s.jobCtx.Err() != nil:
		prepared.Status, prepared.Error = "failed", "interrupted" // the daemon is stopping, as the job records
	case report.StoppedBy != "":
		prepared.Status, prepared.Error = "failed", report.StoppedBy
	case prepared.Status != "ready":
		prepared.Status, prepared.Error = "failed", "index incomplete"
	}
	if err := s.savePreparedBlock(prepared); err != nil {
		return prepared, report, err
	}
	s.emit("memory", prepared.ID, map[string]any{"source_room": prepared.SourceRoom, "status": prepared.Status})
	if prepared.Error != "" {
		if prepared.Error == "provider_not_configured" {
			return prepared, report, errProviderMissing
		}
		return prepared, report, errors.New(prepared.Error)
	}
	return prepared, report, nil
}

// saveIndexRates keeps measured call latencies for the next time estimate.
func (s *Service) saveIndexRates(eng *engine.Engine) {
	if r := eng.Rates(); r.Samples > 0 {
		_ = s.kvSet("index:rates", r)
	}
}

// ---------------------------------------------------------------------------
// background jobs
// ---------------------------------------------------------------------------

func activeJob(j jobs.Job) bool {
	switch j.Status {
	case jobs.Pending, jobs.Leased, jobs.Running:
		return true
	}
	return false
}

// startPrepareJob queues a prepare job for the room, or returns the one already queued or running.
func (s *Service) startPrepareJob(p preparePayload) (jobs.Job, bool, error) {
	s.blocksMu.Lock()
	list, err := listNonFinishedJobs(s.Store)
	if err != nil {
		s.blocksMu.Unlock()
		return jobs.Job{}, false, err
	}
	for _, j := range list {
		var q preparePayload
		if j.Type == jobPrepare && activeJob(j) && json.Unmarshal([]byte(j.Payload), &q) == nil && q.SourceRoom == p.SourceRoom {
			s.blocksMu.Unlock()
			return j, true, nil
		}
	}
	raw, _ := json.Marshal(p)
	j, err := s.Enqueue(jobPrepare, "", string(raw))
	s.blocksMu.Unlock()
	if err != nil {
		return j, false, err
	}
	_, _ = s.SetJobProgress(j.ID, "", jobs.Progress{Phase: "queued", Label: "queued"})
	s.goJob(j.ID)
	return j, false, nil
}

// goJob runs a job in the background; WaitJobs waits for running ones before the store closes.
func (s *Service) goJob(id string) {
	ctx, done, ok := s.beginWork(s.jobCtx)
	if !ok {
		return
	}
	s.jobsWG.Add(1)
	log := s.Log
	go func() {
		defer done()
		defer s.jobsWG.Done()
		defer crash.Recover(log, "job "+id)
		s.runJob(ctx, id)
	}()
}

// WaitJobs gives running jobs up to timeout to stop after the daemon context is cancelled, so they
// record "interrupted" instead of failing on a closed database.
func (s *Service) WaitJobs(timeout time.Duration) {
	done := make(chan struct{})
	go func() {
		s.jobsWG.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		s.Log.Warn("background jobs still running at shutdown; they resume as interrupted")
	}
}

// errJobTaken is the cause of a run's cancel when its job was retried and is no longer its own.
var errJobTaken = errors.New("job taken by a newer run")

// runJob executes one queued job to completion, failure or cancellation. Jobs run one at a time per
// engine (indexing holds the engine's index lock), so a second job waits in "queued".
func (s *Service) runJob(parent context.Context, id string) {
	token := newID("lease_")
	j, err := s.TakeLease(id, token, jobLeaseTTL)
	if err != nil {
		s.Log.Warn("job not started", "id", id, "err", err)
		return
	}
	ctx, cancelCause := context.WithCancelCause(parent)
	defer cancelCause(nil)
	// a panic fails the job (and frees its lease) instead of leaving it "running" forever
	defer func() {
		if r := recover(); r != nil {
			s.finishJob(j.ID, token, j.Attempt, nil, crash.Error(s.Log, "job "+id, r), "INTERNAL")
		}
	}()
	// a cancel from the console (jobs.cancel) stops the run at the next batch
	watchDone := make(chan struct{})
	defer func() { cancelCause(nil); <-watchDone }()
	go func() {
		defer close(watchDone)
		s.watchJob(ctx, cancelCause, j, token, jobLeaseTTL, jobWatchPeriod)
	}()
	switch j.Type {
	case jobPrepare:
		s.runPrepareJob(ctx, j, token)
	default:
		s.finishJob(j.ID, token, j.Attempt, nil, fmt.Errorf("unknown job type %s", j.Type), "UNKNOWN_JOB")
	}
}

// watchJob stops a run whose job was cancelled, or is no longer its own (a retry handed the job to a
// new run: errJobTaken, and the run leaves the block to the new one), and renews the run's lease every
// quarter of ttl while it runs. It returns when ctx ends or the run is stopped.
func (s *Service) watchJob(ctx context.Context, cancelCause context.CancelCauseFunc, j jobs.Job, token string, ttl, period time.Duration) {
	// stopped reads the row and stops the run if the job is not its own any more
	stopped := func() bool {
		cur, err := getJob(s.Store, j.ID)
		switch {
		case err != nil:
			return false
		case cur.Attempt == j.Attempt && cur.Status == jobs.Cancelled:
			cancelCause(nil)
		case cur.Attempt != j.Attempt || cur.LeaseToken != token:
			cancelCause(errJobTaken)
		default:
			return false
		}
		return true
	}
	t := time.NewTicker(period)
	defer t.Stop()
	// wall time, like the lease: the monotonic clock stops while the machine sleeps
	renewed, warned := s.Clock.Now().Round(0), time.Time{}
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if stopped() {
				return
			}
			now := s.Clock.Now().Round(0)
			if sinceWithin(now, renewed, ttl/4) {
				continue
			}
			err := s.renewJobLease(j.ID, token, j.Attempt, ttl)
			switch {
			case err == nil:
				renewed = now
			case errors.Is(err, sql.ErrNoRows):
				cancelCause(errJobTaken) // the job is gone
				return
			case err.Error() == errcode.LeaseMismatch:
				// lost between the read and the renewal: a cancel stops it as a cancel, anything else as taken
				if !stopped() {
					cancelCause(errJobTaken)
				}
				return
			default:
				// a busy database; the next tick tries again (logged once in ten minutes)
				if !sinceWithin(now, warned, 10*time.Minute) {
					s.Log.Warn("job lease not renewed", "id", j.ID, "err", err)
					warned = now
				}
			}
		}
	}
}

// runOwnsJob says whether the run holding token for attempt still owns its job: still its lease, or
// its own attempt cancelled with nobody having taken the job since.
func (s *Service) runOwnsJob(id, token string, attempt int) bool {
	cur, err := getJob(s.Store, id)
	if err != nil {
		// a job that is gone is not ours; a read that failed (a busy database) is not a reason to
		// leave the job running with nobody to finish it
		return !errors.Is(err, sql.ErrNoRows)
	}
	return cur.Attempt == attempt && (cur.LeaseToken == token || (cur.Status == jobs.Cancelled && cur.LeaseToken == ""))
}

func (s *Service) runPrepareJob(ctx context.Context, j jobs.Job, token string) {
	ctx = observability.WithJob(ctx, j.ID)
	defer s.flushMemoryLLMCalls(ctx)
	var p preparePayload
	if err := json.Unmarshal([]byte(j.Payload), &p); err != nil {
		s.finishJob(j.ID, token, j.Attempt, nil, err, errcode.InvalidInput)
		return
	}
	candidate, ok := s.blockBySource(p.SourceRoom)
	if !ok {
		s.finishJob(j.ID, token, j.Attempt, nil, errors.New("block not found"), errcode.InvalidInput)
		return
	}
	// a batch for the room already in flight is resumed in batch mode whatever the job asked for
	if eng, err := s.MemoryEngine(); err == nil && p.mode() == prepareLive {
		if _, ok := batchPlanOf(eng, p.SourceRoom); ok {
			p.Mode = prepareBatch
		}
	}
	batch := p.mode() == prepareBatch
	started := time.Now()
	var last time.Time
	phaseSpan := map[string][2]float64{"sync": {0, 5}, "embed": {5, 70}, "summaries": {70, 100}}
	if !s.Doc().Memory.Summaries {
		phaseSpan["embed"] = [2]float64{5, 100}
	}
	if s.Doc().Memory.MessageVectorsOn() {
		// the per-message vectors are the last phase; the leaves and summaries share the first part
		phaseSpan["embed"], phaseSpan["summaries"], phaseSpan["messages"] = [2]float64{5, 40}, [2]float64{40, 65}, [2]float64{65, 100}
		if !s.Doc().Memory.Summaries {
			phaseSpan["messages"] = [2]float64{40, 100}
		}
	}
	if batch {
		// waiting for the provider is most of a batch preparation; the live remainder is the tail
		phaseSpan = map[string][2]float64{"sync": {0, 5}, "batch": {5, 85}, "embed": {85, 90}, "summaries": {90, 95}, "messages": {95, 100}}
	}
	publish := func(phase, label string, done, total int, eta bool) {
		now := time.Now()
		if now.Sub(last) < jobProgressPeriod && done < total {
			return
		}
		last = now
		span, ok := phaseSpan[phase]
		if !ok {
			span = [2]float64{0, 100}
		}
		pct := 0.0
		if total > 0 {
			pct = float64(done) / float64(total)
		}
		overall := span[0] + (span[1]-span[0])*pct
		elapsed := now.Sub(started).Seconds()
		pr := jobs.Progress{Phase: phase, Done: done, Total: total, Remaining: max(0, total-done), PhasePct: math.Round(pct*1000) / 10,
			OverallPct: math.Round(overall*10) / 10, ElapsedS: int(elapsed), Label: label}
		if eta && overall > 1 && overall < 100 {
			eta := elapsed * (100 - overall) / overall
			e := int(eta)
			pr.ETAS, pr.ETALabel = &e, jobs.FormatETA(eta)
		}
		_, _ = s.setRunProgress(j.ID, token, j.Attempt, pr)
	}
	progress := func(phase string, done, total int) {
		publish(phase, fmt.Sprintf("%s %d/%d", phase, done, total), done, total, true)
	}
	// the provider decides when a batch is done (within its deadline): no ETA, and the state in the label
	// (the console localizes the phase)
	status := func(state openai.BatchState, done, total int) {
		publish("batch", fmt.Sprintf("batch %s %d/%d", state, done, total), done, total, false)
	}
	owned := func() bool { return s.runOwnsJob(j.ID, token, j.Attempt) }
	var prepared blocks.Block
	var rep engine.IndexReport
	var err error
	if batch {
		prepared, rep, err = s.prepareRoomBatch(ctx, candidate, p, progress, status, owned)
	} else {
		prepared, rep, err = s.prepareRoom(ctx, candidate, p, progress, owned)
	}
	if errors.Is(err, errJobTaken) || errors.Is(context.Cause(ctx), errJobTaken) {
		return // the job is a newer run's now: its outcome, not this one, is recorded
	}
	result := map[string]any{"source_room": p.SourceRoom, "block_id": prepared.ID, "status": prepared.Status,
		"leaf_vectors": rep.LeafVectors, "from_cache": rep.FromCache, "summaries": rep.Summaries,
		"spent_usd": rep.SpentUSD, "stopped_by": rep.StoppedBy, "elapsed": rep.Elapsed, "mode": p.mode()}
	if s.Doc().Memory.MessageVectorsOn() {
		result["message_vectors"] = rep.MessageVectors
	}
	if batch {
		// counts only: leaves and messages sent in batches, those of them left to the live remainder
		// (failed, expired, missing), and those planned live because the host cannot batch them
		result["batch_items"], result["batch_failed"], result["live_items"] = rep.BatchItems, rep.BatchFailed, rep.LiveItems
		if rep.Estimate != nil && rep.Estimate.BatchUSD != nil {
			result["batch_estimate_usd"] = *rep.Estimate.BatchUSD
		}
	}
	code := ""
	switch {
	case err == nil:
	case s.jobCtx != nil && s.jobCtx.Err() != nil:
		code = "INTERRUPTED" // the service is stopping; the next start offers a retry
		result["stopped_by"] = "interrupted"
	case errors.Is(err, engine.ErrBatchSubmitUnknown):
		code = "BATCH_SUBMIT_UNKNOWN" // the room stays held until the plan is settled or discarded
	case errors.Is(err, engine.ErrBatchSubmitRefused):
		code = "BATCH_SUBMIT_REFUSED"
	case errors.Is(err, engine.ErrBatchProviderGone):
		code = "BATCH_PROVIDER_GONE"
	case errors.Is(err, engine.ErrBatchResultsUnreadable):
		code = "BATCH_RESULTS_UNREADABLE"
	case errors.Is(err, engine.ErrBatchDraining):
		code = "BATCH_DRAINING"
	case rep.StoppedBy == "budget":
		code = "BUDGET_REACHED"
	case rep.StoppedBy == "cancelled" || errors.Is(err, context.Canceled):
		code = "CANCELLED"
		result["stopped_by"] = "cancelled"
	case errors.Is(err, errProviderMissing):
		code = errcode.ProviderUnconfigured
	case rep.StoppedBy == "provider_error":
		code = errcode.ProviderUnavailable
	default:
		code = "PREPARE_FAILED"
	}
	s.finishJob(j.ID, token, j.Attempt, result, err, code)
}

// finishJob records the outcome of the run that holds token for attempt; a job cancelled meanwhile
// keeps its cancelled state. Every write compares the attempt: a run never writes to a later one's row.
func (s *Service) finishJob(id, token string, attempt int, result map[string]any, runErr error, code string) {
	raw, _ := json.Marshal(result)
	cur, err := getJob(s.Store, id)
	if err != nil || cur.Status == jobs.Cancelled {
		if err == nil && result != nil {
			cur.Result = string(raw)
			// only while it is still this attempt's cancel: a retry made in between must not be turned
			// back, nor a later attempt's cancelled result overwritten
			if saveRunJob(s.Store, cur, cur.LeaseToken, jobs.Cancelled, attempt) == nil {
				s.jobChanged(cur)
			}
		}
		return
	}
	if renewed, err := jobs.Renew(cur, token, s.Clock.Now(), time.Minute); err == nil && cur.Attempt == attempt {
		// a lease that ran out while nobody took the job (the machine slept past it) is still this
		// run's: the save below compares token and attempt
		cur = renewed
	}
	if runErr == nil {
		j, err := jobs.Complete(cur, token, s.Clock.Now(), string(raw))
		if err == nil {
			err = saveRunJob(s.Store, j, token, "", attempt)
		}
		if err != nil {
			s.Log.Warn("job completion not saved", "id", id, "err", err)
			return
		}
		s.jobChanged(j)
		return
	}
	j, err := jobs.Fail(cur, token, s.Clock.Now(), code, false)
	if err != nil {
		s.Log.Warn("job failure not saved", "id", id, "err", err)
		return
	}
	if result == nil {
		result = map[string]any{}
	}
	result["error"] = clip(runErr.Error(), 300)
	raw, _ = json.Marshal(result)
	j.Result = string(raw)
	if err := saveRunJob(s.Store, j, token, "", attempt); err == nil {
		s.jobChanged(j)
	}
}

// ResumeJobs is called once at daemon start: jobs that were running when the process stopped are
// marked failed (they can be retried), and queued ones start. A batch preparation is the exception: its
// batches are submitted and paid for, so a run interrupted with its plan stored runs again at once (as a
// new attempt of the same job) and polls the same batches. A stored plan no running job owns is taken up
// by the room's latest prepare job when that one failed (a graceful stop records it INTERRUPTED; the
// same job runs again, so a restart never adds a job), else by a new job; a draining plan is collected
// in the background without a job.
func (s *Service) ResumeJobs(ctx context.Context) {
	s.jobCtx = ctx
	if s.Store == nil {
		return
	}
	list, err := listNonFinishedJobs(s.Store)
	if err != nil {
		return
	}
	plans := map[string]bool{}
	eng, err := s.MemoryEngine()
	if err == nil {
		if stored, err := eng.BatchPlans(); err == nil {
			for _, p := range stored {
				plans[p.Room] = true
			}
		} else {
			s.Log.Warn("memory: stored batch plans not read", "err", err)
		}
	}
	owned := map[string]bool{}
	for _, j := range list {
		if j.Type != jobPrepare {
			continue
		}
		var p preparePayload
		_ = json.Unmarshal([]byte(j.Payload), &p)
		planned := plans[p.SourceRoom]
		switch j.Status {
		case jobs.Leased, jobs.Running:
			prev := j.LeaseToken
			j.Status, j.ErrorCode, j.LeaseToken = jobs.Failed, "INTERRUPTED", ""
			j.Result = `{"stopped_by":"interrupted"}`
			if planned {
				// a new attempt: the dead run's lease is gone, and the batches are polled, not sent again
				if again, err := jobs.Retry(j); err == nil && saveJob(s.Store, again, prev) == nil {
					owned[p.SourceRoom] = true
					s.jobChanged(again)
					s.goJob(again.ID)
				}
				continue
			}
			if saveJob(s.Store, j, prev) == nil {
				s.jobChanged(j)
			}
		case jobs.Pending:
			owned[p.SourceRoom] = true // its run resumes the room's plan (runPrepareJob)
			s.goJob(j.ID)
		}
	}
	latest := s.latestJobs()
	for room := range plans {
		if owned[room] {
			continue
		}
		plan, ok, err := eng.LoadBatchPlan(room)
		if err != nil || !ok {
			continue
		}
		if plan.Drain {
			s.goDrain(room) // only collected: nothing of it runs live, and no job is needed
			continue
		}
		// a plan whose job ended without finishing it: resume it, or the room would stay held
		if j, ok := latest[room]; ok && j.Status == jobs.Failed {
			if again, err := jobs.Retry(j); err == nil && saveJob(s.Store, again, j.LeaseToken) == nil {
				s.jobChanged(again)
				s.goJob(again.ID)
				continue
			}
		}
		if _, _, err := s.startPrepareJob(preparePayload{SourceRoom: room, Mode: prepareBatch, BudgetUSD: plan.BudgetUSD, Limit: plan.Limit}); err != nil {
			s.Log.Warn("memory: stored batch not resumed", "room", room, "err", err)
		}
	}
}

// retriedJob starts a job that jobs.retry put back to pending.
func (s *Service) retriedJob(j jobs.Job) {
	if j.Type == jobPrepare && j.Status == jobs.Pending {
		s.goJob(j.ID)
	}
}

// latestJobs maps a source room to its newest prepare job.
func (s *Service) latestJobs() map[string]jobs.Job {
	out := map[string]jobs.Job{}
	if s.Store == nil {
		return out
	}
	list, err := latestPrepareJobs(s.Store)
	if err != nil {
		return out
	}
	for _, j := range list {
		var p preparePayload
		if j.Type == jobPrepare && json.Unmarshal([]byte(j.Payload), &p) == nil {
			out[p.SourceRoom] = j
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// update and delete
// ---------------------------------------------------------------------------

func (s *Service) updateMemoryBlock(req action.Request, in map[string]any) action.Result {
	b, ok := s.resolveBlockRoom(in)
	if !ok {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "block not found", nil)
	}
	eng, err := s.MemoryEngine()
	if err != nil {
		return action.Fail(req, errcode.NotSetup, "generic.error", err.Error(), nil)
	}
	changed := []string{}
	if _, set := in["label"]; set {
		label := strings.TrimSpace(strVal(in, "label", ""))
		if label == "" || utf8.RuneCountInString(label) > maxBlockLabel || strings.ContainsAny(label, "\r\n\x00") {
			return action.Fail(req, errcode.InvalidInput, "generic.error", fmt.Sprintf("label must be 1–%d characters on one line", maxBlockLabel), nil)
		}
		if err := eng.Store.SetRoomLabel(b.SourceRoom, label); err != nil {
			if errors.Is(err, store.ErrNoRoom) {
				return action.Fail(req, errcode.InvalidInput, "generic.error", "this block has no stored messages yet; it takes the channel name", nil)
			}
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		changed = append(changed, "label")
	}
	meta := s.blockMeta(b.SourceRoom)
	metaChanged := false
	if _, set := in["note"]; set {
		note := strings.TrimSpace(strVal(in, "note", ""))
		if utf8.RuneCountInString(note) > maxBlockNote || strings.ContainsRune(note, 0) {
			return action.Fail(req, errcode.InvalidInput, "generic.error", fmt.Sprintf("note must be at most %d characters", maxBlockNote), nil)
		}
		meta.Note, metaChanged = note, true
		changed = append(changed, "note")
	}
	if _, set := in["auto_update"]; set {
		v := boolVal(in, "auto_update")
		meta.AutoUpdate, metaChanged = &v, true
		changed = append(changed, "auto_update")
	}
	if _, set := in["budget_usd"]; set {
		raw := strings.TrimSpace(strVal(in, "budget_usd", ""))
		if raw != "" {
			if v := parseUSD(raw, -1); v < 0 || v > 1000 {
				return action.Fail(req, errcode.InvalidInput, "generic.error", "budget_usd must be 0–1000", nil)
			}
		}
		meta.BudgetUSD, metaChanged = raw, true
		changed = append(changed, "budget_usd")
	}
	if _, set := in["mode"]; set {
		mode := strings.TrimSpace(strVal(in, "mode", ""))
		if mode != "" && !validPrepareMode(mode) {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "mode must be live or batch (empty = live)", nil)
		}
		meta.Mode, metaChanged = mode, true
		changed = append(changed, "mode")
	}
	if boolVal(in, "discard_batch") {
		// the operator drops the room's batch plan (a submission that cannot be settled, a provider that is
		// gone): open batches are cancelled and every reachable one cleaned up, best effort; the room is
		// released and nothing of the batch runs live
		if _, ok := batchPlanOf(eng, b.SourceRoom); !ok {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "this block has no batch to discard", nil)
		}
		for _, j := range s.latestJobs() {
			var p preparePayload
			if activeJob(j) && json.Unmarshal([]byte(j.Payload), &p) == nil && p.SourceRoom == b.SourceRoom {
				return action.Fail(req, errcode.InvalidInput, "generic.error", "a prepare job is running for this block; cancel it first", map[string]any{"job_id": j.ID})
			}
		}
		dctx, cancel := context.WithTimeout(context.Background(), batchCancelTimeout)
		err := s.discardBatch(dctx, eng, b.SourceRoom)
		cancel()
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		changed = append(changed, "discard_batch")
	}
	if len(changed) == 0 {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "nothing to change (label, note, auto_update, budget_usd, mode, discard_batch)", nil)
	}
	if metaChanged {
		if err := s.kvSet(blockMetaKey(b.SourceRoom), meta); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
	}
	s.emit("memory", b.ID, map[string]any{"source_room": b.SourceRoom, "changed": changed})
	res := action.OK(req, map[string]any{"source_room": b.SourceRoom, "changed": changed})
	if eng.Index() != nil && contains(changed, "label") {
		s.ownedGo("memory reload", func(context.Context) { _, _ = eng.Reload() }) // evidence labels come from the loaded index
	}
	return res
}

func contains(list []string, v string) bool {
	for _, x := range list {
		if x == v {
			return true
		}
	}
	return false
}

// deleteMemoryBlock removes a block's stored messages and index rows and detaches it from every
// channel. The imported source file is not touched. confirm must repeat the block's label (or room).
func (s *Service) deleteMemoryBlock(req action.Request, in map[string]any) action.Result {
	b, ok := s.resolveBlockRoom(in)
	if !ok {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "block not found", nil)
	}
	confirm := strings.TrimSpace(strVal(in, "confirm", ""))
	if confirm == "" || (confirm != b.Label && confirm != b.SourceRoom) {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "type the block's name to confirm", map[string]any{"expected": b.Label})
	}
	for _, j := range s.latestJobs() {
		var p preparePayload
		if activeJob(j) && json.Unmarshal([]byte(j.Payload), &p) == nil && p.SourceRoom == b.SourceRoom {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "a prepare job is running for this block; cancel it first", map[string]any{"job_id": j.ID})
		}
	}
	eng, err := s.MemoryEngine()
	if err != nil {
		return action.Fail(req, errcode.NotSetup, "generic.error", err.Error(), nil)
	}
	// detach from every channel first, so no channel keeps a dangling reference
	detached := 0
	for guildID, g := range s.Doc().Guilds {
		for channelID, pol := range g.Channels {
			for _, id := range pol.RAGView.Blocks {
				if bb, ok := s.lookupBlock(id); ok && bb.SourceRoom == b.SourceRoom {
					r := s.channelsViewDetach(req, map[string]any{"guild_id": guildID, "channel_id": channelID, "block_id": id})
					if !r.OK {
						return r
					}
					detached++
				}
			}
		}
	}
	// a batch still at the provider is cancelled and its files removed before the room goes (DeleteRoom
	// drops the plan row with the rest)
	if _, ok := batchPlanOf(eng, b.SourceRoom); ok {
		dctx, cancel := context.WithTimeout(context.Background(), batchCancelTimeout)
		if err := s.discardBatch(dctx, eng, b.SourceRoom); err != nil {
			s.Log.Warn("memory: batch of a deleted block not discarded", "room", b.SourceRoom, "err", err)
		}
		cancel()
	}
	removed, err := eng.Store.DeleteRoom(b.SourceRoom)
	if err != nil && !errors.Is(err, store.ErrNoRoom) {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	s.dropPreparedBlocks(b.SourceRoom)
	_ = s.kvDelete(blockMetaKey(b.SourceRoom))
	s.ownedGo("memory reload", func(context.Context) { _, _ = eng.Reload() })
	s.emit("memory", b.ID, map[string]any{"source_room": b.SourceRoom, "deleted": true})
	return action.OK(req, map[string]any{"source_room": b.SourceRoom, "messages_removed": removed, "channels_detached": detached,
		"source_file_untouched": true})
}

// ---------------------------------------------------------------------------
// catalog
// ---------------------------------------------------------------------------

func (s *Service) savePreparedBlock(block blocks.Block) error {
	s.blocksMu.Lock()
	defer s.blocksMu.Unlock()
	items := s.loadPreparedBlocks()
	found := false
	for i := range items {
		if items[i].ID == block.ID {
			items[i] = block
			found = true
			break
		}
	}
	if !found {
		items = append(items, block)
	}
	return s.writePreparedBlocks(items)
}

func (s *Service) writePreparedBlocks(items []blocks.Block) error {
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	raw, err := json.MarshalIndent(map[string]any{"schema": 1, "blocks": items}, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(s.blockCatalogPath(), raw)
}

func (s *Service) dropPreparedBlocks(room string) {
	s.blocksMu.Lock()
	defer s.blocksMu.Unlock()
	items := s.loadPreparedBlocks()
	kept := items[:0]
	for _, b := range items {
		if b.SourceRoom != room {
			kept = append(kept, b)
		}
	}
	_ = s.writePreparedBlocks(kept)
}

func (s *Service) blockBySource(source string) (blocks.Block, bool) {
	for _, group := range [][]blocks.Block{s.dbBlocks(), s.loadPreparedBlocks(), s.declaredDiscordBlocks()} {
		for _, block := range group {
			if block.SourceRoom == source {
				return block, true
			}
		}
	}
	return blocks.Block{}, false
}

func (s *Service) embedSpace() string {
	m := s.Doc().Models["embed"]
	return blocks.EmbeddingSpace(m.Provider, m.Model, m.Dimensions, "l2")
}

func (s *Service) blockCatalogPath() string {
	return filepath.Join(s.DataRoot, "memory", "blocks", "index.json")
}

func (s *Service) loadPreparedBlocks() []blocks.Block {
	raw, err := os.ReadFile(s.blockCatalogPath())
	if err != nil {
		return nil
	}
	var payload struct {
		Blocks []blocks.Block `json:"blocks"`
	}
	if json.Unmarshal(raw, &payload) != nil {
		return nil
	}
	return payload.Blocks
}

// channelInfo names one channel the console knows (live on Discord or configured).
type channelInfo struct {
	GuildID   string `json:"guild_id"`
	GuildName string `json:"guild_name"`
	ID        string `json:"channel_id"`
	Name      string `json:"name"`
	Live      bool   `json:"live"`
}

func (s *Service) knownChannels() []channelInfo {
	var out []channelInfo
	seen := map[string]bool{}
	guildNames := map[string]string{}
	if s.Discord != nil && s.Discord.Connected() {
		for _, g := range s.Discord.Guilds() {
			guildNames[g.ID] = g.Name
			for _, c := range s.Discord.Channels(g.ID) {
				if c.Type != "" && c.Type != "text" {
					continue
				}
				seen[g.ID+":"+c.ID] = true
				out = append(out, channelInfo{GuildID: g.ID, GuildName: g.Name, ID: c.ID, Name: c.Name, Live: true})
			}
		}
	}
	for guildID, g := range s.Doc().Guilds {
		for channelID := range g.Channels {
			if seen[guildID+":"+channelID] {
				continue
			}
			name := guildNames[guildID]
			if name == "" {
				name = guildID
			}
			out = append(out, channelInfo{GuildID: guildID, GuildName: name, ID: channelID, Name: channelID})
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].GuildName != out[j].GuildName {
			return out[i].GuildName < out[j].GuildName
		}
		return out[i].Name < out[j].Name
	})
	return out
}

// channelDirectory is every known channel with the blocks it searches, for the connection map.
func (s *Service) channelDirectory() []map[string]any {
	out := []map[string]any{}
	var lookup func(string) (blocks.Block, bool) // read once, at the first attached block
	for _, c := range s.knownChannels() {
		pol := s.channelPolicy(c.GuildID, c.ID)
		attached := []map[string]any{}
		for _, id := range pol.RAGView.Blocks {
			entry := map[string]any{"block_id": id, "stale": true}
			if lookup == nil {
				lookup = s.blockLookup()
			}
			if b, ok := lookup(id); ok {
				entry = map[string]any{"block_id": id, "source_room": b.SourceRoom, "label": b.Label, "stale": false}
			}
			attached = append(attached, entry)
		}
		out = append(out, map[string]any{"guild_id": c.GuildID, "guild_name": c.GuildName, "channel_id": c.ID, "name": c.Name,
			"live": c.Live, "own_room": engine.RoomKey(c.GuildID, c.ID), "memory_use": pol.MemoryUse, "capture": pol.Capture,
			"attached": attached})
	}
	return out
}

func (s *Service) declaredDiscordBlocks() []blocks.Block {
	space := s.embedSpace()
	seen := map[string]struct{}{}
	out := []blocks.Block{}
	for _, c := range s.knownChannels() {
		room := blocks.ChannelKey(c.GuildID, c.ID)
		id := blocks.BlockID("discord", room, blocks.UnpreparedSegmentation, space)
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		label := strings.TrimSpace(c.Name)
		if label == "" || label == c.ID {
			label = blocks.LabelForRoom(room)
		} else if !strings.HasPrefix(label, "#") {
			label = "#" + label
		}
		out = append(out, blocks.Block{ID: id, Platform: "discord", Collection: "discord", SourceRoom: room, Label: label,
			SegmentationVersion: blocks.UnpreparedSegmentation, EmbeddingSpace: space, Status: "none", TimeSemantics: "offset"})
	}
	return out
}

// mergedBlocks is every block the console shows: stored rooms, prepared artifacts and the Discord
// channels that have no stored messages yet, each with its counts, span, settings, connected
// channels and newest job.
func (s *Service) mergedBlocks() []map[string]any {
	byRoom := map[string]blocks.Block{}
	for _, b := range s.declaredDiscordBlocks() {
		byRoom[b.SourceRoom] = b
	}
	prepared := s.loadPreparedBlocks()
	preparedByRoom := map[string][]blocks.Block{}
	for _, b := range prepared {
		byRoom[b.SourceRoom] = b
		preparedByRoom[b.SourceRoom] = append(preparedByRoom[b.SourceRoom], b)
	}
	stats := map[string]store.RoomStats{}
	spans := map[string]store.RoomSpan{}
	var estimate func(messages int) int64 // nil while the engine is unavailable
	resident := map[string]bool{}
	batches := map[string]engine.BatchPlanInfo{}
	if eng, err := s.MemoryEngine(); err == nil {
		if plans, err := eng.BatchPlans(); err == nil {
			for _, p := range plans {
				batches[p.Room] = p
			}
		}
		estimate = eng.EstimateRoomBytes
		if ix := eng.Index(); ix != nil { // none before the first load
			for _, room := range ix.Rooms() {
				resident[room] = true
			}
		}
		if rs, err := eng.Store.Rooms(); err == nil {
			for _, r := range rs {
				stats[r.Room.Room] = r
			}
		}
		spans, _ = eng.Store.RoomSpans()
	}
	for _, b := range s.dbBlocks() {
		// Width changes produce a new ID, but preparation belongs to the same
		// room, recipe and model. Keep the latest compatible preparation record.
		var latest blocks.Block
		for _, p := range preparedByRoom[b.SourceRoom] {
			if sameBlockPreparation(b, p) && (latest.ID == "" || p.PreparedAt > latest.PreparedAt) {
				latest = p
			}
		}
		if latest.ID != "" {
			b.PreparedAt, b.CostUSD, b.Error, b.Hints = latest.PreparedAt, latest.CostUSD, latest.Error, latest.Hints
			if latest.Status == "failed" && b.Status != "ready" {
				b.Status = "failed"
			}
		}
		byRoom[b.SourceRoom] = b
	}
	// which channels search each room (attached blocks resolve to their room)
	usedBy := map[string][]map[string]any{}
	names := map[string]channelInfo{}
	var lookup func(string) (blocks.Block, bool) // read once, at the first attached block
	for _, c := range s.knownChannels() {
		names[engine.RoomKey(c.GuildID, c.ID)] = c
		pol := s.channelPolicy(c.GuildID, c.ID)
		for _, id := range pol.RAGView.Blocks {
			if lookup == nil {
				lookup = s.blockLookup()
			}
			if b, ok := lookup(id); ok {
				usedBy[b.SourceRoom] = append(usedBy[b.SourceRoom], map[string]any{"guild_id": c.GuildID, "channel_id": c.ID,
					"name": c.Name, "guild_name": c.GuildName, "block_id": id})
			}
		}
	}
	jobsByRoom := s.latestJobs()
	items := make([]blocks.Block, 0, len(byRoom))
	for _, b := range byRoom {
		items = append(items, b)
	}
	sort.Slice(items, func(i, j int) bool { return items[i].SourceRoom < items[j].SourceRoom })
	out := make([]map[string]any, 0, len(items))
	for _, b := range items {
		st := stats[b.SourceRoom]
		meta := s.blockMeta(b.SourceRoom)
		status := b.Status
		if st.Messages > 0 && status == "none" {
			status = "imported"
		}
		m := map[string]any{
			"block_id": b.ID, "platform": b.Platform, "collection": b.Collection, "source_room": b.SourceRoom,
			"label": b.Label, "segmentation_version": b.SegmentationVersion, "embedding_space": b.EmbeddingSpace,
			"status": status, "time_semantics": b.TimeSemantics, "message_count": st.Messages, "leaf_count": st.Leaves,
			"embedded": st.Embedded, "pending_leaves": max(0, st.Leaves-st.Open-st.Embedded), "open_leaves": st.Open, "prepared_at": b.PreparedAt,
			"cost_usd": b.CostUSD, "error": b.Error, "hints": b.Hints, "note": meta.Note, "auto_update": meta.autoUpdate(),
			"budget_usd": meta.BudgetUSD, "mode": meta.Mode, "channels": append([]map[string]any{}, usedBy[b.SourceRoom]...),
		}
		if sp, ok := spans[b.SourceRoom]; ok {
			m["first"], m["last"], m["speakers"] = sp.First, sp.Last, sp.Speakers
		}
		if estimate != nil && st.Messages > 0 {
			// an estimate from the room's size (a fixed cost plus a cost per message), not a measured size;
			// resident says whether the room is in the loaded index now (prepared is not the same as loaded)
			m["memory_estimate_bytes"], m["resident"] = estimate(st.Messages), resident[b.SourceRoom]
		}
		if c, ok := names[b.SourceRoom]; ok && b.Platform == "discord" {
			pol := s.channelPolicy(c.GuildID, c.ID)
			m["own_channel"] = map[string]any{"guild_id": c.GuildID, "channel_id": c.ID, "name": c.Name, "guild_name": c.GuildName,
				"capture": pol.Capture, "capture_on": pol.Capture && s.Doc().Memory.Capture}
		}
		if j, ok := jobsByRoom[b.SourceRoom]; ok {
			m["job"] = jobJSON(j)
			if activeJob(j) {
				m["status"] = "preparing"
			}
		}
		if p, ok := batches[b.SourceRoom]; ok && p.State != engine.BatchPlanApplied {
			// the provider has the room's work (up to its deadline, about a day): preparing, waiting on the batch.
			// batch.state says which wait: waiting (results outstanding), draining (cancelled or a stopped
			// submission: collected, nothing more is sent) or submit_unknown (the operator must settle it)
			m["status"], m["waiting_batch"] = "preparing", true
			note := "waiting for the provider to return the batch (usually within 24 hours)"
			switch p.State {
			case engine.BatchPlanDraining:
				note = "collecting the batches still at the provider; nothing more is sent or run live"
			case engine.BatchPlanSubmitUnknown:
				m["status"], note = "failed", "a batch submission got no answer; check the provider's batch list, then discard the batch (discard_batch)"
				m["error"] = p.Reason
			}
			m["batch"] = map[string]any{"items": p.Items, "submitted_at": p.CreatedAt, "note": note, "state": p.State, "reason": p.Reason}
		}
		out = append(out, m)
	}
	return out
}

func (s *Service) lookupBlock(id string) (blocks.Block, bool) {
	if !validBlockID(id) {
		return blocks.Block{}, false
	}
	return s.blockLookup()(id)
}

// blockLookup reads the stored, declared and prepared blocks once and resolves any number of ids
// against them. A page that resolves every channel's attached blocks uses one: each fresh read counts
// every stored room, about a second on a large memory.
func (s *Service) blockLookup() func(id string) (blocks.Block, bool) {
	stored := s.dbBlocks()
	declared := s.declaredDiscordBlocks()
	// Display-name ties or renames must not rebuild aliases for unchanged IDs.
	sort.Slice(declared, func(i, j int) bool { return declared[i].ID < declared[j].ID })
	prepared := s.loadPreparedBlocks()
	return func(id string) (blocks.Block, bool) {
		if !validBlockID(id) {
			return blocks.Block{}, false
		}
		return s.resolveBlock(id, stored, declared, prepared)
	}
}

func (s *Service) resolveBlock(id string, stored, declared, prepared []blocks.Block) (blocks.Block, bool) {
	index, alias := s.blockWidthAliasIndex(stored, declared, id)
	if alias && index < len(stored) {
		return stored[index], true
	}
	for _, b := range prepared {
		if b.ID == id {
			for _, current := range stored {
				if sameBlockPreparation(b, current) {
					return current, true
				}
			}
			return b, true
		}
	}
	if alias {
		return declared[index-len(stored)], true
	}
	return blocks.Block{}, false
}

// lookupBlocks resolves the blocks a view holds; ids that no longer resolve (their room was deleted
// or re-prepared under another recipe) are skipped so they never block a new attach.
func (s *Service) lookupBlocks(ids []string) []blocks.Block {
	out := make([]blocks.Block, 0, len(ids))
	if len(ids) == 0 {
		return out
	}
	lookup := s.blockLookup()
	for _, id := range ids {
		if b, ok := lookup(id); ok {
			out = append(out, b)
		}
	}
	return out
}

func viewMap(v config.MemoryView, channelKey string) map[string]any {
	return map[string]any{
		"channel_key":       channelKey,
		"blocks":            v.Blocks,
		"revision":          v.Revision,
		"scope_fingerprint": v.ScopeFingerprint,
		"updated_at":        v.UpdatedAt,
	}
}
