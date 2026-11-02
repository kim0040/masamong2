package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
	"masamong/internal/providers/openai"
)

// Batch mode prepares a room through provider batch jobs instead of live calls: about half the price,
// results within about a day. One preparation runs in stages, and only the first and the saves hold
// indexMu, so live indexing of other rooms and Discord auto-update keep running while the provider
// works:
//
//	PrepareBatch  sync the room, the free reuse pass, collect the pending leaf embeddings, summary
//	              chats and message vectors, submit a batch per group the role's host can batch
//	              (split at the host's item limit and a byte size) and store the plan
//	WaitBatch     poll the batches, backing off from a minute to five; no lock
//	ApplyBatch    read each finished batch's results and save them (Apply) the way the live pass does
//	FinishBatch   the live remainder: IndexRooms on the room with what is left of the budget (summary
//	              vectors, which need the summary text first, failed or expired items, groups the host
//	              could not batch), then CleanupBatch and the plan is deleted
//
// The plan is stored per room (store.SaveBatchPlan) after every submit, so a restart polls the batches
// it has and never submits them again. While a plan is waiting, the room's pending leaves are already
// paid for: the background index pass skips the room (BatchWaitingRooms) until the results are applied.

// BatchClient is the provider batch API the engine uses (internal/providers/openai batch.go). An
// *openai.Adapter satisfies it; tests use a fake.
type BatchClient interface {
	BatchSupport(kind openai.BatchKind) bool
	BatchMaxItems(kind openai.BatchKind) int
	SubmitBatch(ctx context.Context, kind openai.BatchKind, model string, items []openai.BatchItem) (openai.BatchRef, error)
	PollBatch(ctx context.Context, ref openai.BatchRef) (openai.BatchStatus, error)
	BatchResults(ctx context.Context, ref openai.BatchRef) ([]openai.BatchOutcome, error)
	CancelBatch(ctx context.Context, ref openai.BatchRef) error
	CleanupBatch(ctx context.Context, ref openai.BatchRef) error
}

var _ BatchClient = (*openai.Adapter)(nil)

// Plan states (store.BatchPlanRow.State).
const (
	BatchPlanWaiting = "waiting" // results outstanding: the room is held
	BatchPlanApplied = "applied" // results saved; only the live remainder is left
)

const (
	batchPlanVersion = 1
	// batchDiscount is every batching host's price against live (half, for all of them now). The provider
	// API names no per-host discount yet; outcome costs are the host's own.
	batchDiscount = 0.5
	// one batch carries at most this much request text, and an embeddings batch at most this many inputs,
	// whatever the host allows: a smaller batch is uploaded, read back and applied in pieces that fit in
	// memory (4,096 vectors of 4,096 dimensions are 64 MiB of floats)
	batchMaxBytes       = 16 << 20
	batchMaxEmbedInputs = 4096
	batchHashLen        = 16 // hex characters of SHA-256 kept per input to tell a changed text

	purposeLeaf    = "leaf"    // leaf vectors (embed role)
	purposeSummary = "summary" // summary chats (summary role)
	purposeMessage = "message" // message vectors (embed role)
)

// Poll pacing and the bounds of the waits. Variables so tests can shorten them.
var (
	batchPollFirst     = time.Minute
	batchPollMax       = 5 * time.Minute
	batchGiveUp        = 97 * time.Hour // without a host deadline: past the longest one (96 h) the wait ends
	batchDeadlineSlack = time.Hour      // polled past the host's own deadline: the batch is given up
	batchCancelWait    = 2 * time.Minute
	batchCancelPoll    = 10 * time.Second
	batchResultTries   = 3
	batchResultRetry   = 10 * time.Second
	batchCleanupWait   = time.Minute
)

// ErrBatchPlanGone means the room's plan was deleted (cancelled, or the room removed) by someone else.
var ErrBatchPlanGone = errors.New("memory: batch plan no longer stored")

// ErrRetired is returned by an engine a newer configuration replaced; the caller retries on the
// current engine (a batch can outlast several).
var ErrRetired = errRetired

// BatchPlan is one room's in-flight batch preparation. It is stored as JSON and holds ids and short text
// hashes only, never message text.
type BatchPlan struct {
	Version int    `json:"version"`
	Room    string `json:"room"`
	State   string `json:"state"`
	Space   string `json:"space"` // the leaf embedding space at submit
	// MessageSpace is the message space at submit. Empty with a message batch: the width was not known yet
	// (a model without MRL, no leaf embedded), and the leaf results this plan applies first set it.
	MessageSpace string       `json:"message_space,omitempty"`
	EmbedModel   string       `json:"embed_model"`
	SummaryModel string       `json:"summary_model,omitempty"`
	BudgetUSD    float64      `json:"budget_usd"`
	Limit        int          `json:"limit,omitempty"`
	EstimateUSD  float64      `json:"estimate_usd"` // the batch-price estimate the submit was checked against
	CreatedAt    time.Time    `json:"created_at"`
	Groups       []BatchGroup `json:"groups"`
	// BatchItems are the leaves and messages submitted (each leaf vector, summary and message vector
	// once), LiveItems those left live at submit (the role's host cannot batch, or refused). They and
	// the counters below are counts only.
	BatchItems  int `json:"batch_items"`
	LiveItems   int `json:"live_items"`
	BatchFailed int `json:"batch_failed"` // submitted items that failed, expired or never came back
	BatchStale  int `json:"batch_stale"`  // results whose leaf or message changed meanwhile (not saved)
	// what Apply saved, and what the outcomes cost (provider-reported, or estimated at the batch price)
	LeafVectors    int     `json:"leaf_vectors"`
	Summaries      int     `json:"summaries"`
	MessageVectors int     `json:"message_vectors"`
	SpentUSD       float64 `json:"spent_usd"`
}

// BatchGroup is one submitted batch.
type BatchGroup struct {
	Purpose string           `json:"purpose"` // leaf | summary | message
	Kind    openai.BatchKind `json:"kind"`
	Model   string           `json:"model"`
	Ref     openai.BatchRef  `json:"ref"`
	Items   []BatchPlanItem  `json:"items"`
	Units   int              `json:"units"` // leaves or messages the group covers
	// the latest poll; saved with the plan's next write
	State     openai.BatchState `json:"state,omitempty"`
	Error     string            `json:"error,omitempty"`
	ExpiresAt time.Time         `json:"expires_at,omitempty"`
	Abandoned bool              `json:"abandoned,omitempty"` // given up locally (unpollable, past its deadline)
	Applied   bool              `json:"applied,omitempty"`
	done      int               // requests finished or failed, from the latest poll
	total     int
}

// BatchPlanItem is one request of a batch: its custom_id and the leaves or messages it carries, in input
// order (one leaf for a summary).
type BatchPlanItem struct {
	ID      string        `json:"id"`
	Targets []BatchTarget `json:"targets"`
}

// BatchTarget is a leaf (id and body hash) or a message (observation id and text hash).
type BatchTarget struct {
	ID   string `json:"id"`
	Hash string `json:"hash"`
}

// BatchOptions bound a batch preparation.
type BatchOptions struct {
	BudgetUSD float64 // the whole preparation's budget (batch, then the live remainder); 0 = no limit
	Limit     int     // max leaves (and messages) to process (0 = all pending)
}

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

// BatchPlanInfo is a stored plan's summary (no plan body).
type BatchPlanInfo struct {
	Room      string `json:"room"`
	State     string `json:"state"`
	Items     int    `json:"items"`
	CreatedAt string `json:"created_at"`
}

// BatchStatusFunc receives the combined poll state of a plan: the least advanced state of its open
// batches and the requests finished out of all.
type BatchStatusFunc func(state openai.BatchState, done, total int)

// batcher is the batch client of a purpose's role: an injected one (tests), else the role's adapter.
func (e *Engine) batcher(purpose string) BatchClient {
	if purpose == purposeSummary {
		if e.SummaryBatcher != nil {
			return e.SummaryBatcher
		}
		if e.SummaryLLM == nil && e.EmbedBatcher != nil {
			return e.EmbedBatcher // the embed role's provider serves summaries too
		}
		if a := e.summaryLLM(); a != nil && a.Configured() {
			return a
		}
		return nil
	}
	if e.EmbedBatcher != nil {
		return e.EmbedBatcher
	}
	if e.LLM != nil && e.LLM.Configured() {
		return e.LLM
	}
	return nil
}

func purposeKind(purpose string) openai.BatchKind {
	if purpose == purposeSummary {
		return openai.BatchChat
	}
	return openai.BatchEmbed
}

func (e *Engine) canBatch(purpose string) bool {
	c := e.batcher(purpose)
	return c != nil && c.BatchSupport(purposeKind(purpose))
}

// BatchEstimate prices an estimate for batch mode: leaf and message embeddings and summary chats at the
// batch discount where the role's host can batch them, everything else (summary vectors, a host without
// batches) at the live price, in BatchUSD.
func (e *Engine) BatchEstimate(est Estimate) Estimate {
	embedF, chatF := 1.0, 1.0
	if e.canBatch(purposeLeaf) {
		embedF = batchDiscount
	}
	if e.canBatch(purposeSummary) {
		chatF = batchDiscount
	}
	batchable := est.leafEmbedUSD + est.messageEmbedUSD + est.summaryChatUSD
	v := (est.leafEmbedUSD+est.messageEmbedUSD)*embedF + est.summaryChatUSD*chatF + max(0, est.TotalUSD-batchable)
	est.BatchUSD = &v
	return est
}

func shortHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])[:batchHashLen]
}

func hashPrefix(h string) string {
	if len(h) > batchHashLen {
		return h[:batchHashLen]
	}
	return h
}

// ---------------------------------------------------------------------------
// stored plans
// ---------------------------------------------------------------------------

// LoadBatchPlan returns the room's stored plan. A plan this version cannot read is dropped with a
// warning: nothing could poll it, and it would hold the room forever.
func (e *Engine) LoadBatchPlan(room string) (BatchPlan, bool, error) {
	raw, ok, err := e.Store.BatchPlan(room)
	if err != nil || !ok {
		return BatchPlan{}, false, err
	}
	var p BatchPlan
	if err := json.Unmarshal(raw, &p); err != nil || p.Version != batchPlanVersion || p.Room != room {
		slog.Warn("memory: unreadable batch plan dropped", "room", room, "version", p.Version)
		return BatchPlan{}, false, e.Store.DeleteBatchPlan(room)
	}
	return p, true, nil
}

func (e *Engine) saveBatchPlan(p *BatchPlan) error {
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	return e.Store.SaveBatchPlan(p.Room, p.State, p.BatchItems, p.CreatedAt.UTC().Format(time.RFC3339), raw)
}

// BatchPlans lists the stored plans.
func (e *Engine) BatchPlans() ([]BatchPlanInfo, error) {
	rows, err := e.Store.BatchPlans()
	if err != nil {
		return nil, err
	}
	out := make([]BatchPlanInfo, 0, len(rows))
	for _, r := range rows {
		out = append(out, BatchPlanInfo{Room: r.Room, State: r.State, Items: r.Items, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

// BatchWaitingRooms are the rooms whose batch results are still outstanding. Their pending leaves and
// messages are paid for in a batch: a live pass must leave them alone until the plan is applied.
func (e *Engine) BatchWaitingRooms() (map[string]bool, error) {
	rows, err := e.Store.BatchPlans()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{}
	for _, r := range rows {
		if r.State != BatchPlanApplied {
			out[r.Room] = true
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// prepare and submit
// ---------------------------------------------------------------------------

// workItem is one request to submit with the leaves or messages it covers.
type workItem struct {
	req     openai.BatchItem
	targets []BatchTarget
	bytes   int
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
// (nothing is submitted again): the caller resumes waiting on it. The returned plan has no groups when
// nothing was submitted (no work, a budget stop, every host live); the caller then runs the pass live.
// It holds indexMu, like IndexRooms.
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
	var needSumm []store.PendingLeaf
	if e.Settings.Summaries {
		after, err := e.Store.PendingAtWidth(rooms, space, true, opts.Limit, leafDims, recipe)
		if err != nil {
			return done(BatchPlan{}, err)
		}
		for _, p := range after {
			if _, cached := e.cachedSummaryText(p.Body); p.NeedSumm && !cached {
				needSumm = append(needSumm, p)
			}
		}
	}
	var msgNeed []store.PendingMessage
	msgSpace := ""
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
		}
	}
	est := e.BatchEstimate(e.estimate(needEmbed, pending, msgNeed))
	rep.Estimate = &est
	if e.LLM == nil || !e.LLM.Configured() {
		if len(needEmbed)+len(needSumm)+len(msgNeed) > 0 {
			rep.StoppedBy = "provider_not_configured"
		}
		return done(BatchPlan{}, nil)
	}
	// the whole batch is submitted at once, so the budget is checked against its estimate before anything
	// is sent (a live pass instead stops starting calls once the budget is spent)
	if opts.BudgetUSD > 0 && *est.BatchUSD > opts.BudgetUSD {
		rep.StoppedBy = "budget"
		return done(BatchPlan{}, nil)
	}
	plan := BatchPlan{Version: batchPlanVersion, Room: room, State: BatchPlanWaiting, Space: space, MessageSpace: msgSpace,
		EmbedModel: e.Models.Embed, SummaryModel: e.Models.Summary, BudgetUSD: opts.BudgetUSD, Limit: opts.Limit,
		EstimateUSD: *est.BatchUSD, CreatedAt: time.Now().UTC()}
	works := []batchWork{
		{purpose: purposeLeaf, model: e.Models.Embed, items: e.leafWork(needEmbed)},
		{purpose: purposeSummary, model: e.Models.Summary, items: e.summaryWork(needSumm)},
		{purpose: purposeMessage, model: e.Models.Embed, items: e.messageWork(msgNeed)},
	}
	for _, w := range works {
		if len(w.items) == 0 {
			continue
		}
		client := e.batcher(w.purpose)
		kind := purposeKind(w.purpose)
		if client == nil || !client.BatchSupport(kind) {
			plan.LiveItems += w.units(0)
			continue
		}
		from := 0
		for _, chunk := range splitWork(w.items, client.BatchMaxItems(kind), kind) {
			if ctx.Err() != nil {
				break
			}
			reqs := make([]openai.BatchItem, len(chunk))
			items := make([]BatchPlanItem, len(chunk))
			units := 0
			for i, it := range chunk {
				reqs[i] = it.req
				items[i] = BatchPlanItem{ID: it.req.ID, Targets: it.targets}
				units += len(it.targets)
			}
			ref, err := client.SubmitBatch(ctx, kind, w.model, reqs)
			if err != nil {
				// this chunk and the rest of the group run live; the batches already sent go on
				if !errors.Is(err, openai.ErrBatchUnsupported) && ctx.Err() == nil {
					slog.Warn("memory: batch not submitted; that part runs live", "room", room, "purpose", w.purpose, "err", err)
				}
				break
			}
			plan.Groups = append(plan.Groups, BatchGroup{Purpose: w.purpose, Kind: kind, Model: w.model, Ref: ref, Items: items,
				Units: units, State: openai.BatchPending})
			plan.BatchItems += units
			from += len(chunk)
			if err := e.saveBatchPlan(&plan); err != nil {
				// a batch nobody could find again would be paid for and lost: take it back
				_ = client.CancelBatch(context.WithoutCancel(ctx), ref)
				_ = client.CleanupBatch(context.WithoutCancel(ctx), ref)
				plan.Groups = plan.Groups[:len(plan.Groups)-1]
				plan.BatchItems -= units
				if len(plan.Groups) > 0 {
					_ = e.saveBatchPlan(&plan)
				}
				return done(plan, err)
			}
		}
		plan.LiveItems += w.units(from)
	}
	if ctx.Err() != nil {
		rep.StoppedBy = "cancelled" // the caller cancels what was sent
	}
	if len(plan.Groups) > 0 {
		progress("batch", 0, plan.requests())
	}
	return done(plan, nil)
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

func (p *BatchPlan) requests() int {
	n := 0
	for _, g := range p.Groups {
		n += len(g.Items)
	}
	return n
}

// ---------------------------------------------------------------------------
// wait
// ---------------------------------------------------------------------------

// WaitBatch polls the plan's batches until every one has ended (or was given up: unpollable, or past
// its deadline), every minute at first and backing off to five. It holds no lock and writes nothing:
// the plan is saved when its results are applied, and a restart polls again. It returns ctx's error
// when ctx ends first.
func (e *Engine) WaitBatch(ctx context.Context, plan *BatchPlan, status BatchStatusFunc) error {
	delay := batchPollFirst
	for {
		open := e.pollPlan(ctx, plan)
		if status != nil {
			status(plan.pollState())
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

// pollPlan polls every open batch once and reports whether any is still open.
func (e *Engine) pollPlan(ctx context.Context, plan *BatchPlan) bool {
	open := false
	now := time.Now()
	for i := range plan.Groups {
		g := &plan.Groups[i]
		if g.Applied || g.State.Final() {
			continue
		}
		client := e.batcher(g.Purpose)
		if client == nil {
			g.State, g.Error, g.Abandoned = openai.BatchFailed, "no batch client for the role", true
			continue
		}
		st, err := client.PollBatch(ctx, g.Ref)
		if err != nil {
			if ctx.Err() != nil {
				return true
			}
			if errors.Is(err, openai.ErrBatchUnsupported) || now.After(g.deadline(plan.CreatedAt)) {
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
	return open
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

func clipText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ---------------------------------------------------------------------------
// apply
// ---------------------------------------------------------------------------

// applyCache keeps the room's current pending leaves and messages across the groups of one ApplyBatch:
// the groups cover different leaves and messages, and every save checks the text again.
type applyCache struct {
	leaves, summaries map[string]string // leaf id → current body
	messages          map[string]store.PendingMessage
	messageSpace      string
}

// ApplyBatch reads the results of every ended, unapplied batch of the plan and applies them. Results
// that cannot be read (after a few tries) count as failed: their leaves stay pending for the live
// remainder.
func (e *Engine) ApplyBatch(ctx context.Context, plan *BatchPlan) (BatchApplied, error) {
	var total BatchApplied
	cache := &applyCache{}
	for i := range plan.Groups {
		g := plan.Groups[i]
		if g.Applied || !g.State.Final() {
			continue
		}
		var outcomes []openai.BatchOutcome
		if !g.Abandoned && g.State != openai.BatchFailed {
			var err error
			outcomes, err = e.batchResults(ctx, g)
			if err != nil {
				if ctx.Err() != nil {
					return total, ctx.Err()
				}
				slog.Warn("memory: batch results unreadable; those items run live", "room", plan.Room, "purpose", g.Purpose, "err", err)
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

func (e *Engine) batchResults(ctx context.Context, g BatchGroup) ([]openai.BatchOutcome, error) {
	client := e.batcher(g.Purpose)
	if client == nil {
		return nil, errors.New("no batch client for the role")
	}
	var last error
	for try := 0; try < batchResultTries; try++ {
		if try > 0 {
			if err := sleepCtx(ctx, batchResultRetry); err != nil {
				return nil, err
			}
		}
		out, err := client.BatchResults(ctx, g.Ref)
		if err == nil {
			return out, nil
		}
		if errors.Is(err, openai.ErrBatchUnsupported) || ctx.Err() != nil {
			return nil, err
		}
		last = err
	}
	return nil, last
}

// Apply saves one batch's outcomes the way the live pass saves its results (fitted, normalized float16
// leaf vectors; normalized summaries, cached by text, saved without their vector, which the live
// remainder embeds; quantized message vectors), records each outcome's cost and call, and marks the
// batch applied in the stored plan. A result whose leaf or message changed or is no longer pending is
// skipped (stale); an item with no result or an error counts as failed and stays pending. It holds
// indexMu. Applying a batch twice (another run got there first) saves and records nothing.
func (e *Engine) Apply(ctx context.Context, plan *BatchPlan, group int, outcomes []openai.BatchOutcome) (BatchApplied, error) {
	return e.apply(ctx, plan, group, outcomes, &applyCache{})
}

func (e *Engine) apply(ctx context.Context, plan *BatchPlan, group int, outcomes []openai.BatchOutcome, cache *applyCache) (BatchApplied, error) {
	e.indexMu.Lock()
	defer e.indexMu.Unlock()
	var out BatchApplied
	if e.retired.Load() {
		return out, errRetired
	}
	stored, ok, err := e.LoadBatchPlan(plan.Room)
	if err != nil {
		return out, err
	}
	if !ok {
		return out, ErrBatchPlanGone
	}
	if group < 0 || group >= len(stored.Groups) || group >= len(plan.Groups) {
		return out, fmt.Errorf("memory: batch group %d out of range", group)
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
	switch g.Purpose {
	case purposeLeaf:
		out, err = e.applyLeaves(ctx, &stored, g, byID, cache)
		stored.LeafVectors += out.Saved
	case purposeSummary:
		out, err = e.applySummaries(ctx, &stored, g, byID, cache)
		stored.Summaries += out.Saved
	case purposeMessage:
		out, err = e.applyMessages(ctx, &stored, g, byID, cache)
		stored.MessageVectors += out.Saved
	default:
		out.Failed = g.Units
	}
	if err != nil {
		return out, err
	}
	g.Applied = true
	stored.SpentUSD += out.SpentUSD
	stored.BatchFailed += out.Failed
	stored.BatchStale += out.Stale
	if err := e.saveBatchPlan(&stored); err != nil {
		return out, err
	}
	*plan = stored
	return out, nil
}

// mergePolled copies the caller's latest poll results onto the stored plan (polls are not saved on
// their own).
func mergePolled(dst *BatchPlan, src *BatchPlan) {
	for i := range dst.Groups {
		if i >= len(src.Groups) || dst.Groups[i].Applied || dst.Groups[i].Ref.ID != src.Groups[i].Ref.ID {
			continue
		}
		s := src.Groups[i]
		d := &dst.Groups[i]
		d.State, d.Error, d.ExpiresAt, d.Abandoned, d.done, d.total = s.State, s.Error, s.ExpiresAt, s.Abandoned, s.done, s.total
	}
}

func costOf(c *float64, fallback float64) float64 {
	if c != nil {
		return *c
	}
	return fallback
}

// embedOutcome checks one embeddings item's outcome and records its call and cost; ok is false when it
// has no usable vectors (missing, failed, or not one per input).
func (e *Engine) embedOutcome(ctx context.Context, model string, o openai.BatchOutcome, ok bool, inputs int, out *BatchApplied) (*openai.EmbedResult, bool) {
	if !ok {
		return nil, false
	}
	if o.Err != nil || o.Embed == nil {
		err := o.Err
		if err == nil {
			err = errors.New("batch item without an embedding")
		}
		e.recordCall(ctx, "embed", model, e.CallProviders.Embed, nil, 0, 0, 0, 0, 0, err)
		return nil, false
	}
	res := o.Embed
	fallback := 0.00002 * float64(inputs) * batchDiscount
	e.recordCall(ctx, "embed", model, e.CallProviders.Embed, res.CostUSD, fallback, res.Elapsed, res.PromptTokens, 0, 0, nil)
	e.spend("embed", res.CostUSD, fallback)
	out.SpentUSD += costOf(res.CostUSD, fallback)
	if len(res.Vectors) != inputs {
		return nil, false
	}
	return res, true
}

func (e *Engine) applyLeaves(ctx context.Context, plan *BatchPlan, g *BatchGroup, byID map[string]openai.BatchOutcome, cache *applyCache) (BatchApplied, error) {
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
		res, ok := e.embedOutcome(ctx, plan.EmbedModel, o, ok, len(it.Targets), &out)
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
			if err := e.Store.SaveLeafVector(t.ID, space, search.EncodeFloat16(v), body); err != nil {
				out.Failed++ // stays pending; the live remainder embeds it
				continue
			}
			delete(cache.leaves, t.ID)
			out.Saved++
		}
	}
	return out, nil
}

func (e *Engine) applySummaries(ctx context.Context, plan *BatchPlan, g *BatchGroup, byID map[string]openai.BatchOutcome, cache *applyCache) (BatchApplied, error) {
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
	fallback := fallbackSummaryCall * batchDiscount
	for _, it := range g.Items {
		o, ok := byID[it.ID]
		if !ok || len(it.Targets) != 1 {
			out.Failed += len(it.Targets)
			continue
		}
		if o.Err != nil || o.Chat == nil {
			err := o.Err
			if err == nil {
				err = errors.New("batch item without a reply")
			}
			e.recordCall(ctx, "summary", plan.SummaryModel, e.CallProviders.Summary, nil, 0, 0, 0, 0, 0, err)
			out.Failed++
			continue
		}
		res := o.Chat
		var logged error
		if strings.TrimSpace(res.Content) == "" {
			logged = emptyReply{res.FinishReason}
		}
		e.recordCall(ctx, "summary", plan.SummaryModel, e.CallProviders.Summary, res.CostUSD, fallback, res.Elapsed,
			res.PromptTokens, res.CompletionTokens, res.CachedTokens, logged)
		e.spend("summary", res.CostUSD, fallback)
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
		summary := normalizeSummary(res.Content, body)
		_ = e.Store.PutSummaryText(store.CacheKey(plan.SummaryModel, "summary-text", body), t.ID, body, summary)
		// the vector needs the summary text: the live remainder embeds it (or takes it from the cache)
		if err := e.Store.SaveSummary(t.ID, summary, nil, body); err != nil {
			out.Failed++
			continue
		}
		delete(cache.summaries, t.ID)
		out.Saved++
	}
	return out, nil
}

func (e *Engine) applyMessages(ctx context.Context, plan *BatchPlan, g *BatchGroup, byID map[string]openai.BatchOutcome, cache *applyCache) (BatchApplied, error) {
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
	var vecs []store.MessageVector
	for _, it := range g.Items {
		o, ok := byID[it.ID]
		res, ok := e.embedOutcome(ctx, plan.EmbedModel, o, ok, len(it.Targets), &out)
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
			vecs = append(vecs, store.MessageVector{ObservationID: t.ID, Space: space, TextHash: p.TextHash,
				Vec: search.EncodeQuantizedRow(fitMessageVec(v, dims))})
		}
	}
	for i := 0; i < len(vecs); i += messageSaveChunk {
		chunk := vecs[i:min(len(vecs), i+messageSaveChunk)]
		saved, err := e.Store.SaveMessageVectors(chunk)
		if err != nil {
			out.Failed += len(chunk) // they stay pending for the live remainder
			continue
		}
		out.Saved += saved
		out.Stale += len(chunk) - saved // edited, erased or blocked meanwhile: the store refused them
		for _, v := range chunk {
			delete(cache.messages, v.ObservationID)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// finish, cancel, discard
// ---------------------------------------------------------------------------

// FinishBatch ends a plan whose batches are applied: the room is released (the plan reads applied), the
// live remainder runs through IndexRooms with what is left of the budget, the hosts' batch files are
// cleaned up and the plan is deleted. The report is the remainder's with the batch's results and cost
// added, so a batch preparation ends exactly like a live one. A remainder that fails (an error, not a
// stop) keeps the plan, applied, so the next run finishes it.
func (e *Engine) FinishBatch(ctx context.Context, plan *BatchPlan, progress Progress) (IndexReport, error) {
	if err := e.markApplied(plan); err != nil {
		return IndexReport{Mode: "batch"}, err
	}
	rep, err := e.IndexRooms(ctx, IndexOptions{Rooms: []string{plan.Room}, BudgetUSD: plan.BudgetUSD, Limit: plan.Limit, spent: plan.SpentUSD}, progress)
	rep.Mode, rep.BatchItems, rep.BatchFailed, rep.LiveItems = "batch", plan.BatchItems, plan.BatchFailed, plan.LiveItems
	if err != nil {
		return rep, err
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
	e.cleanupPlan(ctx, plan)
	if err := e.Store.DeleteBatchPlan(plan.Room); err != nil {
		return rep, err
	}
	return rep, nil
}

// markApplied releases the room: from now on its pending work is the live remainder's.
func (e *Engine) markApplied(plan *BatchPlan) error {
	e.indexMu.Lock()
	defer e.indexMu.Unlock()
	stored, ok, err := e.LoadBatchPlan(plan.Room)
	if err != nil {
		return err
	}
	if !ok {
		return ErrBatchPlanGone
	}
	mergePolled(&stored, plan)
	if stored.State != BatchPlanApplied {
		stored.State = BatchPlanApplied
		if err := e.saveBatchPlan(&stored); err != nil {
			return err
		}
	}
	*plan = stored
	return nil
}

// cleanupPlan deletes what the batches left on the hosts (best effort; a short wait of its own, so a
// cancelled run still tidies up).
func (e *Engine) cleanupPlan(ctx context.Context, plan *BatchPlan) {
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), batchCleanupWait)
	defer cancel()
	for _, g := range plan.Groups {
		if client := e.batcher(g.Purpose); client != nil {
			if err := client.CleanupBatch(cctx, g.Ref); err != nil && !errors.Is(err, openai.ErrBatchUnsupported) {
				slog.Warn("memory: batch cleanup failed", "room", plan.Room, "purpose", g.Purpose, "err", err)
			}
		}
	}
}

// CancelPlan stops a plan the operator cancelled: it asks the hosts to cancel the open batches (best
// effort), waits a little for them to end, applies whatever finished, cleans up and deletes the plan.
// ctx should outlive the cancelled job (context.WithoutCancel with a timeout). Nothing runs live.
func (e *Engine) CancelPlan(ctx context.Context, plan *BatchPlan) (BatchApplied, error) {
	for i := range plan.Groups {
		g := &plan.Groups[i]
		if g.Applied || g.State.Final() {
			continue
		}
		if client := e.batcher(g.Purpose); client != nil {
			if err := client.CancelBatch(ctx, g.Ref); err != nil && !errors.Is(err, openai.ErrBatchUnsupported) {
				slog.Warn("memory: batch cancel failed", "room", plan.Room, "purpose", g.Purpose, "err", err)
			}
		}
	}
	deadline := time.Now().Add(batchCancelWait)
	for e.pollPlan(ctx, plan) && time.Now().Before(deadline) {
		if sleepCtx(ctx, batchCancelPoll) != nil {
			break
		}
	}
	applied, err := e.ApplyBatch(ctx, plan)
	e.cleanupPlan(ctx, plan)
	if delErr := e.Store.DeleteBatchPlan(plan.Room); delErr != nil && err == nil {
		err = delErr
	}
	if applied.Saved > 0 {
		if _, rerr := e.Reload(); rerr != nil && !errors.Is(rerr, errRetired) && err == nil {
			err = rerr
		}
	}
	if errors.Is(err, ErrBatchPlanGone) {
		err = nil
	}
	return applied, err
}

// DiscardBatch drops the room's plan without applying it (the room is being deleted): the open batches
// are cancelled and every batch is cleaned up, best effort.
func (e *Engine) DiscardBatch(ctx context.Context, room string) error {
	plan, ok, err := e.LoadBatchPlan(room)
	if err != nil || !ok {
		return err
	}
	for _, g := range plan.Groups {
		if client := e.batcher(g.Purpose); client != nil && !g.Applied {
			_ = client.CancelBatch(ctx, g.Ref)
		}
	}
	e.cleanupPlan(ctx, &plan)
	return e.Store.DeleteBatchPlan(room)
}
