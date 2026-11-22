package engine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"masamong/internal/errclass"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
)

// ErrBatchResultsUnreadable: a finished batch's results could not be read (after retries, or for a reason
// retrying does not fix). The plan stays and the results stay at the host.
var ErrBatchResultsUnreadable = errors.New("memory: batch results could not be read")

// errRemainderStopped: the live remainder stopped before it was done (cancelled, the daemon stopping):
// the applied plan stays so the next run finishes it.
var errRemainderStopped = errors.New("memory: the live remainder of a batch stopped early")

// BatchApplied counts what applying results did.
type BatchApplied struct {
	Saved, Stale, Failed int
	SpentUSD             float64
}

func (a *BatchApplied) add(b BatchApplied) {
	a.Saved += b.Saved
	a.Stale += b.Stale
	a.Failed += b.Failed
	a.SpentUSD += b.SpentUSD
}

// applyCache keeps the room's current pending leaves and messages across the groups of one ApplyBatch:
// the groups cover different leaves and messages, and every save checks the text again.
type applyCache struct {
	leaves, summaries map[string]string // leaf id → current body
	messages          map[string]store.PendingMessage
	messageSpace      string
}

// applyAccount collects what one Apply costs: the spend is written with the plan's checkpoint, the call
// records after it, so a retried apply counts neither twice.
type applyAccount struct {
	spends map[string]*store.BatchSpend
	calls  []CallInfo
}

func (a *applyAccount) spend(kind string, cost *float64, fallback float64) {
	if a.spends == nil {
		a.spends = map[string]*store.BatchSpend{}
	}
	sp := a.spends[kind]
	if sp == nil {
		sp = &store.BatchSpend{Kind: kind}
		a.spends[kind] = sp
	}
	sp.USD += costOf(cost, fallback)
	sp.Calls++
}

func (a *applyAccount) call(e *Engine, role, model, provider string, cost *float64, fallback float64,
	latency time.Duration, promptTokens, completionTokens, cachedTokens int, err error) {
	if e.OnCall == nil {
		return
	}
	v := fallback
	if cost != nil {
		v = *cost
	}
	a.calls = append(a.calls, CallInfo{Role: role, Model: model, Provider: provider, CostUSD: v, Latency: latency,
		Class: errclass.Of(err), PromptTokens: promptTokens, CompletionTokens: completionTokens, CachedTokens: cachedTokens})
}

func (a *applyAccount) list() []store.BatchSpend {
	out := make([]store.BatchSpend, 0, len(a.spends))
	for _, k := range []string{"embed", "summary"} {
		if sp := a.spends[k]; sp != nil {
			out = append(out, *sp)
		}
	}
	return out
}

// resultsError is a batch whose results could not be read; transient ones are read again later.
type resultsError struct {
	purpose   string
	transient bool
	err       error
}

func (r *resultsError) Error() string {
	return fmt.Sprintf("%v (%s batch): %v", ErrBatchResultsUnreadable, r.purpose, r.err)
}

func (r *resultsError) Unwrap() []error { return []error{ErrBatchResultsUnreadable, r.err} }

// transientResultErr is a result read that may work later: a lost connection, a timeout, a rate limit,
// a server error, results not there yet. A refused key, a missing file or an unreadable answer are not.
func transientResultErr(err error) bool {
	var pe *providers.Error
	if errors.As(err, &pe) {
		switch pe.Kind {
		case "timeout", "connection":
			return true
		case "status":
			return pe.Status == 429 || pe.Status >= 500
		}
	}
	var ae *openai.APIError
	if errors.As(err, &ae) {
		return ae.Status == 429 || ae.Status >= 500
	}
	return errors.Is(err, openai.ErrBatchNotFinal)
}

// ApplyBatch reads the results of every ended, unapplied batch of the plan and applies them. A batch the
// host reports failed, or that was given up, has none: its items stay pending for the live remainder.
// Results that cannot be read are an error (a *resultsError): the batch stays unapplied and its results
// stay at the host, so a later call reads them (CollectBatch retries).
func (e *Engine) ApplyBatch(ctx context.Context, plan *BatchPlan) (BatchApplied, error) {
	var total BatchApplied
	cache := &applyCache{}
	for i := range plan.Groups {
		g := plan.Groups[i]
		if g.Applied || !g.State.Final() || g.Submit != "" {
			continue
		}
		var outcomes []openai.BatchOutcome
		if !g.Abandoned && g.State != openai.BatchFailed {
			client, err := e.groupBatcher(&g)
			if err != nil {
				return total, err
			}
			outcomes, err = client.BatchResults(ctx, g.Ref)
			if err != nil {
				if ctx.Err() != nil {
					return total, ctx.Err()
				}
				return total, &resultsError{purpose: g.Purpose, transient: transientResultErr(err), err: err}
			}
		}
		a, err := e.apply(ctx, plan, i, outcomes, cache)
		total.add(a)
		if err != nil {
			return total, err
		}
	}
	return total, nil
}

// CollectBatch is ApplyBatch until every ended batch is applied, reading results that fail for a
// transient reason again with a backoff (from batchResultRetry to batchPollMax) for up to
// batchResultGiveUp. Another error ends it at once; the plan stays as it is, resumable.
func (e *Engine) CollectBatch(ctx context.Context, plan *BatchPlan) (BatchApplied, error) {
	var total BatchApplied
	delay := batchResultRetry
	start := time.Now()
	for {
		a, err := e.ApplyBatch(ctx, plan)
		total.add(a)
		var re *resultsError
		if err == nil || !errors.As(err, &re) || !re.transient {
			return total, err
		}
		if time.Since(start) > batchResultGiveUp {
			return total, fmt.Errorf("%w; the plan and the results at the host stay, and the next run reads them", err)
		}
		slog.Warn("memory: batch results not readable yet; retrying", "room", plan.Room, "purpose", re.purpose, "in", delay, "err", re.err)
		if err := sleepCtx(ctx, delay); err != nil {
			return total, err
		}
		delay = min(2*delay, batchPollMax)
	}
}

// Apply saves one batch's outcomes the way the live pass saves its results (fitted, normalized float16
// leaf vectors; normalized summaries, cached by text, saved without their vector, which the live
// remainder embeds; quantized message vectors). The results, the counts of what was saved, the group's
// applied mark and the outcomes' cost are written in one transaction (store.CommitBatchApply), so a crash
// before the commit leaves nothing of it and applying the batch again saves and counts it exactly once. A
// result whose leaf or message changed or is no longer pending is skipped (stale); an item with no result
// or an error counts as failed and stays pending. A plan of another generation than the stored one, or a
// group that is not the stored group, changes nothing (ErrBatchPlanReplaced). It holds indexMu.
func (e *Engine) Apply(ctx context.Context, plan *BatchPlan, group int, outcomes []openai.BatchOutcome) (BatchApplied, error) {
	return e.apply(ctx, plan, group, outcomes, &applyCache{})
}

// beforeApplyCommit lets a test fail the apply after its results are written, inside the transaction that
// would commit them with the checkpoint (a crash before the commit).
var beforeApplyCommit func() error

func (e *Engine) apply(ctx context.Context, plan *BatchPlan, group int, outcomes []openai.BatchOutcome, cache *applyCache) (BatchApplied, error) {
	e.indexMu.Lock()
	defer e.indexMu.Unlock()
	var out BatchApplied
	if e.retired.Load() {
		return out, errRetired
	}
	stored, err := e.storedCurrent(plan, "apply")
	if err != nil {
		return out, err
	}
	if group < 0 || group >= len(stored.Groups) || group >= len(plan.Groups) {
		return out, fmt.Errorf("memory: batch group %d out of range", group)
	}
	sg, pg := stored.Groups[group], plan.Groups[group]
	if sg.Ref.ID == "" || sg.Ref.ID != pg.Ref.ID || sg.Purpose != pg.Purpose || sg.Kind != pg.Kind || sg.Submit != "" {
		logStale(plan.Room, plan.Generation, "apply of another group")
		return out, ErrBatchPlanReplaced
	}
	mergePolled(&stored, plan)
	g := &stored.Groups[group]
	if g.Applied {
		*plan = stored
		return out, nil
	}
	byID := make(map[string]openai.BatchOutcome, len(outcomes))
	for _, o := range outcomes {
		if _, dup := byID[o.ID]; !dup {
			byID[o.ID] = o
		}
	}
	var client BatchClient
	if len(outcomes) > 0 {
		client, _ = e.groupBatcher(g)
	}
	acct := &applyAccount{}
	var w store.BatchWrites
	switch g.Purpose {
	case purposeLeaf:
		out, err = e.applyLeaves(ctx, &stored, g, byID, cache, client, acct, &w)
	case purposeSummary:
		out, err = e.applySummaries(ctx, &stored, g, byID, cache, client, acct, &w)
	case purposeMessage:
		out, err = e.applyMessages(ctx, &stored, g, byID, cache, client, acct, &w)
	default:
		out.Failed = g.Units
	}
	if err != nil {
		return out, err
	}
	planned := len(w.Leaves) + len(w.Summaries) + len(w.Messages)
	before := stored // the counters before this group (the closure sets them from what was written)
	err = e.Store.CommitBatchApply(stored.Room, stored.Generation, w, acct.list(), func(got store.BatchWritten) (store.BatchPlanRecord, error) {
		saved := got.Leaves + got.Summaries + got.Messages
		stored.LeafVectors = before.LeafVectors + got.Leaves
		stored.Summaries = before.Summaries + got.Summaries
		stored.MessageVectors = before.MessageVectors + got.Messages
		stored.SpentUSD = before.SpentUSD + out.SpentUSD
		stored.BatchFailed = before.BatchFailed + out.Failed
		stored.BatchStale = before.BatchStale + out.Stale + planned - saved // changed while the transaction waited
		g.Applied = true
		if hook := beforeApplyCommit; hook != nil {
			if err := hook(); err != nil {
				return store.BatchPlanRecord{}, err
			}
		}
		return planRecord(&stored)
	})
	if err != nil {
		g.Applied = false
		return BatchApplied{}, planErr(err)
	}
	saved := stored.LeafVectors - before.LeafVectors + stored.Summaries - before.Summaries + stored.MessageVectors - before.MessageVectors
	out.Saved, out.Stale = saved, out.Stale+planned-saved
	for _, l := range w.Leaves {
		delete(cache.leaves, l.ID)
	}
	for _, sw := range w.Summaries {
		delete(cache.summaries, sw.ID)
	}
	for _, m := range w.Messages {
		delete(cache.messages, m.ObservationID)
	}
	if e.OnCall != nil {
		for _, c := range acct.calls {
			e.OnCall(ctx, c)
		}
	}
	*plan = stored
	return out, nil
}

func costOf(c *float64, fallback float64) float64 {
	if c != nil {
		return *c
	}
	return fallback
}

// embedOutcome checks one embeddings item's outcome and accounts its call and cost; ok is false when it
// has no usable vectors (missing, failed, or not one per input). A cost the host reported neither as a
// cost nor as tokens is priced from the item's estimated tokens at the batch price, as a live call
// estimates its own.
func (e *Engine) embedOutcome(model string, g *BatchGroup, it BatchPlanItem, o openai.BatchOutcome, ok bool, client BatchClient,
	acct *applyAccount, out *BatchApplied) (*openai.EmbedResult, bool) {
	if !ok {
		return nil, false
	}
	if o.Err != nil || o.Embed == nil {
		err := o.Err
		if err == nil {
			err = errors.New("batch item without an embedding")
		}
		acct.call(e, "embed", model, g.Provider.ID, nil, 0, 0, 0, 0, 0, err)
		return nil, false
	}
	res := o.Embed
	inputs := len(it.Targets)
	cost := res.CostUSD
	if cost == nil && client != nil {
		tokens := res.PromptTokens
		if tokens == 0 {
			tokens = it.Tokens
		}
		cost = client.BatchEmbedCost(model, tokens, g.Ref.SubmittedAt)
	}
	discount := 1.0
	if client != nil {
		discount = client.BatchDiscount(openai.BatchEmbed, model)
	}
	fallback := 0.00002 * float64(inputs) * discount
	acct.call(e, "embed", model, g.Provider.ID, cost, fallback, res.Elapsed, res.PromptTokens, 0, 0, nil)
	acct.spend("embed", cost, fallback)
	out.SpentUSD += costOf(cost, fallback)
	if len(res.Vectors) != inputs {
		return nil, false
	}
	return res, true
}

// applyLeaves collects a leaf batch's vectors into w (written by apply's commit); out counts the stale and
// failed results and the cost.
func (e *Engine) applyLeaves(_ context.Context, plan *BatchPlan, g *BatchGroup, byID map[string]openai.BatchOutcome, cache *applyCache,
	client BatchClient, acct *applyAccount, w *store.BatchWrites) (BatchApplied, error) {
	var out BatchApplied
	space := e.EmbedSpace()
	if cache.leaves == nil {
		leafDims, err := e.pendingLeafDims()
		if err != nil {
			return out, err
		}
		pending, err := e.Store.PendingAtWidth([]string{plan.Room}, space, false, 0, leafDims, e.Settings.Recipe.Version())
		if err != nil {
			return out, err
		}
		cache.leaves = map[string]string{}
		for _, p := range pending {
			if p.NeedLeaf {
				cache.leaves[p.LeafID] = p.Body
			}
		}
	}
	sameSpace := plan.Space == space && plan.EmbedModel == e.Models.Embed
	for _, it := range g.Items {
		o, ok := byID[it.ID]
		res, ok := e.embedOutcome(plan.EmbedModel, g, it, o, ok, client, acct, &out)
		if !ok {
			out.Failed += len(it.Targets)
			continue
		}
		for i, t := range it.Targets {
			body, pending := cache.leaves[t.ID]
			if !sameSpace || !pending || shortHash(body) != t.Hash {
				out.Stale++
				continue
			}
			v := e.fitDims(res.Vectors[i])
			e.observeLeafWidth(len(v))
			search.Normalize(v)
			w.Leaves = append(w.Leaves, store.LeafVectorWrite{ID: t.ID, Space: space, Vec: search.EncodeFloat16(v), Body: body})
		}
	}
	return out, nil
}

// applySummaries collects a summary batch's texts into w (written by apply's commit).
func (e *Engine) applySummaries(_ context.Context, plan *BatchPlan, g *BatchGroup, byID map[string]openai.BatchOutcome, cache *applyCache,
	client BatchClient, acct *applyAccount, w *store.BatchWrites) (BatchApplied, error) {
	var out BatchApplied
	if cache.summaries == nil {
		leafDims, err := e.pendingLeafDims()
		if err != nil {
			return out, err
		}
		pending, err := e.Store.PendingAtWidth([]string{plan.Room}, e.EmbedSpace(), true, 0, leafDims, e.Settings.Recipe.Version())
		if err != nil {
			return out, err
		}
		cache.summaries = map[string]string{}
		for _, p := range pending {
			if p.NeedSumm {
				cache.summaries[p.LeafID] = p.Body
			}
		}
	}
	discount := 1.0
	if client != nil {
		discount = client.BatchDiscount(openai.BatchChat, plan.SummaryModel)
	}
	fallback := fallbackSummaryCall * discount
	var refusal error // the first item error, while every item has failed with one
	allRefused := len(g.Items) > 0
	for _, it := range g.Items {
		o, ok := byID[it.ID]
		if !ok || len(it.Targets) != 1 {
			out.Failed += len(it.Targets)
			allRefused = false
			continue
		}
		if o.Err != nil || o.Chat == nil {
			err := o.Err
			if err == nil {
				err = errors.New("batch item without a reply")
			}
			var ae *openai.APIError
			if errors.As(err, &ae) && (ae.Status == 400 || ae.Status == 422) {
				if refusal == nil {
					refusal = err
				}
			} else {
				allRefused = false
			}
			if res := o.Chat; res != nil {
				// an answer that is still an error (the model declined: stop_reason refusal) was billed
				acct.call(e, "summary", plan.SummaryModel, g.Provider.ID, res.CostUSD, 0, res.Elapsed,
					res.PromptTokens, res.CompletionTokens, res.CachedTokens, err)
				acct.spend("summary", res.CostUSD, 0)
				out.SpentUSD += costOf(res.CostUSD, 0)
			} else {
				acct.call(e, "summary", plan.SummaryModel, g.Provider.ID, nil, 0, 0, 0, 0, 0, err)
			}
			out.Failed++
			continue
		}
		allRefused = false
		res := o.Chat
		var logged error
		if strings.TrimSpace(res.Content) == "" {
			logged = emptyReply{res.FinishReason}
		}
		acct.call(e, "summary", plan.SummaryModel, g.Provider.ID, res.CostUSD, fallback, res.Elapsed,
			res.PromptTokens, res.CompletionTokens, res.CachedTokens, logged)
		acct.spend("summary", res.CostUSD, fallback)
		out.SpentUSD += costOf(res.CostUSD, fallback)
		if logged != nil {
			out.Failed++ // no text: the leaf stays unsummarized for the live remainder, as a live empty reply does
			continue
		}
		t := it.Targets[0]
		body, pending := cache.summaries[t.ID]
		if !pending || shortHash(body) != t.Hash {
			out.Stale++ // a changed or erased text: its old summary is not kept anywhere
			continue
		}
		// the vector needs the summary text: the live remainder embeds it (or takes it from the cache)
		w.Summaries = append(w.Summaries, store.SummaryWrite{ID: t.ID, Body: body, Summary: normalizeSummary(res.Content, body),
			CacheKey: store.CacheKey(plan.SummaryModel, "summary-text", body)})
	}
	if allRefused && refusal != nil {
		// every request was refused for its fields: learn the model's shape the way a live retry does, so
		// the remainder and the next batch leave the refused field out
		if sl, ok := client.(shapeLearner); ok && sl.LearnRefusal(e.summaryRequest(""), refusal) {
			slog.Warn("memory: the summary batch was refused for a request field; the field is left out from now on", "room", plan.Room, "err", refusal)
		}
	}
	return out, nil
}

// applyMessages collects a message batch's vectors into w (written by apply's commit; the store keeps a
// vector only if its message still reads as the text it was made from).
func (e *Engine) applyMessages(_ context.Context, plan *BatchPlan, g *BatchGroup, byID map[string]openai.BatchOutcome, cache *applyCache,
	client BatchClient, acct *applyAccount, w *store.BatchWrites) (BatchApplied, error) {
	var out BatchApplied
	dims, _, err := e.messageVectorState()
	if err != nil {
		return out, err
	}
	space := e.messageSpaceAt(dims)
	// a width or model change since the submit makes every result stale (they are another space's); a plan
	// submitted before the width was known takes the width its leaf results set
	current := dims > 0 && space != "" && (plan.MessageSpace == "" || space == plan.MessageSpace) && plan.EmbedModel == e.Models.Embed
	if current && (cache.messages == nil || cache.messageSpace != space) {
		pending, err := e.Store.PendingMessages([]string{plan.Room}, space, 0, e.Settings.Recipe.Version())
		if err != nil {
			return out, err
		}
		cache.messages, cache.messageSpace = make(map[string]store.PendingMessage, len(pending)), space
		for _, p := range pending {
			cache.messages[p.ObservationID] = p
		}
	}
	short := false
	for _, it := range g.Items {
		o, ok := byID[it.ID]
		res, ok := e.embedOutcome(plan.EmbedModel, g, it, o, ok, client, acct, &out)
		if !ok {
			out.Failed += len(it.Targets)
			continue
		}
		for i, t := range it.Targets {
			p, pending := cache.messages[t.ID]
			if !current || !pending || hashPrefix(p.TextHash) != t.Hash {
				out.Stale++
				continue
			}
			v := res.Vectors[i]
			if short || (!e.Models.EmbedTruncatable && len(v) < dims) {
				if !short {
					// as embedMessages: keep the messages pending until the leaf width changes
					e.messageWidthMu.Lock()
					e.messageShortExpected, e.messageShortReturned = dims, len(v)
					e.messageWidthMu.Unlock()
					short = true
				}
				out.Failed++
				continue
			}
			w.Messages = append(w.Messages, store.MessageVector{ObservationID: t.ID, Space: space, TextHash: p.TextHash,
				Vec: search.EncodeQuantizedRow(fitMessageVec(v, dims))})
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// finish, cancel, drain, discard
// ---------------------------------------------------------------------------

// FinishBatch ends a plan whose batches are applied: the room is released (the plan reads applied), the
// live remainder runs through IndexRooms on what the preparation selected with what is left of the
// budget, the hosts' batch files are cleaned up and the plan is deleted. The report is the remainder's
// with the batch's results and cost added, so a batch preparation ends exactly like a live one. A
// remainder that fails, or stops before it is done (cancelled, the daemon stopping), keeps the plan,
// applied, so the next run finishes it; a budget stop ends it normally.
func (e *Engine) FinishBatch(ctx context.Context, plan *BatchPlan, progress Progress) (IndexReport, error) {
	if err := e.markApplied(plan); err != nil {
		return IndexReport{Mode: "batch"}, err
	}
	rep, err := e.IndexRooms(ctx, IndexOptions{Rooms: []string{plan.Room}, BudgetUSD: plan.BudgetUSD, Limit: plan.Limit,
		spent: plan.SpentUSD, workset: plan.Workset}, progress)
	rep.Mode, rep.BatchItems, rep.BatchFailed, rep.LiveItems = "batch", plan.BatchItems, plan.BatchFailed, plan.LiveItems
	if err != nil {
		return rep, err
	}
	if rep.StoppedBy == "cancelled" || ctx.Err() != nil {
		if cause := context.Cause(ctx); cause != nil {
			return rep, cause
		}
		return rep, errRemainderStopped
	}
	rep.LeafVectors += plan.LeafVectors
	rep.Summaries += plan.Summaries
	rep.MessageVectors += plan.MessageVectors
	rep.SpentUSD += plan.SpentUSD
	if rep.Reload == nil && plan.LeafVectors+plan.Summaries+plan.MessageVectors > 0 {
		// the applied results changed the stored index even when the remainder had nothing to do
		reload, err := e.Reload()
		if err != nil && !errors.Is(err, errRetired) {
			return rep, err
		}
		rep.Reload = reload
	}
	if _, err := e.storedCurrent(plan, "finish"); err != nil {
		return rep, err
	}
	e.cleanupPlan(ctx, plan)
	if err := e.deletePlan(plan, "finish"); err != nil {
		return rep, err
	}
	return rep, nil
}
