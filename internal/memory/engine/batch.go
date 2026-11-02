package engine

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"masamong/internal/errclass"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
)

// Batch mode prepares a room through provider batch jobs instead of live calls: about half the price,
// results within about a day. One preparation runs in stages, and only the first and the saves hold
// indexMu, so live indexing of other rooms and Discord auto-update keep running while the provider
// works:
//
//	PrepareBatch   sync the room, the free reuse pass, collect the pending leaf embeddings, summary
//	               chats and message vectors, submit a batch per group the role's host can batch
//	               (split at the host's item limit and a byte size) and store the plan
//	ReconcilePlan  settle a submission whose answer was lost (the host's batch list, by key or file)
//	WaitBatch      poll the batches, backing off from a minute to five; no lock
//	CollectBatch   read each finished batch's results and save them (Apply) the way the live pass does,
//	               retrying a result download that fails for a while
//	FinishBatch    the live remainder: IndexRooms on what the preparation selected, with what is left of
//	               the budget (summary vectors, which need the summary text first, items the host reported
//	               failed or expired, groups the host or model cannot batch), then the hosts' files are
//	               cleaned up and the plan is deleted
//
// A plan has an immutable generation: every write after the first compares it, so a late apply or
// cleanup of an older run, or of an engine a newer configuration replaced, changes nothing of a newer plan
// of the room. A batch is never submitted twice: the intent is stored before the create call, the call is
// never retried, and an answer that was lost leaves the submission "unknown" (the room held) until the
// host's list settles it or the operator discards the plan. Only ErrBatchUnsupported (the host or model
// cannot batch) sends work live; a refused or unsettled submission stops the preparation, which then
// collects what was sent (draining) and runs nothing live. A cancel drains the same way: the room stays
// held until every batch has ended at the host, and the finished results are kept.
//
// The plan is stored per room (store.SaveBatchPlan) before and after every submit, so a restart polls the
// batches it has and never submits them again. While a plan is waiting, the room's pending leaves are
// already paid for: the background index pass skips the room (BatchWaitingRooms) until it is applied.

// BatchClient is the provider batch API the engine uses (internal/providers/openai batch.go). An
// *openai.Adapter satisfies it; tests use a fake.
type BatchClient interface {
	BatchSupport(kind openai.BatchKind) bool
	BatchMaxItems(kind openai.BatchKind) int
	// BatchDiscount is the batch price of model as a share of the live price (1 for none)
	BatchDiscount(kind openai.BatchKind, model string) float64
	// BatchEmbedCost prices an embeddings item the host reported neither a cost nor tokens for
	BatchEmbedCost(model string, tokens int, at time.Time) *float64
	SubmitBatch(ctx context.Context, kind openai.BatchKind, model string, items []openai.BatchItem, key string) (openai.BatchRef, error)
	ReconcileBatch(ctx context.Context, ref openai.BatchRef) (openai.BatchRef, error)
	PollBatch(ctx context.Context, ref openai.BatchRef) (openai.BatchStatus, error)
	BatchResults(ctx context.Context, ref openai.BatchRef) ([]openai.BatchOutcome, error)
	CancelBatch(ctx context.Context, ref openai.BatchRef) error
	CleanupBatch(ctx context.Context, ref openai.BatchRef) error
}

var _ BatchClient = (*openai.Adapter)(nil)

// shapeLearner is a batch client that knows its models' chat request shapes (an *openai.Adapter): a batch
// cannot retry a refused optional field the way a live call does.
type shapeLearner interface {
	ShapeLearned(model string) bool
	LearnRefusal(req openai.ChatRequest, err error) bool
}

var _ shapeLearner = (*openai.Adapter)(nil)

// BatchProvider names the provider a group was submitted to: the config provider id and the address and
// family it had then. Never a key.
type BatchProvider struct {
	ID      string     `json:"id,omitempty"`
	BaseURL string     `json:"base_url,omitempty"`
	API     openai.API `json:"api,omitempty"`
}

// BatchResolver returns the batch client of the provider a group was submitted to (the application
// builds it from the current config by id and refuses one whose address or family changed).
type BatchResolver func(p BatchProvider) (BatchClient, error)

// Plan states (store.BatchPlanRow.State).
const (
	BatchPlanWaiting = "waiting" // results outstanding: the room is held
	BatchPlanApplied = "applied" // results saved; only the live remainder is left
	// BatchPlanDraining: cancelled, or a submission stopped early: the batches sent are collected to their
	// end and nothing runs live; the room is held until then
	BatchPlanDraining = "draining"
	// BatchPlanSubmitUnknown: a submission's outcome could not be settled; the room is held until the host's
	// list settles it or the operator discards the plan
	BatchPlanSubmitUnknown = "submit_unknown"
)

// group submission states (BatchGroup.Submit); "" is a submitted batch
const (
	submitPending = "pending" // recorded before the create call; still unanswered (or the process stopped)
	submitUnknown = "unknown" // the call ended without a clear answer
)

const (
	batchPlanVersion = 1
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
	batchResultRetry   = 10 * time.Second // the first wait before reading results again
	batchResultGiveUp  = 6 * time.Hour    // results that stay unreadable this long end the run (the plan stays)
	batchCleanupWait   = time.Minute
	// a submission is looked up in the host's list no sooner than this after its create call, so a batch
	// the host has just stored is listed by then
	batchReconcileSettle = 2 * time.Minute
)

// ShortBatchWaitsForTest shortens the batch waits (polls, the cancel wait, result retries, the reconcile
// settle time) for a test, also of another package, and returns what restores them. Tests only: call it
// before anything that waits starts, and restore after it stopped.
func ShortBatchWaitsForTest() (restore func()) {
	first, mx, cw, cp, rr, rs := batchPollFirst, batchPollMax, batchCancelWait, batchCancelPoll, batchResultRetry, batchReconcileSettle
	batchPollFirst, batchPollMax, batchCancelWait, batchCancelPoll, batchResultRetry = time.Millisecond, 5*time.Millisecond, 200*time.Millisecond, time.Millisecond, time.Millisecond
	batchReconcileSettle = 0
	return func() {
		batchPollFirst, batchPollMax, batchCancelWait, batchCancelPoll, batchResultRetry, batchReconcileSettle = first, mx, cw, cp, rr, rs
	}
}

// ErrBatchPlanGone means the room's plan was deleted (cancelled, discarded, or the room removed).
var ErrBatchPlanGone = errors.New("memory: batch plan no longer stored")

// ErrBatchPlanReplaced means the room's stored plan is another generation than the caller's (a newer
// preparation): the call changed nothing. It is an ErrBatchPlanGone too.
var ErrBatchPlanReplaced = fmt.Errorf("%w: the room has a newer batch plan", ErrBatchPlanGone)

// ErrBatchSubmitUnknown: a submission's outcome could not be settled (openai.ErrBatchSubmitUnknown). The
// plan stays (the room held) until the host's list settles it or the operator discards it.
var ErrBatchSubmitUnknown = openai.ErrBatchSubmitUnknown

// ErrBatchSubmitRefused: the host refused a batch (a 4xx other than "this model cannot batch", a rate
// limit, the daily gate) or a submission turned out never to have reached it. Nothing of it runs live;
// what was sent before is collected.
var ErrBatchSubmitRefused = errors.New("memory: the provider refused the batch")

// ErrBatchProviderGone: the provider a batch was submitted to is no longer configured as it was (removed,
// another address or family, no key): its batches cannot be polled or read. The plan stays.
var ErrBatchProviderGone = errors.New("memory: the provider the batch was sent to is no longer configured")

// ErrBatchResultsUnreadable: a finished batch's results could not be read (after retries, or for a reason
// retrying does not fix). The plan stays and the results stay at the host.
var ErrBatchResultsUnreadable = errors.New("memory: batch results could not be read")

// ErrBatchDraining: the plan is draining (cancelled, or its submission stopped early); it is only
// collected, never finished live.
var ErrBatchDraining = errors.New("memory: the batch plan is draining")

// errRemainderStopped: the live remainder stopped before it was done (cancelled, the daemon stopping):
// the applied plan stays so the next run finishes it.
var errRemainderStopped = errors.New("memory: the live remainder of a batch stopped early")

// ErrRetired is returned by an engine a newer configuration replaced; the caller retries on the
// current engine (a batch can outlast several).
var ErrRetired = errRetired

// BatchPlan is one room's in-flight batch preparation. It is stored as JSON and holds ids and short text
// hashes only, never message text.
type BatchPlan struct {
	Version int    `json:"version"`
	Room    string `json:"room"`
	// Generation is the plan's immutable id (store writes compare it)
	Generation string `json:"generation"`
	State      string `json:"state"`
	// Reason says why a plan is draining or its submission unknown (operator-facing; no text of the room)
	Reason string `json:"reason,omitempty"`
	Space  string `json:"space"` // the leaf embedding space at submit
	// MessageSpace is the message space at submit. Empty with a message batch: the width was not known yet
	// (a model without MRL, no leaf embedded), and the leaf results this plan applies first set it.
	MessageSpace string  `json:"message_space,omitempty"`
	EmbedModel   string  `json:"embed_model"`
	SummaryModel string  `json:"summary_model,omitempty"`
	BudgetUSD    float64 `json:"budget_usd"`
	Limit        int     `json:"limit,omitempty"`
	// Workset is what the preparation selected under Limit; the live remainder stays within it. Nil
	// without a limit: the remainder is a pass over the room, as a live preparation would be.
	Workset     *BatchWorkset `json:"workset,omitempty"`
	EstimateUSD float64       `json:"estimate_usd"` // the batch-price estimate the submit was checked against
	CreatedAt   time.Time     `json:"created_at"`
	Groups      []BatchGroup  `json:"groups"`
	// Cut: the submission stopped before every group was sent (a refusal, an unknown answer); the plan
	// drains once what was sent is settled
	Cut bool `json:"cut,omitempty"`
	// Drain: the plan is only collected, never finished live (cancelled, or cut); it survives a
	// submit_unknown state in between
	Drain bool `json:"drain,omitempty"`
	// BatchItems are the leaves and messages submitted (each leaf vector, summary and message vector
	// once), LiveItems those left live at submit (the role's host or model cannot batch, the summary
	// probe). They and the counters below are counts only.
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

// BatchWorkset is the leaves (and messages) a limited preparation selected.
type BatchWorkset struct {
	Leaves   []string `json:"leaves"`
	Messages []string `json:"messages,omitempty"`
	// MessagesOpen: the messages were not selected at submit (their width was not known yet); the
	// remainder selects them up to the limit, as a live pass would
	MessagesOpen bool `json:"messages_open,omitempty"`
}

func (w *BatchWorkset) leafSet() map[string]bool {
	m := make(map[string]bool, len(w.Leaves))
	for _, id := range w.Leaves {
		m[id] = true
	}
	return m
}

func (w *BatchWorkset) messageSet() map[string]bool {
	m := make(map[string]bool, len(w.Messages))
	for _, id := range w.Messages {
		m[id] = true
	}
	return m
}

// BatchGroup is one submitted batch.
type BatchGroup struct {
	Purpose string           `json:"purpose"` // leaf | summary | message
	Kind    openai.BatchKind `json:"kind"`
	Model   string           `json:"model"`
	// Provider is where the group was submitted; polls and results go there whatever the role is now
	Provider BatchProvider `json:"provider"`
	// Submit is "" for a submitted batch, "pending" between the stored intent and the create call's answer,
	// "unknown" when that answer was lost. Ref then holds what is known (key, time, input file), no id.
	Submit string          `json:"submit,omitempty"`
	Ref    openai.BatchRef `json:"ref"`
	Items  []BatchPlanItem `json:"items"`
	Units  int             `json:"units"` // leaves or messages the group covers
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
	// Tokens estimates an embeddings request's inputs, for a host that reports no usage
	Tokens int `json:"tokens,omitempty"`
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
	Reason    string `json:"reason,omitempty"`
}

// BatchStatusFunc receives the combined poll state of a plan: the least advanced state of its open
// batches and the requests finished out of all.
type BatchStatusFunc func(state openai.BatchState, done, total int)

// batcher is the batch client of a purpose's current role: an injected one (tests), else the role's
// adapter. Submissions use it; a submitted group's client is groupBatcher's.
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

// roleProvider is the config provider id of a purpose's current role.
func (e *Engine) roleProvider(purpose string) string {
	if purpose == purposeSummary {
		return e.CallProviders.Summary
	}
	return e.CallProviders.Embed
}

// providerOf is the identity of the provider a purpose's batch goes to now.
func (e *Engine) providerOf(purpose string, c BatchClient) BatchProvider {
	p := BatchProvider{ID: e.roleProvider(purpose)}
	if a, ok := c.(*openai.Adapter); ok {
		p.BaseURL, p.API = strings.TrimRight(a.BaseURL, "/"), a.API
	}
	return p
}

// injectedBatcher is a batch client a test put in place of the purpose's role (EmbedBatcher,
// SummaryBatcher); nil when the roles' adapters serve batches.
func (e *Engine) injectedBatcher(purpose string) BatchClient {
	if purpose == purposeSummary {
		if e.SummaryBatcher != nil {
			return e.SummaryBatcher
		}
		if e.SummaryLLM == nil {
			return e.EmbedBatcher
		}
		return nil
	}
	return e.EmbedBatcher
}

// groupBatcher is the client of the provider a group was submitted to: ResolveBatcher's when set (and
// the group names its provider), else the purpose's current one if it is still that provider. Anything
// else is ErrBatchProviderGone. An injected client (tests) serves every group of its purpose.
func (e *Engine) groupBatcher(g *BatchGroup) (BatchClient, error) {
	gone := func(why string) error {
		return fmt.Errorf("%w (provider %q at %s: %s); restore it, or discard the batch (memory.blocks.update discard_batch)",
			ErrBatchProviderGone, g.Provider.ID, g.Provider.BaseURL, why)
	}
	if c := e.injectedBatcher(g.Purpose); c != nil {
		return c, nil
	}
	if e.ResolveBatcher != nil && g.Provider.ID != "" {
		c, err := e.ResolveBatcher(g.Provider)
		if err != nil {
			return nil, gone(err.Error())
		}
		if c == nil {
			return nil, gone("no batch client")
		}
		return c, nil
	}
	c := e.batcher(g.Purpose)
	if c == nil {
		return nil, gone("no batch client for the role")
	}
	if g.Provider.ID != "" && e.roleProvider(g.Purpose) != g.Provider.ID {
		return nil, gone("the role uses another provider now")
	}
	if a, ok := c.(*openai.Adapter); ok && g.Provider.BaseURL != "" &&
		(strings.TrimRight(a.BaseURL, "/") != strings.TrimRight(g.Provider.BaseURL, "/") || a.API != g.Provider.API) {
		return nil, gone("the provider's address or family changed")
	}
	return c, nil
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

// discount is a purpose's batch price share on its current host for model (1 when it cannot batch).
func (e *Engine) discount(purpose, model string) float64 {
	c := e.batcher(purpose)
	if c == nil || !c.BatchSupport(purposeKind(purpose)) {
		return 1
	}
	d := c.BatchDiscount(purposeKind(purpose), model)
	if d <= 0 || d > 1 {
		return 1
	}
	return d
}

// BatchEstimate prices an estimate for batch mode: leaf and message embeddings and summary chats at the
// batch price of the role's host for the model (BatchDiscount; the live price where it cannot batch or
// bills the model normally), everything else (summary vectors) at the live price, in BatchUSD.
func (e *Engine) BatchEstimate(est Estimate) Estimate {
	embedF := e.discount(purposeLeaf, e.Models.Embed)
	chatF := e.discount(purposeSummary, e.Models.Summary)
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

func newGeneration() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("g%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// staleLogged keeps a late call on a replaced plan to one log line per room, generation and kind of call.
var staleLogged sync.Map

func logStale(room, gen, what string) {
	if _, seen := staleLogged.LoadOrStore(room+"|"+gen+"|"+what, true); !seen {
		slog.Info("memory: late batch "+what+" for a replaced plan ignored", "room", room, "generation", gen)
	}
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

// saveBatchPlan writes p over the stored plan of its generation (or stores it when the room has none).
func (e *Engine) saveBatchPlan(p *BatchPlan) error {
	if p.Generation == "" {
		p.Generation = newGeneration()
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return err
	}
	err = e.Store.SaveBatchPlan(p.Room, p.Generation, p.State, p.BatchItems, p.CreatedAt.UTC().Format(time.RFC3339), raw)
	if errors.Is(err, store.ErrBatchPlanStale) || errors.Is(err, store.ErrBatchPlanExists) {
		return ErrBatchPlanReplaced
	}
	return err
}

// deletePlan deletes the stored plan of p's generation; another generation's stays (a late call is a no-op).
func (e *Engine) deletePlan(p *BatchPlan, what string) error {
	deleted, err := e.Store.DeleteBatchPlanGeneration(p.Room, p.Generation)
	if err == nil && !deleted {
		if _, ok, _ := e.LoadBatchPlan(p.Room); ok {
			logStale(p.Room, p.Generation, what)
		}
	}
	return err
}

// storedCurrent loads the room's plan and checks it is p's generation.
func (e *Engine) storedCurrent(p *BatchPlan, what string) (BatchPlan, error) {
	stored, ok, err := e.LoadBatchPlan(p.Room)
	if err != nil {
		return stored, err
	}
	if !ok {
		return stored, ErrBatchPlanGone
	}
	if stored.Generation != p.Generation {
		logStale(p.Room, p.Generation, what)
		return stored, ErrBatchPlanReplaced
	}
	return stored, nil
}

// BatchPlans lists the stored plans.
func (e *Engine) BatchPlans() ([]BatchPlanInfo, error) {
	rows, err := e.Store.BatchPlans()
	if err != nil {
		return nil, err
	}
	out := make([]BatchPlanInfo, 0, len(rows))
	for _, r := range rows {
		info := BatchPlanInfo{Room: r.Room, State: r.State, Items: r.Items, CreatedAt: r.CreatedAt}
		if r.State == BatchPlanDraining || r.State == BatchPlanSubmitUnknown {
			if p, ok, err := e.LoadBatchPlan(r.Room); err == nil && ok {
				info.Reason = p.Reason
			}
		}
		out = append(out, info)
	}
	return out, nil
}

// BatchWaitingRooms are the rooms whose batch results are still outstanding (waiting, draining, or a
// submission not settled). Their pending leaves and messages may be paid for in a batch: a live pass must
// leave them alone until the plan is applied or gone.
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
		Limit: opts.Limit, EstimateUSD: *est.BatchUSD, CreatedAt: time.Now().UTC()}
	if opts.Limit > 0 {
		plan.Workset = workset(pending, after, msgNeed, msgSelected)
	}
	// a summary batch for a model whose request shape is not learned yet: one live summary first, so the
	// batch leaves out the optional fields the model refuses (a batch item cannot be retried without them)
	if len(needSumm) > 0 && e.canBatch(purposeSummary) {
		if sl, ok := e.batcher(purposeSummary).(shapeLearner); ok && !sl.ShapeLearned(e.Models.Summary) {
			needSumm = e.probeSummary(ctx, needSumm, &plan)
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
					Kind: kind, Model: w.model, Count: len(chunk), SubmitKey: key, SubmittedAt: time.Now().UTC()}})
			g := &plan.Groups[len(plan.Groups)-1]
			if err := e.saveBatchPlan(&plan); err != nil {
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
				if err := e.saveBatchPlan(&plan); err != nil {
					// a batch nobody could find again would be paid for and lost: take it back (the stored intent
					// lets a restart look it up if even this fails)
					_ = client.CancelBatch(context.WithoutCancel(ctx), ref)
					_ = client.CleanupBatch(context.WithoutCancel(ctx), ref)
					plan.Groups = plan.Groups[:len(plan.Groups)-1]
					plan.BatchItems -= units
					_ = e.saveBatchPlan(&plan)
					stopErr = err
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
		return done(BatchPlan{LiveItems: plan.LiveItems, Summaries: plan.Summaries, SpentUSD: plan.SpentUSD}, stopErr)
	}
	if stopErr != nil && !hasOpenSubmit(&plan) {
		// what was sent is collected; nothing else runs
		plan.State, plan.Drain = BatchPlanDraining, true
		plan.Reason = clipText("submission stopped: "+stopErr.Error()+"; the batches sent are collected, the rest is not sent; prepare again for it", 300)
	}
	if err := e.saveBatchPlan(&plan); err != nil && stopErr == nil {
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

func hasOpenSubmit(p *BatchPlan) bool {
	for _, g := range p.Groups {
		if g.Submit != "" {
			return true
		}
	}
	return false
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
// returns the leaves left for the batch: all of them when the live call failed (they then run live).
func (e *Engine) probeSummary(ctx context.Context, leaves []store.PendingLeaf, plan *BatchPlan) []store.PendingLeaf {
	a := e.summaryLLM()
	if a == nil || !a.Configured() {
		return leaves
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
		// a live call that fails would fail in the batch too: the summaries are left to the live remainder
		slog.Warn("memory: the summary model's first live request failed; its summaries run live", "room", plan.Room, "err", err)
		plan.LiveItems += len(leaves)
		return nil
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
	return leaves[1:]
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

func (p *BatchPlan) requests() int {
	n := 0
	for _, g := range p.Groups {
		n += len(g.Items)
	}
	return n
}

// ---------------------------------------------------------------------------
// reconcile
// ---------------------------------------------------------------------------

// ReconcilePlan settles the plan's submissions that have no answer (a lost answer, or an intent a restart
// found): each is looked up in its host's batch list no sooner than batchReconcileSettle after its call.
// Found, it is a normal batch. Certainly never created, it is dropped, and the plan (whose submission
// was then cut short) drains: nothing of it runs live. When the host cannot tell, the plan is marked
// submit_unknown, the room stays held and ErrBatchSubmitUnknown says what the operator can do. No lock is
// held while waiting.
func (e *Engine) ReconcilePlan(ctx context.Context, plan *BatchPlan) error {
	if !hasOpenSubmit(plan) {
		return e.cutToDraining(plan, nil)
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
	return e.cutToDraining(plan, nil)
}

// cutToDraining turns a plan whose submission was cut short into a draining one (collected, nothing live),
// or deletes it when nothing was sent; it returns ErrBatchSubmitRefused then. A plan that was not cut is
// only saved.
func (e *Engine) cutToDraining(plan *BatchPlan, cause error) error {
	if !plan.Cut || plan.Drain {
		if len(plan.Groups) == 0 {
			return e.deletePlan(plan, "reconcile")
		}
		return e.saveBatchPlan(plan)
	}
	why := "a batch submission did not reach the host"
	if cause != nil {
		why = cause.Error()
	}
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
// and ErrBatchProviderGone at once when a batch's provider is no longer configured or cannot be polled.
func (e *Engine) WaitBatch(ctx context.Context, plan *BatchPlan, status BatchStatusFunc) error {
	if hasOpenSubmit(plan) {
		if err := e.ReconcilePlan(ctx, plan); err != nil {
			return err
		}
	}
	delay := batchPollFirst
	for {
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
	now := time.Now()
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
// remainder embeds; quantized message vectors), and marks the batch applied in the stored plan in one
// transaction with the outcomes' cost, so applying a batch again (a crash before the checkpoint, another
// run) never counts its cost twice. A result whose leaf or message changed or is no longer pending is
// skipped (stale); an item with no result or an error counts as failed and stays pending. A plan of
// another generation than the stored one, or a group that is not the stored group, changes nothing
// (ErrBatchPlanReplaced). It holds indexMu.
func (e *Engine) Apply(ctx context.Context, plan *BatchPlan, group int, outcomes []openai.BatchOutcome) (BatchApplied, error) {
	return e.apply(ctx, plan, group, outcomes, &applyCache{})
}

// beforeApplyCommit lets a test fail the checkpoint after the results are written (a crash).
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
	switch g.Purpose {
	case purposeLeaf:
		out, err = e.applyLeaves(ctx, &stored, g, byID, cache, client, acct)
		stored.LeafVectors += out.Saved
	case purposeSummary:
		out, err = e.applySummaries(ctx, &stored, g, byID, cache, client, acct)
		stored.Summaries += out.Saved
	case purposeMessage:
		out, err = e.applyMessages(ctx, &stored, g, byID, cache, client, acct)
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
	if hook := beforeApplyCommit; hook != nil {
		if err := hook(); err != nil {
			return out, err
		}
	}
	raw, err := json.Marshal(&stored)
	if err != nil {
		return out, err
	}
	if err := e.Store.CommitBatchApply(stored.Room, stored.Generation, stored.State, stored.BatchItems,
		stored.CreatedAt.UTC().Format(time.RFC3339), raw, acct.list()); err != nil {
		if errors.Is(err, store.ErrBatchPlanStale) {
			return out, ErrBatchPlanReplaced
		}
		return out, err
	}
	if e.OnCall != nil {
		for _, c := range acct.calls {
			e.OnCall(ctx, c)
		}
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

func (e *Engine) applyLeaves(_ context.Context, plan *BatchPlan, g *BatchGroup, byID map[string]openai.BatchOutcome, cache *applyCache,
	client BatchClient, acct *applyAccount) (BatchApplied, error) {
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

func (e *Engine) applySummaries(_ context.Context, plan *BatchPlan, g *BatchGroup, byID map[string]openai.BatchOutcome, cache *applyCache,
	client BatchClient, acct *applyAccount) (BatchApplied, error) {
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
			acct.call(e, "summary", plan.SummaryModel, g.Provider.ID, nil, 0, 0, 0, 0, 0, err)
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
	if allRefused && refusal != nil {
		// every request was refused for its fields: learn the model's shape the way a live retry does, so
		// the remainder and the next batch leave the refused field out
		if sl, ok := client.(shapeLearner); ok && sl.LearnRefusal(e.summaryRequest(""), refusal) {
			slog.Warn("memory: the summary batch was refused for a request field; the field is left out from now on", "room", plan.Room, "err", refusal)
		}
	}
	return out, nil
}

func (e *Engine) applyMessages(_ context.Context, plan *BatchPlan, g *BatchGroup, byID map[string]openai.BatchOutcome, cache *applyCache,
	client BatchClient, acct *applyAccount) (BatchApplied, error) {
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

// markApplied releases the room: from now on its pending work is the live remainder's. A draining plan,
// or one with a submission not settled, is not released.
func (e *Engine) markApplied(plan *BatchPlan) error {
	e.indexMu.Lock()
	defer e.indexMu.Unlock()
	stored, err := e.storedCurrent(plan, "finish")
	if err != nil {
		return err
	}
	switch {
	case stored.State == BatchPlanDraining || stored.Drain:
		return fmt.Errorf("%w: %s", ErrBatchDraining, stored.Reason)
	case stored.State == BatchPlanSubmitUnknown || hasOpenSubmit(&stored):
		return fmt.Errorf("%w: %s", ErrBatchSubmitUnknown, stored.Reason)
	case !stored.settled():
		return errors.New("memory: the batch plan has results not applied yet")
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
		if err := e.ReconcilePlan(ctx, plan); err != nil && !errors.Is(err, ErrBatchSubmitRefused) {
			return total, err
		}
		if len(plan.Groups) == 0 {
			return total, nil // nothing reached a host; the plan is gone
		}
	}
	if err := e.WaitBatch(ctx, plan, nil); err != nil {
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
