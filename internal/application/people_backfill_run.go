package application

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"masamong/internal/action"
	"masamong/internal/errclass"
	"masamong/internal/errcode"
	"masamong/internal/memory/episodes"
	"masamong/internal/memory/peoplebackfill"
	"masamong/internal/memory/store"
	"masamong/internal/providers"
	"masamong/internal/providers/openai"
	"masamong/internal/storage"
)

// The workers of a people backfill run: the pilot (standard API, both models, answers stored) and the
// run (Message Batches, the chosen model, answers applied). Only an operator's pilot or run action
// starts a worker that sends anything; after a restart resumePeopleBackfill polls the batches of runs an
// operator started and sends nothing new.

// Batch states in a run's batches list.
const (
	pbBatchSubmitting = "submitting" // written before the create call: the host may have it
	pbBatchSubmitted  = "submitted"
	pbBatchUnknown    = "unknown" // the create call ended without an answer; the host keeps no key to find it by
	pbBatchApplied    = "applied"
	pbBatchFailed     = "failed" // never ran (refused at create, failed at the host, or released by the operator)
)

// pbStopFails is how many failed steps in a row stop a run (stopped_by error).
const pbStopFails = 10

// pbCreateTimeout bounds one batch create call, which a shutdown waits for.
const pbCreateTimeout = 2 * time.Minute

// pbPollEvery is how often a run polls its batches, in nanoseconds (0 = one minute; tests shorten it).
var pbPollEvery atomic.Int64

func pbPollInterval() time.Duration {
	if v := pbPollEvery.Load(); v > 0 {
		return time.Duration(v)
	}
	return time.Minute
}

// pbBatch is one Message Batches job of a run (the run's batches column).
type pbBatch struct {
	Key     string           `json:"key"`
	State   string           `json:"state"`
	Model   string           `json:"model"`
	Windows int              `json:"windows"`
	MaxUSD  float64          `json:"max_usd"` // the reserved maximum while it is in flight
	Ref     *openai.BatchRef `json:"ref,omitempty"`
	At      string           `json:"at"`
}

// pbHasInFlight reports whether a run has batches the host may still run.
func pbHasInFlight(r store.BackfillRun) bool {
	for _, b := range pbDecodeBatches(r.Batches) {
		if b.inFlight() {
			return true
		}
	}
	return false
}

func pbDecodeBatches(raw []byte) []pbBatch {
	var out []pbBatch
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

func (b pbBatch) inFlight() bool {
	return b.State == pbBatchSubmitting || b.State == pbBatchSubmitted || b.State == pbBatchUnknown
}

// pbAnswer is one model's pilot answer to one window (the window's result column, by model).
type pbAnswer struct {
	Hash     string                `json:"hash,omitempty"`
	ReadAt   string                `json:"read_at,omitempty"`
	Facts    []episodes.PersonFact `json:"facts,omitempty"`
	Rejected int                   `json:"rejected,omitempty"`
	Refused  bool                  `json:"refused,omitempty"`
	Failed   string                `json:"failed,omitempty"` // the error class; the window is sent again by the run
	Skipped  bool                  `json:"skipped,omitempty"`
	USD      float64               `json:"usd,omitempty"`
	// Intent is the reserved maximum of a send in progress, written before the call: an answer still
	// carrying it after a restart was sent and never settled, and is charged at that maximum.
	Intent float64 `json:"intent,omitempty"`
	// Unknown: the call may have been run and billed with no answer read; it was charged at its maximum.
	Unknown bool `json:"unknown,omitempty"`
}

// pbUnknownClass is the Failed class of a send whose outcome is not known.
const pbUnknownClass = "unknown"

func pbAnswers(raw []byte) map[string]pbAnswer {
	out := map[string]pbAnswer{}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &out)
	}
	return out
}

// ---------------------------------------------------------------------------
// live workers
// ---------------------------------------------------------------------------

// pbLoop is a run's live worker. submit says whether it may send (an operator started or continued
// the run); a worker resumed after a restart only polls. stop is why a worker that may no longer send
// stops once nothing is in flight.
type pbLoop struct {
	submit  atomic.Bool
	requeue atomic.Bool // the operator released unknown submissions (requeue_unknown)
	wake    chan struct{}
	stop    string // written by the worker only
}

// pbLoops are the live workers, by service and run. The lock also orders an operator's action against a
// worker deciding to stop.
var pbLoops = struct {
	sync.Mutex
	m map[*Service]map[string]*pbLoop
}{m: map[*Service]map[string]*pbLoop{}}

// pbActive reports whether a run has a live worker.
func (s *Service) pbActive(runID string) bool {
	pbLoops.Lock()
	defer pbLoops.Unlock()
	return pbLoops.m[s][runID] != nil
}

// pbStartLocked starts a run's worker (pbLoops held). It returns false while the service closes.
func (s *Service) pbStartLocked(runID, name string, submit bool, fn func(context.Context, *pbLoop)) bool {
	l := &pbLoop{wake: make(chan struct{}, 1)}
	l.submit.Store(submit)
	if pbLoops.m[s] == nil {
		pbLoops.m[s] = map[string]*pbLoop{}
	}
	pbLoops.m[s][runID] = l
	ok := s.ownedGoContext(name, context.Background(), func(ctx context.Context) {
		defer s.pbRelease(runID, l)
		fn(ctx, l)
	})
	if !ok {
		delete(pbLoops.m[s], runID)
	}
	return ok
}

func (s *Service) pbRelease(runID string, l *pbLoop) {
	pbLoops.Lock()
	defer pbLoops.Unlock()
	if pbLoops.m[s][runID] == l {
		delete(pbLoops.m[s], runID)
	}
	if len(pbLoops.m[s]) == 0 {
		delete(pbLoops.m, s)
	}
}

// ---------------------------------------------------------------------------
// budget
// ---------------------------------------------------------------------------

// pbMaxIn bounds a prompt's input tokens: a token covers at least one byte of text, so the prompt's UTF-8
// length plus the format's framing is never exceeded, whatever the language or the tokenizer.
func pbMaxIn(bp pbPrompt) int { return len(bp.system) + len(bp.user) + pbFraming }

// pbMaxCost is the most one window's request can be billed: every input token at the cache-write price
// (1.25 of the input price, the highest any input token is billed at) and every output token the request
// allows on the wire (pbWireOut, thinking room included). The budget check reserves it; the settlement
// uses the reported usage, which can never be higher.
func pbMaxCost(bp pbPrompt, model peoplebackfill.Model, mode peoplebackfill.Mode) float64 {
	in := pbMaxIn(bp)
	in += (in + 3) / 4 // the cache-write surcharge, rounded up as pbSettle rounds it
	v, err := peoplebackfill.Settle(peoplebackfill.Usage{InputTokens: in, OutputTokens: pbWireOut}, model, mode)
	if err != nil {
		return 0
	}
	return v
}

// pbSendTracker marks, through the request's context, that a Messages call reached the transport: a
// failure after that may have been billed (pbSendUnknown).
type pbSendTracker struct{ base http.RoundTripper }

type pbSentKey struct{}

func (t pbSendTracker) RoundTrip(r *http.Request) (*http.Response, error) {
	if f, ok := r.Context().Value(pbSentKey{}).(*atomic.Bool); ok && r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/messages") {
		f.Store(true)
	}
	return t.base.RoundTrip(r)
}

// pbSendUnknown reports a failed standard call that may have been run and billed: the request reached the
// transport, and no answer came back (a timeout, a lost connection, a call cut by a shutdown, an answer
// that could not be read). A host that answered with an error status, or a connection never made, ran
// nothing.
func pbSendUnknown(err error, sent bool) bool {
	if err == nil || !sent || errors.Is(err, providers.ErrOffline) {
		return false
	}
	var ae *openai.APIError
	if errors.As(err, &ae) && ae.Status >= 400 {
		return false
	}
	var pe *providers.Error
	if errors.As(err, &pe) && (pe.NotSent || (pe.Kind == "status" && pe.Status >= 400)) {
		return false
	}
	return true
}

// pbBudget is the pilot's running budget: spend settled plus the maxima of the requests in flight.
type pbBudget struct {
	mu       sync.Mutex
	spent    float64
	inflight float64
	cap      float64
	over     bool
	unknown  bool // a send's outcome is not known: nothing more is sent until the operator goes on
}

func (b *pbBudget) reserve(v float64) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.over || !peoplebackfill.CanSubmit(b.spent, b.inflight, v, b.cap) {
		b.over = true
		return false
	}
	b.inflight += v
	return true
}

func (b *pbBudget) settle(reserved, spent float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inflight -= reserved
	b.spent += spent
}

func (b *pbBudget) stopped() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.over || b.unknown
}

func (b *pbBudget) halt() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.unknown = true
}

func (b *pbBudget) halted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.unknown
}

// ---------------------------------------------------------------------------
// pilot
// ---------------------------------------------------------------------------

// pbStartPilot is mode=pilot.
func (s *Service) pbStartPilot(ctx context.Context, req action.Request, db *storage.Store, in map[string]any) action.Result {
	plan, fail := s.pbConfirmedPlan(ctx, req, db, in)
	if fail != nil {
		return *fail
	}
	provider, a, err := s.pbAdapter(s.Doc(), strVal(in, "provider", ""))
	if err != nil {
		return s.pbProviderFail(req, err)
	}
	guild := plan.Params.Guild
	picks := pbPilotPick(plan.Windows)
	var pilot []pbWindow
	for _, w := range plan.Windows {
		if picks[w.ID] && !w.Skipped {
			pilot = append(pilot, w)
		}
	}
	if len(pilot) == 0 {
		return action.Fail(req, errcode.InvalidInput, "people_backfill.no_rooms", s.msg(req, "people_backfill.no_rooms"), map[string]any{"windows": len(plan.Windows)})
	}
	low, high := 0.0, 0.0
	for _, m := range pbPilotModels {
		l, h := pbEstimate(pilot, m, peoplebackfill.ModeStandard)
		low, high = low+l, high+h
	}
	spent, err := store.BackfillGuildSpent(db.Reader(), guild)
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	if spent+high > plan.Params.BudgetUSD {
		return action.Fail(req, errcode.BudgetExceeded, "people_backfill.over_budget", s.msg(req, "people_backfill.over_budget"),
			map[string]any{"budget_usd": plan.Params.BudgetUSD, "spent_usd": usd(spent), "pilot_high_usd": usd(high)})
	}
	pbLoops.Lock()
	defer pbLoops.Unlock()
	runs, err := store.BackfillRuns(db.Reader(), guild)
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	var resume *store.BackfillRun
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	for i := range runs {
		r := runs[i]
		switch {
		case (r.State == store.BackfillPiloting || r.State == store.BackfillSubmitted) && pbLoops.m[s][r.ID] != nil:
			return action.Fail(req, errcode.ConfigConflict, "people_backfill.busy", s.msg(req, "people_backfill.busy"), map[string]any{"run": pbRunJSON(db, r)})
		case r.State == store.BackfillSubmitted || pbHasInFlight(r):
			// sent batches no worker polls: the operator continues that run, never a second one beside it
			return action.Fail(req, errcode.ConfigConflict, "people_backfill.busy", s.msg(req, "people_backfill.busy"), map[string]any{"run": pbRunJSON(db, r)})
		case r.Token == plan.Token && r.PilotDone && (r.State == store.BackfillPiloted || r.State == store.BackfillStopped):
			// this plan's pilot is done: report it again, nothing is sent
			return action.OK(req, map[string]any{"mode": "pilot", "run": pbRunJSON(db, r), "model_calls": 0})
		case r.Token == plan.Token && !r.PilotDone && resume == nil && (r.State == store.BackfillPiloting || r.State == store.BackfillStopped):
			resume = &runs[i] // a pilot of this plan that a restart or the budget stopped
		case r.State == store.BackfillPiloting || r.State == store.BackfillPiloted || r.State == store.BackfillStopped:
			// an older plan's run: superseded, its stored answers dropped
			from := r.State
			r.State, r.UpdatedAt = store.BackfillSuperseded, now
			err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
				if err := store.SaveBackfillRun(tx, r, from); err != nil {
					return err
				}
				return store.ClearBackfillResults(tx, r.ID, now)
			})
			if err != nil && !errors.Is(err, store.ErrBackfillRunState) {
				return s.failErr(req, errcode.IO, "error.storage", err, nil)
			}
		}
	}
	var run store.BackfillRun
	if resume != nil {
		run = *resume
		from := run.State
		run.State, run.StoppedBy, run.UpdatedAt = store.BackfillPiloting, "", now
		if err := db.WithWriterContext(ctx, func(tx *sql.Tx) error { return store.SaveBackfillRun(tx, run, from) }); err != nil {
			return s.failErr(req, errcode.IO, "error.storage", err, nil)
		}
	} else {
		params, _ := json.Marshal(plan.Params)
		run = store.BackfillRun{ID: newID("pb_"), GuildID: guild, State: store.BackfillPiloting, Token: plan.Token, Params: params,
			Provider: provider, BudgetUSD: plan.Params.BudgetUSD, Windows: len(plan.Windows), CreatedAt: now, UpdatedAt: now}
		ws := make([]store.BackfillWindow, 0, len(plan.Windows))
		for _, w := range plan.Windows {
			state := store.WindowQueued
			if w.Skipped {
				state = store.WindowSkipped
			}
			ws = append(ws, store.BackfillWindow{RunID: run.ID, ID: w.ID, Room: w.Room, Seq: w.Seq, Lines: w.Lines, Obs: w.Obs,
				FirstAt: w.First.UTC().Format(time.RFC3339Nano), LastAt: w.Last.UTC().Format(time.RFC3339Nano), State: state, Pilot: picks[w.ID], UpdatedAt: now})
		}
		err := db.WithWriterContext(ctx, func(tx *sql.Tx) error {
			if err := store.InsertBackfillRun(tx, run); err != nil {
				return err
			}
			return store.InsertBackfillWindows(tx, ws)
		})
		if err != nil {
			return s.failErr(req, errcode.IO, "error.storage", err, nil)
		}
	}
	runID := run.ID
	if !s.pbStartLocked(runID, "people-backfill-pilot", true, func(ctx context.Context, l *pbLoop) { s.pbPilot(ctx, runID, a) }) {
		return action.Fail(req, errcode.FeatureDisabled, "people_backfill.unavailable", s.msg(req, "people_backfill.unavailable"), nil)
	}
	s.Log.Info("people backfill: pilot started", "run", runID, "windows", len(pilot), "provider", provider)
	return action.OK(req, map[string]any{"mode": "pilot", "run": pbRunJSON(db, run), "pilot_windows": len(pilot),
		"estimate_usd": map[string]float64{"low": usd(low), "high": usd(high)}})
}

// pbPilot sends each pilot window to both models on the standard API and stores the answers.
func (s *Service) pbPilot(ctx context.Context, runID string, a *openai.Adapter) {
	db, err := s.adminDB()
	if err != nil {
		return
	}
	run, ok, err := store.LoadBackfillRun(db.Reader(), runID)
	if err != nil || !ok || run.State != store.BackfillPiloting {
		return
	}
	if err := s.pbSettleLeftIntents(db, runID); err != nil {
		s.Log.Warn("people backfill: unsettled pilot sends not charged", "run", runID, "err", err)
		return
	}
	ws, err := store.BackfillWindows(db.Reader(), runID, true, store.WindowQueued)
	if err != nil {
		s.Log.Warn("people backfill: pilot windows not read", "run", runID, "err", err)
		return
	}
	type job struct {
		w     store.BackfillWindow
		model peoplebackfill.Model
	}
	var jobs []job
	for _, w := range ws {
		have := pbAnswers(w.Result)
		for _, m := range pbPilotModels {
			if ans, ok := have[string(m)]; !ok || ans.Failed != "" {
				jobs = append(jobs, job{w, m})
			}
		}
	}
	spent, err := store.BackfillGuildSpent(db.Reader(), run.GuildID)
	if err != nil {
		s.Log.Warn("people backfill: spend not read", "run", runID, "err", err)
		return
	}
	budget := &pbBudget{spent: spent, cap: run.BudgetUSD} // the cap is the server's total across its runs
	queue := make(chan job)
	var wg sync.WaitGroup
	for range pbPilotWorkers {
		wg.Go(func() {
			for j := range queue {
				if ctx.Err() != nil || budget.stopped() {
					continue
				}
				s.pbPilotOne(ctx, db, run, a, j.w, j.model, budget)
			}
		})
	}
	for _, j := range jobs {
		queue <- j
	}
	close(queue)
	wg.Wait()
	if ctx.Err() != nil {
		return // still piloting: the next start marks it stopped (restart) and the operator resumes it
	}
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	err = db.WithWriterContext(context.Background(), func(tx *sql.Tx) error {
		cur, ok, err := store.LoadBackfillRun(tx, runID)
		if err != nil || !ok {
			return err
		}
		if budget.stopped() {
			cur.State, cur.StoppedBy = store.BackfillStopped, "budget"
			if budget.halted() {
				cur.StoppedBy = "submit_unknown"
			}
		} else {
			rep, err := pbPilotReport(tx, runID)
			if err != nil {
				return err
			}
			cur.Pilot, _ = json.Marshal(rep)
			cur.State, cur.PilotDone, cur.StoppedBy = store.BackfillPiloted, true, ""
		}
		cur.UpdatedAt = now
		return store.SaveBackfillRun(tx, cur, store.BackfillPiloting)
	})
	if err != nil {
		s.Log.Warn("people backfill: pilot not finished", "run", runID, "err", err)
		return
	}
	s.Log.Info("people backfill: pilot ended", "run", runID, "budget_stop", budget.stopped())
}

// pbPilotOne sends one window to one model and stores the answer (validated facts, counts and cost).
// The send's intent (its reserved maximum) is stored before the call, so a send cut by a crash is charged
// at that maximum when the pilot is resumed, and the call is made once: a send whose outcome is not known
// (it may have been run and billed with no answer read) is charged at its maximum and stops the pilot.
func (s *Service) pbPilotOne(ctx context.Context, db *storage.Store, run store.BackfillRun, a *openai.Adapter, w store.BackfillWindow,
	model peoplebackfill.Model, budget *pbBudget) {
	bp, err := s.pbBuildPrompt(db, run.GuildID, w)
	ans := pbAnswer{}
	spent, held, calls := 0.0, 0.0, 0
	switch {
	case err != nil:
		ans.Failed = string(errclass.Of(err))
	case !bp.ok:
		ans.Skipped = true
	default:
		reserved := pbMaxCost(bp, model, peoplebackfill.ModeStandard)
		if !budget.reserve(reserved) {
			return
		}
		if err := s.pbPilotStore(db, run.ID, w.ID, model, pbAnswer{Hash: bp.hash, ReadAt: bp.readAt, Intent: reserved}, 0, 0, 0); err != nil {
			budget.settle(reserved, 0)
			s.Log.Warn("people backfill: pilot send not recorded, not sent", "run", run.ID, "err", err)
			return
		}
		sent := &atomic.Bool{}
		res, err := a.Chat(context.WithValue(ctx, pbSentKey{}, sent), pbRequest(model, bp))
		ans.Hash, ans.ReadAt = bp.hash, bp.readAt
		switch {
		case err == nil || errors.Is(err, openai.ErrRefused):
			spent, calls = pbSettle(&res, model, peoplebackfill.ModeStandard), 1
		case pbSendUnknown(err, sent.Load()):
			held, calls = reserved, 1 // charged at its maximum: the host may have run it
			ans.Unknown = true
			budget.halt()
		case res.PromptTokens+res.CompletionTokens > 0:
			spent, calls = pbSettle(&res, model, peoplebackfill.ModeStandard), 1 // a failure inside an answer is still billed
		}
		budget.settle(reserved, spent+held)
		ans.USD = spent + held
		switch {
		case errors.Is(err, openai.ErrRefused):
			ans.Refused = true
		case ans.Unknown:
			ans.Failed = pbUnknownClass
		case err != nil:
			ans.Failed = string(errclass.Of(err))
		default:
			ans.Facts, ans.Rejected = bp.p.ParsePeople(res.Content, episodes.PeopleOptions{})
		}
	}
	if err := s.pbPilotStore(db, run.ID, w.ID, model, ans, spent, held, calls); err != nil {
		s.Log.Warn("people backfill: pilot answer not stored", "run", run.ID, "err", err)
	}
}

// pbPilotStore writes one model's answer (or a send's intent) to a pilot window with its spend, in one
// transaction. held is a send charged at its maximum: it counts against the cap (the run's spend) but is
// not reported usage (memory_spend).
func (s *Service) pbPilotStore(db *storage.Store, runID, windowID string, model peoplebackfill.Model, ans pbAnswer, spent, held float64, calls int) error {
	now := s.Clock.Now().UTC()
	return db.WithWriterContext(context.Background(), func(tx *sql.Tx) error {
		cur, ok, err := store.LoadBackfillWindow(tx, runID, windowID)
		if err != nil || !ok {
			return err
		}
		have := pbAnswers(cur.Result)
		have[string(model)] = ans
		cur.Result, _ = json.Marshal(have)
		cur.CostUSD += spent + held
		cur.UpdatedAt = now.Format(time.RFC3339)
		if err := pbSettleTx(tx, runID, cur, spent, calls, now); err != nil {
			return err
		}
		if held > 0 {
			return store.AddBackfillRunSpend(tx, runID, held, now.Format(time.RFC3339))
		}
		return nil
	})
}

// pbSettleLeftIntents charges the pilot sends a crash or a restart cut between their intent and their
// answer at their reserved maximum (the host may have run them) and marks them unknown, so the pilot asks
// them again only under a new reservation.
func (s *Service) pbSettleLeftIntents(db *storage.Store, runID string) error {
	now := s.Clock.Now().UTC()
	return db.WithWriterContext(context.Background(), func(tx *sql.Tx) error {
		ws, err := store.BackfillWindows(tx, runID, true)
		if err != nil {
			return err
		}
		for _, w := range ws {
			have := pbAnswers(w.Result)
			held := 0.0
			for m, ans := range have {
				if ans.Intent <= 0 {
					continue
				}
				held += ans.Intent
				have[m] = pbAnswer{Hash: ans.Hash, ReadAt: ans.ReadAt, Failed: pbUnknownClass, Unknown: true, USD: ans.Intent}
			}
			if held == 0 {
				continue
			}
			w.Result, _ = json.Marshal(have)
			w.CostUSD += held
			w.UpdatedAt = now.Format(time.RFC3339)
			if err := store.SaveBackfillWindow(tx, w); err != nil {
				return err
			}
			if err := store.AddBackfillRunSpend(tx, runID, held, now.Format(time.RFC3339)); err != nil {
				return err
			}
		}
		return nil
	})
}

// pbReviewItem is one claim of the pilot review set (ids only; the text is read when shown).
type pbReviewItem struct {
	Part   string // name | disagreement | hearsay
	Window string
	Model  string
	Index  int
}

// pbReviewSet is the pilot's review set: every link=name fact, every fact the other model gave about
// another person (a fact of the other model sharing an evidence line, none about the same person), and
// a sample of at most pbHearsaySample hearsay facts spread by a hash (the same set every time).
func pbReviewSet(ws []store.BackfillWindow) (items []pbReviewItem, names, disagreements, hearsay int) {
	type cand struct {
		item pbReviewItem
		key  string
	}
	var sample []cand
	for _, w := range ws {
		ans := pbAnswers(w.Result)
		models := make([]string, 0, len(ans))
		for m := range ans {
			models = append(models, m)
		}
		sort.Strings(models)
		for _, m := range models {
			for i, f := range ans[m].Facts {
				it := pbReviewItem{Window: w.ID, Model: m, Index: i}
				disagree := false
				for _, o := range models {
					if o == m {
						continue
					}
					overlap, same := false, false
					for _, g := range ans[o].Facts {
						if pbOverlap(f.Ev, g.Ev) {
							overlap = true
							same = same || g.About == f.About
						}
					}
					disagree = disagree || (overlap && !same)
				}
				self := false
				for _, by := range f.By {
					self = self || by == f.About
				}
				switch {
				case f.Link == episodes.LinkName:
					names++
					it.Part = "name"
					items = append(items, it)
				case disagree:
					disagreements++
					it.Part = "disagreement"
					items = append(items, it)
				case !self:
					hearsay++
					it.Part = "hearsay"
					sum := sha256.Sum256([]byte(w.ID + "\x00" + m + "\x00" + strconv.Itoa(i)))
					sample = append(sample, cand{it, hex.EncodeToString(sum[:8])})
				}
			}
		}
	}
	sort.Slice(sample, func(i, j int) bool { return sample[i].key < sample[j].key })
	for i := 0; i < len(sample) && i < pbHearsaySample; i++ {
		items = append(items, sample[i].item)
	}
	return items, names, disagreements, hearsay
}

func pbOverlap(a, b []string) bool {
	for _, x := range a {
		for _, y := range b {
			if x == y {
				return true
			}
		}
	}
	return false
}

// pbPilotReport is the pilot's report: counts and money only.
func pbPilotReport(q *sql.Tx, runID string) (map[string]any, error) {
	ws, err := store.BackfillWindows(q, runID, true)
	if err != nil {
		return nil, err
	}
	type stat struct {
		answered, facts, rejected, refused, failed, skipped int
		usd                                                 float64
	}
	per := map[string]*stat{}
	for _, m := range pbPilotModels {
		per[string(m)] = &stat{}
	}
	sent := 0
	for _, w := range ws {
		if w.State == store.WindowSkipped && len(w.Result) == 0 {
			continue
		}
		sent++
		for m, a := range pbAnswers(w.Result) {
			st := per[m]
			if st == nil {
				continue
			}
			st.usd += a.USD
			switch {
			case a.Skipped:
				st.skipped++
			case a.Refused:
				st.refused++
			case a.Failed != "":
				st.failed++
			default:
				st.answered++
				st.facts += len(a.Facts)
				st.rejected += a.Rejected
			}
		}
	}
	models := map[string]any{}
	total := 0.0
	for _, m := range pbPilotModels {
		st := per[string(m)]
		rate := 0.0
		if st.facts+st.rejected > 0 {
			rate = float64(st.rejected) / float64(st.facts+st.rejected)
		}
		total += st.usd
		name := "haiku"
		if m == peoplebackfill.ModelSonnet {
			name = "sonnet"
		}
		models[name] = map[string]any{"model": string(m), "answered": st.answered, "facts": st.facts, "rejected": st.rejected,
			"reject_rate": float64(int(rate*1000+0.5)) / 1000, "refused": st.refused, "failed": st.failed, "skipped": st.skipped, "cost_usd": usd(st.usd)}
	}
	items, names, disagreements, hearsay := pbReviewSet(ws)
	return map[string]any{"windows": sent, "models": models, "name_links": names, "disagreements": disagreements, "hearsay": hearsay,
		"review_items": len(items), "cost_usd": usd(total)}, nil
}

// ---------------------------------------------------------------------------
// run
// ---------------------------------------------------------------------------

// pbStartRun is mode=run: after this plan's finished pilot, the chosen model through Message Batches.
func (s *Service) pbStartRun(ctx context.Context, req action.Request, db *storage.Store, in map[string]any) action.Result {
	plan, started, err := s.pbStartedPlan(db, in)
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	if !started {
		var fail *action.Result
		if plan, fail = s.pbConfirmedPlan(ctx, req, db, in); fail != nil {
			return *fail
		}
	}
	model, ok := pbModels[strVal(in, "model", "")]
	if !ok {
		return action.Fail(req, errcode.InvalidInput, "error.invalid_input", s.msg(req, "error.invalid_input"), map[string]any{"field": "model", "allowed": "haiku|sonnet"})
	}
	pbLoops.Lock()
	defer pbLoops.Unlock()
	runs, err := store.BackfillRuns(db.Reader(), plan.Params.Guild)
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	var run *store.BackfillRun
	for i, r := range runs {
		if r.Token != plan.Token && (r.State == store.BackfillPiloting || r.State == store.BackfillSubmitted) {
			return action.Fail(req, errcode.ConfigConflict, "people_backfill.busy", s.msg(req, "people_backfill.busy"), map[string]any{"run": pbRunJSON(db, r)})
		}
		if r.Token == plan.Token && r.PilotDone && run == nil &&
			(r.State == store.BackfillPiloted || r.State == store.BackfillStopped || r.State == store.BackfillSubmitted) {
			run = &runs[i]
		}
	}
	if run == nil {
		return action.Fail(req, errcode.InvalidInput, "people_backfill.no_pilot", s.msg(req, "people_backfill.no_pilot"), nil)
	}
	if run.Model != "" && run.Model != string(model) {
		return action.Fail(req, errcode.InvalidInput, "error.invalid_input", s.msg(req, "error.invalid_input"), map[string]any{"field": "model", "run_model": run.Model})
	}
	if want := strVal(in, "provider", ""); want != "" && want != run.Provider {
		return action.Fail(req, errcode.InvalidInput, "error.invalid_input", s.msg(req, "error.invalid_input"), map[string]any{"field": "provider", "run_provider": run.Provider})
	}
	_, a, err := s.pbAdapter(s.Doc(), run.Provider)
	if err != nil {
		return s.pbProviderFail(req, err)
	}
	requeue := boolVal(in, "requeue_unknown")
	if l := pbLoops.m[s][run.ID]; l != nil {
		// a worker is on it (a resumed one only polls): let it send again
		l.submit.Store(true)
		if requeue {
			l.requeue.Store(true)
		}
		select {
		case l.wake <- struct{}{}:
		default:
		}
		return action.OK(req, map[string]any{"mode": "run", "run": pbRunJSON(db, *run)})
	}
	if run.State != store.BackfillSubmitted {
		from := run.State
		run.State, run.Model, run.StoppedBy, run.UpdatedAt = store.BackfillSubmitted, string(model), "", s.Clock.Now().UTC().Format(time.RFC3339)
		if err := db.WithWriterContext(ctx, func(tx *sql.Tx) error { return store.SaveBackfillRun(tx, *run, from) }); err != nil {
			return s.failErr(req, errcode.IO, "error.storage", err, nil)
		}
	}
	runID := run.ID
	if !s.pbStartLocked(runID, "people-backfill-run", true, func(ctx context.Context, l *pbLoop) {
		if requeue {
			l.requeue.Store(true)
		}
		s.pbRunLoop(ctx, runID, a, l)
	}) {
		return action.Fail(req, errcode.FeatureDisabled, "people_backfill.unavailable", s.msg(req, "people_backfill.unavailable"), nil)
	}
	s.Log.Info("people backfill: run started", "run", runID, "model", string(model))
	return action.OK(req, map[string]any{"mode": "run", "run": pbRunJSON(db, *run)})
}

// pbStartedPlan is the plan of a run that already started (a model chosen) whose token confirm names:
// continuing it reads its own stored windows, so the plan is not built again (members delete and edit
// messages during a long run, and each window is checked again when it is applied).
func (s *Service) pbStartedPlan(db *storage.Store, in map[string]any) (pbPlan, bool, error) {
	confirm := strVal(in, "confirm", "")
	guild, err := pbGuild(db, in)
	if err != nil || guild == "" || confirm == "" {
		return pbPlan{}, false, err
	}
	runs, err := store.BackfillRuns(db.Reader(), guild, store.BackfillStopped, store.BackfillSubmitted)
	if err != nil {
		return pbPlan{}, false, err
	}
	for _, r := range runs {
		if r.Token == confirm && r.Model != "" && r.PilotDone {
			return pbPlan{Params: pbParams{Guild: guild}, Token: r.Token}, true, nil
		}
	}
	return pbPlan{}, false, nil
}

// pbRunLoop drives a run: it settles the batches in flight and, while it may send, submits the next
// one under the cap. It stops when nothing is left, when the cap is reached, or (a worker that may not
// send) once nothing is in flight.
func (s *Service) pbRunLoop(ctx context.Context, runID string, a *openai.Adapter, l *pbLoop) {
	db, err := s.adminDB()
	if err != nil {
		return
	}
	fails := 0
	pilotDone := false
	for {
		if l.submit.Load() && !pilotDone {
			if err := s.pbApplyPilot(ctx, db, runID); err != nil {
				if ctx.Err() != nil {
					return
				}
				s.Log.Warn("people backfill: pilot answers not applied", "run", runID, "err", err)
			} else {
				pilotDone = true
			}
		}
		done, wait, err := s.pbStep(ctx, db, runID, a, l)
		if ctx.Err() != nil {
			return
		}
		if err != nil {
			fails++
			s.Log.Warn("people backfill: step failed", "run", runID, "fails", fails, "class", string(errclass.Of(err)), "err", err)
			if fails >= pbStopFails {
				s.pbStop(db, runID, l, "error")
				return
			}
			wait = true
		} else {
			fails = 0
		}
		if done {
			return
		}
		if !wait {
			continue
		}
		t := time.NewTimer(pbPollInterval())
		select {
		case <-ctx.Done():
			t.Stop()
			return
		case <-l.wake:
			t.Stop()
		case <-t.C:
		}
	}
}

// pbStop ends a run's worker with the run stopped (the rest stays queued).
func (s *Service) pbStop(db *storage.Store, runID string, l *pbLoop, by string) {
	pbLoops.Lock()
	defer pbLoops.Unlock()
	s.pbEndLocked(db, runID, l, store.BackfillStopped, by)
}

// pbEndLocked moves a submitted run to state (pbLoops held) and drops its worker.
func (s *Service) pbEndLocked(db *storage.Store, runID string, l *pbLoop, state, by string) {
	err := db.WithWriterContext(context.Background(), func(tx *sql.Tx) error {
		run, ok, err := store.LoadBackfillRun(tx, runID)
		if err != nil || !ok {
			return err
		}
		run.State, run.StoppedBy, run.UpdatedAt = state, by, s.Clock.Now().UTC().Format(time.RFC3339)
		return store.SaveBackfillRun(tx, run, store.BackfillSubmitted)
	})
	if err != nil && !errors.Is(err, store.ErrBackfillRunState) {
		s.Log.Warn("people backfill: run state not saved", "run", runID, "err", err)
	}
	if pbLoops.m[s][runID] == l {
		delete(pbLoops.m[s], runID)
	}
	s.Log.Info("people backfill: run ended", "run", runID, "state", state, "stopped_by", by)
}

// pbSaveBatches writes a run's batches list (the run must still be submitted).
func (s *Service) pbSaveBatches(ctx context.Context, db *storage.Store, runID string, bs []pbBatch, tx *sql.Tx) error {
	save := func(tx *sql.Tx) error {
		run, ok, err := store.LoadBackfillRun(tx, runID)
		if err != nil || !ok {
			return err
		}
		run.Batches, _ = json.Marshal(bs)
		run.UpdatedAt = s.Clock.Now().UTC().Format(time.RFC3339)
		return store.SaveBackfillRun(tx, run, store.BackfillSubmitted)
	}
	if tx != nil {
		return save(tx)
	}
	return db.WithWriterContext(ctx, save)
}

// pbStep is one turn of a run's worker. done ends the worker; wait sleeps until the next poll.
func (s *Service) pbStep(ctx context.Context, db *storage.Store, runID string, a *openai.Adapter, l *pbLoop) (done, wait bool, err error) {
	run, ok, err := store.LoadBackfillRun(db.Reader(), runID)
	if err != nil {
		return false, true, err
	}
	if !ok || run.State != store.BackfillSubmitted {
		return true, false, nil
	}
	model := peoplebackfill.Model(run.Model)
	batches := pbDecodeBatches(run.Batches)
	inflight, unknown, changed := false, false, false
	for i := range batches {
		b := &batches[i]
		switch b.State {
		case pbBatchSubmitting, pbBatchUnknown:
			if b.State == pbBatchUnknown && l.requeue.Load() {
				// the operator checked the host's batch list: it never ran
				if err := s.pbRequeueBatch(ctx, db, runID, b.Key); err != nil {
					return false, true, err
				}
				b.State, changed = pbBatchFailed, true
				continue
			}
			ref := openai.BatchRef{API: a.API, BaseURL: a.BaseURL, Kind: openai.BatchChat, Model: b.Model, Count: b.Windows, SubmitKey: b.Key}
			if b.Ref != nil {
				ref = *b.Ref
			}
			if t, err := time.Parse(time.RFC3339Nano, b.At); err == nil && ref.SubmittedAt.IsZero() {
				ref.SubmittedAt = t
			}
			found, err := a.ReconcileBatch(ctx, ref)
			switch {
			case err == nil:
				b.Ref, b.State, changed, inflight = &found, pbBatchSubmitted, true, true
			case errors.Is(err, openai.ErrBatchNotSubmitted):
				if err := s.pbRequeueBatch(ctx, db, runID, b.Key); err != nil {
					return false, true, err
				}
				b.State, changed = pbBatchFailed, true
			default:
				if b.State != pbBatchUnknown {
					b.State, changed = pbBatchUnknown, true
				}
				unknown = true
			}
		case pbBatchSubmitted:
			st, err := a.PollBatch(ctx, *b.Ref)
			if err != nil {
				return false, true, err
			}
			if !st.State.Final() {
				inflight = true
				continue
			}
			if st.State == openai.BatchFailed {
				if err := s.pbRequeueBatch(ctx, db, runID, b.Key); err != nil {
					return false, true, err
				}
				b.State, changed = pbBatchFailed, true
				continue
			}
			outs, err := a.BatchResults(ctx, *b.Ref)
			if err != nil {
				return false, true, err
			}
			if err := s.pbApplyBatch(ctx, db, run, b.Key, model, outs); err != nil {
				return false, true, err
			}
			b.State, changed = pbBatchApplied, true
			_ = a.CleanupBatch(ctx, *b.Ref)
		}
	}
	l.requeue.Store(false) // the operator's release covers the submissions unknown when it was given
	if changed {
		if err := s.pbSaveBatches(ctx, db, runID, batches, nil); err != nil {
			return false, true, err
		}
	}
	if unknown {
		l.submit.Store(false)
		l.stop = "submit_unknown"
	}
	if !l.submit.Load() {
		if inflight {
			return false, true, nil
		}
		pbLoops.Lock()
		defer pbLoops.Unlock()
		if l.submit.Load() && !unknown {
			return false, false, nil // the operator asked to go on meanwhile
		}
		by := l.stop
		if by == "" {
			by = "restart"
		}
		state := store.BackfillStopped
		if !unknown && by == "restart" {
			if c, err := store.CountBackfillWindows(db.Reader(), runID); err == nil && c.States[store.WindowQueued]+c.States[store.WindowSubmitted] == 0 {
				state, by = store.BackfillDone, ""
			}
		}
		s.pbEndLocked(db, runID, l, state, by)
		return true, false, nil
	}
	return s.pbSubmit(ctx, db, runID, a, l, inflight)
}

// pbSubmit sends the next batch when the cap allows it.
func (s *Service) pbSubmit(ctx context.Context, db *storage.Store, runID string, a *openai.Adapter, l *pbLoop, inflight bool) (done, wait bool, err error) {
	run, ok, err := store.LoadBackfillRun(db.Reader(), runID)
	if err != nil || !ok {
		return !ok && err == nil, true, err
	}
	model := peoplebackfill.Model(run.Model)
	batches := pbDecodeBatches(run.Batches)
	reserved := 0.0
	for _, b := range batches {
		if b.inFlight() {
			reserved += b.MaxUSD
		}
	}
	queued, err := store.BackfillQueue(db.Reader(), runID, pbMaxBatch)
	if err != nil {
		return false, true, err
	}
	spent, err := store.BackfillGuildSpent(db.Reader(), run.GuildID) // the cap is the server's total across its runs
	if err != nil {
		return false, true, err
	}
	if len(queued) == 0 {
		if inflight {
			return false, true, nil
		}
		pbLoops.Lock()
		defer pbLoops.Unlock()
		c, err := store.CountBackfillWindows(db.Reader(), runID)
		if err != nil {
			return false, true, err
		}
		if c.States[store.WindowSubmitted] > 0 {
			return false, true, nil
		}
		s.pbEndLocked(db, runID, l, store.BackfillDone, "")
		var plan pbParams
		if ok, _ := s.kvGet(pbPlanKey+run.GuildID, &plan); ok && plan.Token == run.Token {
			_ = s.kvDelete(pbPlanKey + run.GuildID)
		}
		return true, false, nil
	}
	type sent struct {
		w  store.BackfillWindow
		bp pbPrompt
	}
	var items []openai.BatchItem
	var picked []sent
	var skipped []store.BackfillWindow
	next := 0.0
	full := false
	for _, w := range queued {
		bp, err := s.pbBuildPrompt(db, run.GuildID, w)
		if err != nil {
			return false, true, err
		}
		if !bp.ok {
			skipped = append(skipped, w)
			continue
		}
		v := pbMaxCost(bp, model, peoplebackfill.ModeBatch)
		if !peoplebackfill.CanSubmit(spent, reserved, next+v, run.BudgetUSD) {
			full = true
			break
		}
		next += v
		r := pbRequest(model, bp)
		items = append(items, openai.BatchItem{ID: w.ID, Chat: &r})
		picked = append(picked, sent{w, bp})
	}
	for _, w := range skipped {
		if err := s.pbSettleWindow(ctx, db, runID, w, store.WindowSkipped, 0, 0); err != nil {
			return false, true, err
		}
	}
	if len(items) == 0 {
		if !full {
			return false, false, nil // only windows with nothing to read were left on this page
		}
		if inflight {
			return false, true, nil // settling the batches in flight frees their reservation
		}
		pbLoops.Lock()
		defer pbLoops.Unlock()
		s.pbEndLocked(db, runID, l, store.BackfillStopped, "budget")
		return true, false, nil
	}
	var kb [8]byte
	_, _ = rand.Read(kb[:])
	now := s.Clock.Now().UTC()
	b := pbBatch{Key: "pbb_" + hex.EncodeToString(kb[:]), State: pbBatchSubmitting, Model: string(model), Windows: len(items), MaxUSD: next,
		At: now.Format(time.RFC3339Nano)}
	batches = append(batches, b)
	// the intent first: after a crash between this and the create call the batch is reconciled, never sent twice
	err = db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		for _, p := range picked {
			w := p.w
			w.State, w.Batch, w.PromptHash, w.ReadAt, w.UpdatedAt = store.WindowSubmitted, b.Key, p.bp.hash, p.bp.readAt, now.Format(time.RFC3339)
			w.Attempts++
			if err := store.SaveBackfillWindow(tx, w); err != nil {
				return err
			}
		}
		return s.pbSaveBatches(ctx, db, runID, batches, tx)
	})
	if err != nil {
		return false, true, err
	}
	// a shutdown does not cut the create call: a cut call leaves a batch the host may run and bill whose
	// results nobody can read (Anthropic keeps no key to find it by); Close waits for it, bounded
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), pbCreateTimeout)
	ref, err := a.SubmitBatch(cctx, openai.BatchChat, string(model), items, b.Key)
	cancel()
	last := &batches[len(batches)-1]
	var unk *openai.SubmitUnknownError
	switch {
	case err == nil:
		last.Ref, last.State = &ref, pbBatchSubmitted
	case errors.As(err, &unk):
		last.Ref, last.State = &unk.Ref, pbBatchUnknown
	default:
		// a definite refusal of the create call: nothing was sent; the windows go back and the run stops
		// once nothing is in flight
		if rerr := s.pbRequeueBatch(context.Background(), db, runID, b.Key, true); rerr != nil {
			return false, true, rerr
		}
		last.State = pbBatchFailed
		l.submit.Store(false)
		l.stop = "error"
		s.Log.Warn("people backfill: batch not accepted", "run", runID, "class", string(errclass.Of(err)), "err", err)
	}
	if serr := s.pbSaveBatches(context.Background(), db, runID, batches, nil); serr != nil {
		return false, true, serr
	}
	s.Log.Info("people backfill: batch sent", "run", runID, "windows", len(items), "state", last.State)
	return false, false, nil
}

// pbRequeueBatch sends a batch's unsettled windows back to the queue (unsent=true: they never left, so
// the send is not counted).
func (s *Service) pbRequeueBatch(ctx context.Context, db *storage.Store, runID, key string, unsent ...bool) error {
	ws, err := store.BackfillBatchWindows(db.Reader(), runID, key)
	if err != nil {
		return err
	}
	for _, w := range ws {
		if len(unsent) > 0 && unsent[0] {
			w.Attempts = max(0, w.Attempts-1)
		}
		if err := s.pbRequeue(ctx, db, runID, w, 0, 0); err != nil {
			return err
		}
	}
	return nil
}

// pbApplyBatch applies a finished batch's outcomes, matched by custom_id (the host's order is not the
// items'). A window without an outcome, an expired or failed item, or a window whose prompt changed
// since it was sent goes back to the queue; a refusal is counted and skipped.
func (s *Service) pbApplyBatch(ctx context.Context, db *storage.Store, run store.BackfillRun, key string, model peoplebackfill.Model, outs []openai.BatchOutcome) error {
	byID := make(map[string]openai.BatchOutcome, len(outs))
	for _, o := range outs {
		byID[o.ID] = o
	}
	ws, err := store.BackfillBatchWindows(db.Reader(), run.ID, key)
	if err != nil {
		return err
	}
	for _, w := range ws {
		o, ok := byID[w.ID]
		if !ok {
			if err := s.pbRequeue(ctx, db, run.ID, w, 0, 0); err != nil {
				return err
			}
			continue
		}
		spent, calls := 0.0, 0
		if o.Chat != nil {
			spent, calls = pbSettle(o.Chat, model, peoplebackfill.ModeBatch), 1
		}
		switch {
		case errors.Is(o.Err, openai.ErrRefused):
			err = s.pbSettleWindow(ctx, db, run.ID, w, store.WindowRefused, spent, calls)
		case o.Err != nil || o.Chat == nil:
			err = s.pbRequeue(ctx, db, run.ID, w, spent, calls)
		default:
			bp, berr := s.pbBuildPrompt(db, run.GuildID, w)
			switch {
			case berr != nil:
				return berr
			case !bp.ok:
				err = s.pbSettleWindow(ctx, db, run.ID, w, store.WindowSkipped, spent, calls)
			case bp.hash != w.PromptHash:
				err = s.pbRequeue(ctx, db, run.ID, w, spent, calls) // a line changed or went since it was sent
			default:
				facts, rejected := bp.p.ParsePeople(o.Chat.Content, episodes.PeopleOptions{})
				err = s.pbApply(ctx, db, run.ID, run.GuildID, w, facts, rejected, bp, spent, calls)
			}
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// pbApplyPilot applies the chosen model's stored pilot answers once a run starts, then drops every
// stored answer (the review set is gone with them). A window whose prompt changed, or whose answer
// failed, stays queued and goes out in a batch. The pilot's spend is already counted.
func (s *Service) pbApplyPilot(ctx context.Context, db *storage.Store, runID string) error {
	run, ok, err := store.LoadBackfillRun(db.Reader(), runID)
	if err != nil || !ok {
		return err
	}
	ws, err := store.BackfillWindows(db.Reader(), runID, true, store.WindowQueued)
	if err != nil {
		return err
	}
	for _, w := range ws {
		ans, ok := pbAnswers(w.Result)[run.Model]
		if !ok || ans.Failed != "" {
			continue
		}
		switch {
		case ans.Refused:
			err = s.pbSettleWindow(ctx, db, runID, w, store.WindowRefused, 0, 0)
		case ans.Skipped:
			err = s.pbSettleWindow(ctx, db, runID, w, store.WindowSkipped, 0, 0)
		default:
			bp, berr := s.pbBuildPrompt(db, run.GuildID, w)
			if berr != nil {
				return berr
			}
			if !bp.ok || bp.hash != ans.Hash {
				continue
			}
			w.Result = nil
			err = s.pbApply(ctx, db, runID, run.GuildID, w, ans.Facts, ans.Rejected, bp, 0, 0)
		}
		if err != nil {
			return err
		}
	}
	return db.WithWriterContext(ctx, func(tx *sql.Tx) error {
		return store.ClearBackfillResults(tx, runID, s.Clock.Now().UTC().Format(time.RFC3339))
	})
}

// ---------------------------------------------------------------------------
// restart
// ---------------------------------------------------------------------------

// resumePeopleBackfill runs at start (ResumeJobs): a pilot a restart cut is marked stopped (the operator
// resumes it), and the batches of a run an operator started are polled and applied. Nothing new is
// created or sent.
func (s *Service) resumePeopleBackfill(ctx context.Context) {
	if s.practice {
		return
	}
	db, err := s.adminDB()
	if err != nil || !db.IsSQLite() {
		return
	}
	runs, err := store.BackfillRuns(db.Reader(), "", store.BackfillPiloting, store.BackfillSubmitted)
	if err != nil {
		return // the tables exist from the first start on; a read error leaves the runs as they are
	}
	pbLoops.Lock()
	defer pbLoops.Unlock()
	now := s.Clock.Now().UTC().Format(time.RFC3339)
	for _, run := range runs {
		if pbLoops.m[s][run.ID] != nil {
			continue
		}
		switch run.State {
		case store.BackfillPiloting:
			run.State, run.StoppedBy, run.UpdatedAt = store.BackfillStopped, "restart", now
			if err := db.WithWriterContext(ctx, func(tx *sql.Tx) error { return store.SaveBackfillRun(tx, run, store.BackfillPiloting) }); err != nil {
				s.Log.Warn("people backfill: cut pilot not marked", "run", run.ID, "err", err)
			}
		case store.BackfillSubmitted:
			_, a, err := s.pbAdapter(s.Doc(), run.Provider)
			if err != nil {
				s.Log.Warn("people backfill: run not resumed, provider unavailable", "run", run.ID)
				continue
			}
			runID := run.ID
			s.pbStartLocked(runID, "people-backfill-poll", false, func(ctx context.Context, l *pbLoop) { s.pbRunLoop(ctx, runID, a, l) })
		}
	}
}

// ---------------------------------------------------------------------------
// review (console only)
// ---------------------------------------------------------------------------

var pbRefPattern = regexp.MustCompile(`\{@([^{}]+)\}`)

// peopleBackfillReview is memory.people.backfill.review: the pilot's review set, rendered for the
// console with the labels the prompt showed. It is never logged. Each item is checked again: a window
// whose lines changed, or a person who withdrew consent, is left out (counted in dropped).
func (s *Service) peopleBackfillReview(ctx context.Context, req action.Request, db *storage.Store, in map[string]any) action.Result {
	run, ok, err := s.pbFindRun(db, in)
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	out := map[string]any{"items": []any{}, "dropped": 0, "available": false}
	if !ok {
		return action.OK(req, out)
	}
	out["run_id"] = run.ID
	if !run.PilotDone || run.Model != "" || (run.State != store.BackfillPiloted && run.State != store.BackfillStopped) {
		return action.OK(req, out) // no pilot yet, or the run started and the answers are gone
	}
	ws, err := store.BackfillWindows(db.Reader(), run.ID, true)
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	items, _, _, _ := pbReviewSet(ws)
	byID := map[string]store.BackfillWindow{}
	for _, w := range ws {
		byID[w.ID] = w
	}
	tx, err := db.Reader().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	denied, err := s.deniedPeople(tx, run.GuildID)
	_ = tx.Rollback()
	if err != nil {
		return s.failErr(req, errcode.IO, "error.storage", err, nil)
	}
	prompts := map[string]pbPrompt{}
	var shown []map[string]any
	dropped := 0
	for _, it := range items {
		w := byID[it.Window]
		ans := pbAnswers(w.Result)[it.Model]
		if it.Index >= len(ans.Facts) {
			dropped++
			continue
		}
		f := ans.Facts[it.Index]
		bp, seen := prompts[w.ID]
		if !seen {
			bp, err = s.pbBuildPrompt(db, run.GuildID, w)
			if err != nil {
				return s.failErr(req, errcode.IO, "error.storage", err, nil)
			}
			prompts[w.ID] = bp
		}
		if !bp.ok || bp.hash != ans.Hash || denied[f.About] {
			dropped++
			continue
		}
		var speakers []string
		gone := false
		for _, by := range f.By {
			gone = gone || denied[by]
			speakers = append(speakers, pbLabel(bp, by))
		}
		if gone {
			dropped++
			continue
		}
		text := pbRefPattern.ReplaceAllStringFunc(f.Text, func(m string) string { return pbLabel(bp, m[2:len(m)-1]) })
		shown = append(shown, map[string]any{"part": it.Part, "window_id": it.Window, "model": it.Model, "kind": f.Kind, "topic": f.Topic,
			"text": text, "subject": pbLabel(bp, f.About), "speakers": speakers, "link": f.Link, "evidence_n": len(f.Ev)})
	}
	out["items"], out["dropped"], out["available"] = shown, dropped, true
	if shown == nil {
		out["items"] = []any{}
	}
	return action.OK(req, out)
}

func pbLabel(bp pbPrompt, ref string) string {
	if v := bp.labels[ref]; v != "" {
		return v
	}
	return "?"
}
