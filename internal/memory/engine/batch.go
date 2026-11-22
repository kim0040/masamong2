package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

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
//
// Engine lock order (an outer lock may be held while an inner one is taken, never the reverse):
// indexMu serializes index passes and the batch stage writes that hold it and is the outermost; under
// it a pass may take adaptMu (the one-time space adaptation), messageWidthMu (message vector width
// reads and the short-width wait), rateMu (call latency samples) and, in Close, reloadMu. reloadMu
// serializes index rebuilds and under it the rebuild may take adaptMu and closers.mu (the retired
// index files). qmu (the query-embedding cache) stands outside this order: it is never held while
// another engine lock is taken and no engine lock is held while it is taken.

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

// ErrRetired is returned by an engine a newer configuration replaced; the caller retries on the
// current engine (a batch can outlast several).
var ErrRetired = errRetired

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

func clipText(s string, n int) string {
	if r := []rune(s); len(r) > n {
		return string(r[:n])
	}
	return s
}

// ---------------------------------------------------------------------------
// apply
// ---------------------------------------------------------------------------
