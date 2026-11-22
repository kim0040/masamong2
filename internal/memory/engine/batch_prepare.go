package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"masamong/internal/memory/store"
	"masamong/internal/providers/openai"
)

// workItem is one request to submit with the leaves or messages it covers.
type workItem struct {
	req     openai.BatchItem
	targets []BatchTarget
	bytes   int
	tokens  int
}

type batchWork struct {
	purpose string
	model   string
	items   []workItem
}

func (w batchWork) units(from int) int {
	n := 0
	for _, it := range w.items[from:] {
		n += len(it.targets)
	}
	return n
}

// PrepareBatch syncs the room, runs the free reuse pass, collects the pending work and submits it as
// batches where the role's host can batch it. A room that already has a stored plan gets that plan back
// (nothing is submitted again): the caller resumes it. The returned plan has no generation when nothing
// was submitted (no work, a budget stop, no host can batch any of it); the caller then runs the pass live.
//
// A submission the host refuses for another reason than ErrBatchUnsupported stops the submitting and
// returns ErrBatchSubmitRefused (the plan, when something was sent before, drains: nothing runs live). A
// lost answer leaves that group unknown for ReconcilePlan (WaitBatch settles it first) and the other
// groups are submitted. It holds indexMu, like IndexRooms.
func (e *Engine) PrepareBatch(ctx context.Context, room string, opts BatchOptions, progress Progress) (BatchPlan, IndexReport, error) {
	e.indexMu.Lock()
	defer e.indexMu.Unlock()
	start := time.Now()
	rep := IndexReport{Mode: "batch"}
	done := func(p BatchPlan, err error) (BatchPlan, IndexReport, error) {
		rep.BatchItems, rep.LiveItems = p.BatchItems, p.LiveItems
		rep.Elapsed = time.Since(start).Round(time.Millisecond).String()
		return p, rep, err
	}
	if e.retired.Load() {
		return done(BatchPlan{}, errRetired)
	}
	if progress == nil {
		progress = func(string, int, int) {}
	}
	if p, ok, err := e.LoadBatchPlan(room); err != nil || ok {
		return done(p, err)
	}
	rooms, err := e.syncRooms([]string{room}, &rep, progress)
	if err != nil || len(rooms) == 0 { // a room that did not sync is in rep.FailedRooms
		return done(BatchPlan{}, err)
	}
	space := e.EmbedSpace()
	if rep.Adapted, err = e.adaptSpace(); err != nil {
		return done(BatchPlan{}, err)
	}
	leafDims, err := e.pendingLeafDims()
	if err != nil {
		return done(BatchPlan{}, err)
	}
	recipe := e.Settings.Recipe.Version()
	pending, err := e.Store.PendingAtWidth(rooms, space, e.Settings.Summaries, opts.Limit, leafDims, recipe)
	if err != nil {
		return done(BatchPlan{}, err)
	}
	rep.Pending = len(pending)
	needEmbed, err := e.reuseLeaves(pending, space, leafDims, &rep)
	if err != nil {
		return done(BatchPlan{}, err)
	}
	// summaries: the leaves the live pass would summarize with a paid call (a summary text kept for the
	// same body is free, and is left to the live remainder like the summary vectors)
	var needSumm, after []store.PendingLeaf
	if e.Settings.Summaries {
		if after, err = e.Store.PendingAtWidth(rooms, space, true, opts.Limit, leafDims, recipe); err != nil {
			return done(BatchPlan{}, err)
		}
		for _, p := range after {
			if _, cached := e.cachedSummaryText(p.Body); p.NeedSumm && !cached {
				needSumm = append(needSumm, p)
			}
		}
	}
	var msgNeed []store.PendingMessage
	msgSpace, msgSelected := "", false
	if e.Settings.MessageVectors {
		dims, wait, err := e.messageVectorState()
		if err != nil {
			return done(BatchPlan{}, err)
		}
		msgSpace = e.messageSpaceAt(dims)
		// a model without MRL takes its message width from the leaves: before the first leaf is embedded it
		// is unknown, and the leaf results this batch brings set it before the message results are applied
		// (Apply goes in plan order). Any other wait (mixed or short widths) leaves the messages to the live
		// pass, which waits too.
		deferred := msgSpace == "" && wait == "leaf_width_unknown" && len(needEmbed) > 0
		if msgSpace != "" || deferred {
			if msgNeed, err = e.Store.PendingMessages(rooms, msgSpace, opts.Limit, recipe); err != nil {
				return done(BatchPlan{}, err)
			}
			msgSelected = true
		}
	}
	est := e.BatchEstimate(e.estimate(needEmbed, pending, msgNeed))
	// a summary batch for a model whose request shape is not learned yet starts with one live summary
	// (probeSummary), at the live price: the estimate the budget is checked against includes it in full
	probe := false
	if len(needSumm) > 0 && e.canBatch(purposeSummary) {
		if sl, ok := e.batcher(purposeSummary).(shapeLearner); ok && !sl.ShapeLearned(e.Models.Summary) {
			probe = true
			if est.Summaries > 0 {
				v := *est.BatchUSD + est.summaryChatUSD/float64(est.Summaries)*(1-e.discount(purposeSummary, e.Models.Summary))
				est.BatchUSD = &v
			}
		}
	}
	rep.Estimate = &est
	if e.LLM == nil || !e.LLM.Configured() {
		if len(needEmbed)+len(needSumm)+len(msgNeed) > 0 {
			rep.StoppedBy = "provider_not_configured"
		}
		return done(BatchPlan{}, nil)
	}
	// no host can batch any of the paid work (or there is none): the caller runs the normal live pass, with
	// its usual budget behaviour, instead of the all-at-once batch budget check below
	if !(len(needEmbed) > 0 && e.canBatch(purposeLeaf)) && !(len(needSumm) > 0 && e.canBatch(purposeSummary)) &&
		!(len(msgNeed) > 0 && e.canBatch(purposeMessage)) {
		return done(BatchPlan{LiveItems: len(needEmbed) + len(needSumm) + len(msgNeed)}, nil)
	}
	// the whole batch is submitted at once, so the budget is checked against its estimate before anything
	// is sent (a live pass instead stops starting calls once the budget is spent)
	if opts.BudgetUSD > 0 && *est.BatchUSD > opts.BudgetUSD {
		rep.StoppedBy = "budget"
		return done(BatchPlan{}, nil)
	}
	plan := BatchPlan{Version: batchPlanVersion, Room: room, Generation: newGeneration(), State: BatchPlanWaiting, Space: space,
		MessageSpace: msgSpace, EmbedModel: e.Models.Embed, SummaryModel: e.Models.Summary, BudgetUSD: opts.BudgetUSD,
		Limit: opts.Limit, EstimateUSD: *est.BatchUSD, CreatedAt: e.nowT().UTC()}
	if opts.Limit > 0 {
		plan.Workset = workset(pending, after, msgNeed, msgSelected)
	}
	// unsent is what the caller's live pass needs when nothing went to a batch: the room, the selection, and
	// what this preparation already spent (the probe), so the pass stays within the budget
	unsent := func() BatchPlan {
		return BatchPlan{Room: room, BudgetUSD: opts.BudgetUSD, Limit: opts.Limit, Workset: plan.Workset, LiveItems: plan.LiveItems,
			Summaries: plan.Summaries, SpentUSD: plan.SpentUSD}
	}
	if probe {
		// one live summary first, so the batch leaves out the optional fields the model refuses (a batch item
		// cannot be retried without them). When it fails nothing is sent: the batch would fail the same way,
		// and running the summaries live instead is what the operator chose not to pay for
		var err error
		if needSumm, err = e.probeSummary(ctx, needSumm, &plan); err != nil {
			if ctx.Err() != nil {
				rep.StoppedBy = "cancelled"
				return done(unsent(), nil)
			}
			return done(unsent(), err)
		}
	}
	stored := false // a plan row exists for this generation
	works := []batchWork{
		{purpose: purposeLeaf, model: e.Models.Embed, items: e.leafWork(needEmbed)},
		{purpose: purposeSummary, model: e.Models.Summary, items: e.summaryWork(needSumm)},
		{purpose: purposeMessage, model: e.Models.Embed, items: e.messageWork(msgNeed)},
	}
	var stopErr error
	for _, w := range works {
		if len(w.items) == 0 || plan.Cut {
			continue
		}
		client := e.batcher(w.purpose)
		kind := purposeKind(w.purpose)
		if client == nil || !client.BatchSupport(kind) {
			plan.LiveItems += w.units(0)
			continue
		}
		from := 0
	chunks:
		for _, chunk := range splitWork(w.items, client.BatchMaxItems(kind), kind) {
			if ctx.Err() != nil {
				break
			}
			reqs := make([]openai.BatchItem, len(chunk))
			items := make([]BatchPlanItem, len(chunk))
			units := 0
			for i, it := range chunk {
				reqs[i] = it.req
				items[i] = BatchPlanItem{ID: it.req.ID, Targets: it.targets, Tokens: it.tokens}
				units += len(it.targets)
			}
			prov := e.providerOf(w.purpose, client)
			key := plan.Generation[:16] + fmt.Sprintf("-%d", len(plan.Groups))
			// the intent is stored before the call: a process that stops during it finds the submission at
			// restart and looks it up instead of sending it again
			plan.Groups = append(plan.Groups, BatchGroup{Purpose: w.purpose, Kind: kind, Model: w.model, Provider: prov,
				Submit: submitPending, Items: items, Units: units, Ref: openai.BatchRef{API: prov.API, BaseURL: prov.BaseURL,
					Kind: kind, Model: w.model, Count: len(chunk), SubmitKey: key, SubmittedAt: e.nowT().UTC()}})
			g := &plan.Groups[len(plan.Groups)-1]
			save := e.saveBatchPlan
			if !stored {
				save = e.insertBatchPlan // only a new preparation creates the row
			}
			if err := save(&plan); err != nil {
				plan.Groups = plan.Groups[:len(plan.Groups)-1] // nothing was sent
				stopErr = err
				plan.Cut = true
				break
			}
			stored = true
			ref, err := client.SubmitBatch(ctx, kind, w.model, reqs, key)
			switch {
			case err == nil:
				g.Ref, g.Submit, g.State = ref, "", openai.BatchPending
				if g.Ref.SubmitKey == "" {
					g.Ref.SubmitKey = key
				}
				if g.Provider.BaseURL == "" {
					g.Provider.BaseURL, g.Provider.API = strings.TrimRight(ref.BaseURL, "/"), ref.API
				}
				plan.BatchItems += units
				from += len(chunk)
				if err := e.saveAccepted(&plan); err != nil {
					// the batch is running and paid for: it is never cancelled or forgotten. The plan keeps it, the
					// stored intent holds the room, and the next run looks the batch up by its key instead of
					// sending it again; nothing more is submitted
					slog.Error("memory: an accepted batch could not be recorded; it is kept and the room stays held",
						"room", room, "purpose", w.purpose, "batch", ref.ID, "err", err)
					stopErr = fmt.Errorf("memory: the %s batch %s was accepted but the plan could not be stored (%w); the batch is kept, the room stays held, and the next run looks it up instead of sending it again",
						w.purpose, ref.ID, err)
					plan.Cut = true
					break chunks
				}
			case errors.Is(err, openai.ErrBatchUnsupported):
				// the host or the model cannot batch: this chunk and the rest of the group run live
				plan.Groups = plan.Groups[:len(plan.Groups)-1]
				break chunks
			case errors.Is(err, openai.ErrBatchSubmitUnknown):
				// the host may be running it: never sent again, never run live; ReconcilePlan settles it
				var su *openai.SubmitUnknownError
				if errors.As(err, &su) {
					g.Ref.InputFile = su.Ref.InputFile
				}
				g.Submit, g.Error = submitUnknown, clipText(err.Error(), 200)
				slog.Warn("memory: a batch submission's answer was lost; it is looked up, not sent again", "room", room, "purpose", w.purpose, "err", err)
				from += len(chunk)
				plan.BatchItems += units // the other batches are independent: submitting goes on
			default:
				// a refusal (a 4xx, a rate limit, the daily gate, no key): nothing of this runs live
				plan.Groups = plan.Groups[:len(plan.Groups)-1]
				stopErr = fmt.Errorf("%w (%s batch): %v", ErrBatchSubmitRefused, w.purpose, err)
				plan.Cut = true
				break chunks
			}
		}
		if !plan.Cut || stopErr == nil {
			plan.LiveItems += liveAfter(w, from, plan.Cut)
		}
	}
	if ctx.Err() != nil && stopErr == nil {
		rep.StoppedBy = "cancelled" // the caller cancels what was sent
	}
	if len(plan.Groups) == 0 {
		// nothing went to a batch (every chunk unsupported, or refused): no plan holds the room
		if stored {
			if err := e.deletePlan(&plan, "submit"); err != nil && stopErr == nil {
				stopErr = err
			}
		}
		return done(unsent(), stopErr)
	}
	if stopErr != nil && !hasOpenSubmit(&plan) {
		// what was sent is collected; nothing else runs
		plan.State, plan.Drain = BatchPlanDraining, true
		plan.Reason = clipText("submission stopped: "+stopErr.Error()+"; the batches sent are collected, the rest is not sent; prepare again for it", 300)
	}
	if err := e.saveAccepted(&plan); err != nil && stopErr == nil {
		stopErr = err
	}
	progress("batch", 0, plan.requests())
	return done(plan, stopErr)
}

// liveAfter is how many of a group's units run live after its submit loop: those after the last chunk
// sent when the loop ended on ErrBatchUnsupported (or ran out of chunks); none when it was cut (they are
// not sent at all).
func liveAfter(w batchWork, from int, cut bool) int {
	if cut {
		return 0
	}
	return w.units(from)
}

// workset is what a limited preparation selected: the leaves of both pending lists and the messages.
func workset(pending, after []store.PendingLeaf, msgs []store.PendingMessage, msgSelected bool) *BatchWorkset {
	w := &BatchWorkset{MessagesOpen: !msgSelected}
	seen := map[string]bool{}
	for _, list := range [][]store.PendingLeaf{pending, after} {
		for _, p := range list {
			if !seen[p.LeafID] {
				seen[p.LeafID] = true
				w.Leaves = append(w.Leaves, p.LeafID)
			}
		}
	}
	for _, m := range msgs {
		w.Messages = append(w.Messages, m.ObservationID)
	}
	return w
}

// probeSummary summarizes the first leaf live, the way the live pass does, so the summary model's request
// shape is learned before its batch is written (the leaf counts as live work and its cost as spent). It
// returns the leaves left for the batch, or the reason it failed (ErrBatchSubmitRefused; nothing was sent).
func (e *Engine) probeSummary(ctx context.Context, leaves []store.PendingLeaf, plan *BatchPlan) ([]store.PendingLeaf, error) {
	a := e.summaryLLM()
	if a == nil || !a.Configured() {
		return leaves, nil
	}
	p := leaves[0]
	began := time.Now()
	res, err := a.Chat(ctx, e.summaryRequest(p.Body))
	latency := res.Elapsed
	if latency <= 0 {
		latency = time.Since(began)
	}
	logged := err
	if err == nil && strings.TrimSpace(res.Content) == "" {
		logged = emptyReply{res.FinishReason}
	}
	e.recordCall(ctx, "summary", e.Models.Summary, e.CallProviders.Summary, res.CostUSD, fallbackSummaryCall, latency,
		res.PromptTokens, res.CompletionTokens, res.CachedTokens, logged)
	if err != nil {
		slog.Warn("memory: the summary model's first live request failed; no batch is sent", "room", plan.Room, "err", err)
		return nil, fmt.Errorf("%w (summary): the summary model's first live request, which learns its request shape before a batch is written, failed: %v; nothing was sent and nothing runs live; prepare again",
			ErrBatchSubmitRefused, err)
	}
	e.spend("summary", res.CostUSD, fallbackSummaryCall)
	plan.SpentUSD += costOf(res.CostUSD, fallbackSummaryCall)
	plan.LiveItems++
	if logged == nil {
		summary := normalizeSummary(res.Content, p.Body)
		_ = e.Store.PutSummaryText(store.CacheKey(e.Models.Summary, "summary-text", p.Body), p.LeafID, p.Body, summary)
		if e.Store.SaveSummary(p.LeafID, summary, nil, p.Body) == nil {
			plan.Summaries++
		}
	}
	return leaves[1:], nil
}

// leafWork groups leaf embeddings as the live pass does (EmbedBatch per request, the same inputs).
func (e *Engine) leafWork(leavesToEmbed []store.PendingLeaf) []workItem {
	var out []workItem
	for i := 0; i < len(leavesToEmbed); i += e.Settings.EmbedBatch {
		bt := leavesToEmbed[i:min(len(leavesToEmbed), i+e.Settings.EmbedBatch)]
		it := workItem{req: openai.BatchItem{ID: fmt.Sprintf("l%d", len(out)), Input: leafInputs(bt)}}
		for _, p := range bt {
			it.targets = append(it.targets, BatchTarget{ID: p.LeafID, Hash: shortHash(p.Body)})
			it.bytes += len(p.Body) + 64
		}
		it.tokens = openai.EstimateTokens(it.req.Input...)
		out = append(out, it)
	}
	return out
}

// summaryWork is one summary request per leaf, built by summaryRequest like the live pass's.
func (e *Engine) summaryWork(leavesToSummarize []store.PendingLeaf) []workItem {
	out := make([]workItem, 0, len(leavesToSummarize))
	for i, p := range leavesToSummarize {
		req := e.summaryRequest(p.Body)
		size := 512
		for _, m := range req.Messages {
			size += len(m.Content)
		}
		out = append(out, workItem{req: openai.BatchItem{ID: fmt.Sprintf("s%d", i), Chat: &req},
			targets: []BatchTarget{{ID: p.LeafID, Hash: shortHash(p.Body)}}, bytes: size})
	}
	return out
}

// messageWork groups message embeddings as the live pass does.
func (e *Engine) messageWork(msgs []store.PendingMessage) []workItem {
	var out []workItem
	for i := 0; i < len(msgs); i += e.Settings.EmbedBatch {
		bt := msgs[i:min(len(msgs), i+e.Settings.EmbedBatch)]
		it := workItem{req: openai.BatchItem{ID: fmt.Sprintf("m%d", len(out)), Input: messageInputs(bt)}}
		for _, p := range bt {
			it.targets = append(it.targets, BatchTarget{ID: p.ObservationID, Hash: hashPrefix(p.TextHash)})
			it.bytes += len(p.Text) + 64
		}
		it.tokens = openai.EstimateTokens(it.req.Input...)
		out = append(out, it)
	}
	return out
}

// splitWork cuts a group's requests into batches: at most hostMax requests (0 = no host limit),
// batchMaxBytes of request text and, for embeddings, batchMaxEmbedInputs inputs each.
func splitWork(items []workItem, hostMax int, kind openai.BatchKind) [][]workItem {
	var out [][]workItem
	var cur []workItem
	size, inputs := 0, 0
	for _, it := range items {
		n := len(it.req.Input)
		full := hostMax > 0 && len(cur) >= hostMax
		if len(cur) > 0 && (full || size+it.bytes > batchMaxBytes || (kind == openai.BatchEmbed && inputs+n > batchMaxEmbedInputs)) {
			out = append(out, cur)
			cur, size, inputs = nil, 0, 0
		}
		cur = append(cur, it)
		size += it.bytes
		inputs += n
	}
	if len(cur) > 0 {
		out = append(out, cur)
	}
	return out
}

// RunUnbatched is the live pass of a batch preparation that sent nothing (no paid work, no host can batch
// it, or the summary probe took the only summary): IndexRooms on the room within what PrepareBatch
// selected, with the budget less what PrepareBatch already spent (the probe, at the live price), as
// FinishBatch does for a remainder. plan is PrepareBatch's plan without groups; the report carries the
// probe's summary and cost.
func (e *Engine) RunUnbatched(ctx context.Context, room string, opts BatchOptions, plan BatchPlan, progress Progress) (IndexReport, error) {
	rep, err := e.IndexRooms(ctx, IndexOptions{Rooms: []string{room}, BudgetUSD: opts.BudgetUSD, Limit: opts.Limit,
		spent: plan.SpentUSD, workset: plan.Workset}, progress)
	rep.Mode, rep.LiveItems = "batch", plan.LiveItems
	rep.Summaries += plan.Summaries
	rep.SpentUSD += plan.SpentUSD
	return rep, err
}
