package engine

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"masamong/internal/memory/store"
	"masamong/internal/providers/openai"
)

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

// ErrBatchPlanGone means the room's plan was deleted (cancelled, discarded, or the room removed).
var ErrBatchPlanGone = errors.New("memory: batch plan no longer stored")

// ErrBatchPlanReplaced means the room's stored plan is another generation than the caller's (a newer
// preparation): the call changed nothing. It is an ErrBatchPlanGone too.
var ErrBatchPlanReplaced = fmt.Errorf("%w: the room has a newer batch plan", ErrBatchPlanGone)

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

// BatchPlanInfo is a stored plan's summary (no plan body).
type BatchPlanInfo struct {
	Room      string `json:"room"`
	State     string `json:"state"`
	Items     int    `json:"items"`
	CreatedAt string `json:"created_at"`
	Reason    string `json:"reason,omitempty"`
}

func newGeneration() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("g%x", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// staleLogged keeps a late call on a replaced plan to one log line per room, generation and kind of call.
// It is bounded: past staleLogMax keys it starts afresh (a repeated line is the worst case).
var staleLogged struct {
	sync.Mutex
	seen map[string]bool
}

const staleLogMax = 256

func logStale(room, gen, what string) {
	key := room + "|" + gen + "|" + what
	staleLogged.Lock()
	seen := staleLogged.seen[key]
	if !seen {
		if staleLogged.seen == nil || len(staleLogged.seen) >= staleLogMax {
			staleLogged.seen = map[string]bool{}
		}
		staleLogged.seen[key] = true
	}
	staleLogged.Unlock()
	if !seen {
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

// planSaveFault lets a test fail plan writes (a full disk, a locked database) after the first one.
var planSaveFault func(p *BatchPlan) error

func planRecord(p *BatchPlan) (store.BatchPlanRecord, error) {
	if p.Generation == "" {
		return store.BatchPlanRecord{}, errors.New("memory: a batch plan without a generation is not stored")
	}
	raw, err := json.Marshal(p)
	return store.BatchPlanRecord{State: p.State, Items: p.BatchItems, CreatedAt: p.CreatedAt.UTC().Format(time.RFC3339), Plan: raw}, err
}

// planErr maps the store's compare-and-set refusals: a plan of another generation is ErrBatchPlanReplaced,
// no plan at all ErrBatchPlanGone.
func planErr(err error) error {
	switch {
	case errors.Is(err, store.ErrBatchPlanMissing):
		return ErrBatchPlanGone
	case errors.Is(err, store.ErrBatchPlanStale), errors.Is(err, store.ErrBatchPlanExists):
		return ErrBatchPlanReplaced
	}
	return err
}

// insertBatchPlan stores a new preparation's plan; the room must have none.
func (e *Engine) insertBatchPlan(p *BatchPlan) error {
	rec, err := planRecord(p)
	if err != nil {
		return err
	}
	return planErr(e.Store.InsertBatchPlan(p.Room, p.Generation, rec))
}

// saveBatchPlan writes p over the stored plan of its generation. A plan replaced meanwhile is left alone
// (ErrBatchPlanReplaced), and one deleted meanwhile (discarded, finished) is not stored again
// (ErrBatchPlanGone): only insertBatchPlan creates a plan.
func (e *Engine) saveBatchPlan(p *BatchPlan) error {
	if hook := planSaveFault; hook != nil {
		if err := hook(p); err != nil {
			return err
		}
	}
	rec, err := planRecord(p)
	if err != nil {
		return err
	}
	return planErr(e.Store.SaveBatchPlan(p.Room, p.Generation, rec))
}

// saveAccepted stores a plan right after a batch was accepted. The batch is running and paid for, so a
// failed write is tried again a few times (a locked or briefly full database); a plan that was replaced or
// deleted meanwhile is not.
func (e *Engine) saveAccepted(p *BatchPlan) error {
	delay := batchSaveRetry
	var err error
	for try := 0; try < batchSaveTries; try++ {
		if try > 0 {
			time.Sleep(delay)
			delay *= 2
		}
		if err = e.saveBatchPlan(p); err == nil || errors.Is(err, ErrBatchPlanGone) {
			return err
		}
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

func hasOpenSubmit(p *BatchPlan) bool {
	for _, g := range p.Groups {
		if g.Submit != "" {
			return true
		}
	}
	return false
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
