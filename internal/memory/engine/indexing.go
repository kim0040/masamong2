package engine

import (
	"context"
	"errors"
	"log/slog"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"masamong/internal/crash"
	"masamong/internal/lang"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
	"masamong/internal/providers/openai"
)

// IndexOptions bound one indexing pass.
type IndexOptions struct {
	Rooms     []string // empty = every registered room
	BudgetUSD float64  // stop starting new paid calls once spent (cache hits are free)
	DryRun    bool     // sync leaves and count work, but make no paid calls
	Limit     int      // max leaves to process this pass (0 = all pending)
}

// IndexReport summarizes a pass. No bodies.
type IndexReport struct {
	Rooms          []store.SyncStats `json:"rooms"`
	Pending        int               `json:"pending"`
	LeafVectors    int               `json:"leaf_vectors"`
	FromCache      int               `json:"from_cache"`
	Adapted        int               `json:"adapted,omitempty"` // leaves truncated to a lower dimensionality
	Summaries      int               `json:"summaries"`
	SummaryVectors int               `json:"summary_vectors"`
	SpentUSD       float64           `json:"spent_usd"`
	Estimate       *Estimate         `json:"estimate,omitempty"`
	StoppedBy      string            `json:"stopped_by,omitempty"`
	// FailedRooms could not be resynced (read or write error); the other rooms were still processed.
	FailedRooms map[string]string `json:"failed_rooms,omitempty"`
	// Warnings are rooms that were indexed with a fallback, such as a stored time zone that is not valid.
	Warnings map[string]string `json:"warnings,omitempty"`
	Reload   map[string]int    `json:"reload,omitempty"`
	Elapsed  string            `json:"elapsed"`
	// MessageVectors are the message vectors embedded this pass (memory.message_vectors).
	MessageVectors int `json:"message_vectors,omitempty"`
}

// Estimate previews a pass before it spends anything. Costs use the models' prices (config or the
// provider's list) with token counts approximated from the text; the provider-reported cost of the
// real run is authoritative. Seconds uses the call latencies measured on earlier runs.
type Estimate struct {
	LeafEmbeds   int     `json:"leaf_embeds"`
	CachedLeaves int     `json:"cached_leaves"`
	Summaries    int     `json:"summaries"`
	EmbedTokens  int     `json:"embed_tokens"`
	EmbedUSD     float64 `json:"embed_usd"`
	SummaryUSD   float64 `json:"summary_usd"`
	TotalUSD     float64 `json:"total_usd"`
	PriceBasis   string  `json:"price_basis"` // prices | fallback (a model's price is unknown)
	Seconds      int     `json:"seconds"`
	Note         string  `json:"note"`
	// MessageEmbeds and MessageTokens are the message vectors a pass would embed and their approximate
	// tokens (memory.message_vectors). They are part of EmbedUSD, TotalUSD and Seconds.
	MessageEmbeds int `json:"message_embeds,omitempty"`
	MessageTokens int `json:"message_tokens,omitempty"`
	// SummaryVectors are leaves whose summary text exists but whose summary vector does not (a new embed
	// space, a cleared cache). summarize embeds each one alone, so they are part of EmbedUSD, TotalUSD and
	// Seconds too; before they were counted nowhere and such a pass was estimated at 0 s and $0.
	SummaryVectors int `json:"summary_vectors,omitempty"`
}

// Rates are measured call latencies (exponential moving averages) used for time estimates.
type Rates struct {
	EmbedBatchSec float64 `json:"embed_batch_sec"` // one embeddings call of EmbedBatch documents
	SummarySec    float64 `json:"summary_sec"`     // one summary chat call plus its vector
	Samples       int     `json:"samples"`
}

// DefaultRates are used until a run has been measured.
func DefaultRates() Rates { return Rates{EmbedBatchSec: 1.5, SummarySec: 3.0} }

func (e *Engine) rates() Rates {
	e.rateMu.Lock()
	defer e.rateMu.Unlock()
	r := e.rate
	d := DefaultRates()
	if r.EmbedBatchSec <= 0 {
		r.EmbedBatchSec = d.EmbedBatchSec
	}
	if r.SummarySec <= 0 {
		r.SummarySec = d.SummarySec
	}
	return r
}

// SetRates seeds the measured latencies (the application persists them between starts).
func (e *Engine) SetRates(r Rates) {
	e.rateMu.Lock()
	e.rate = r
	e.rateMu.Unlock()
}

// Rates returns the current measured latencies.
func (e *Engine) Rates() Rates { return e.rates() }

func (e *Engine) observe(embedBatch, summary time.Duration) {
	e.rateMu.Lock()
	defer e.rateMu.Unlock()
	const w = 0.2
	if embedBatch > 0 {
		v := embedBatch.Seconds()
		if e.rate.EmbedBatchSec <= 0 {
			e.rate.EmbedBatchSec = v
		} else {
			e.rate.EmbedBatchSec = (1-w)*e.rate.EmbedBatchSec + w*v
		}
	}
	if summary > 0 {
		v := summary.Seconds()
		if e.rate.SummarySec <= 0 {
			e.rate.SummarySec = v
		} else {
			e.rate.SummarySec = (1-w)*e.rate.SummarySec + w*v
		}
	}
	e.rate.Samples++
}

// approxTokens estimates a tokenizer's count: Hangul runs near 1.4 tokens per syllable (measured
// 2026-10-11 against OpenRouter's billed qwen3 embedding of Korean chat leaves; the earlier 1/1.3 came
// out at about half the bill), kana and Han near one per 1.3 characters, other scripts one per 3.5.
func approxTokens(text string) int {
	hangul, cjk, other := 0, 0, 0
	for _, r := range text {
		switch {
		case r >= 0xAC00 && r <= 0xD7A3:
			hangul++
		case r >= 0x3040 && r <= 0x30FF, r >= 0x4E00 && r <= 0x9FFF:
			cjk++
		default:
			other++
		}
	}
	return int(float64(hangul)*1.4 + float64(cjk)/1.3 + float64(other)/3.5 + 0.5)
}

// Progress is reported during a pass.
type Progress func(phase string, done, total int)

func (e *Engine) rooms(opts IndexOptions) ([]string, error) {
	if len(opts.Rooms) > 0 {
		return opts.Rooms, nil
	}
	rs, err := e.Store.Rooms()
	if err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rs {
		out = append(out, r.Room.Room)
	}
	return out, nil
}

type budget struct {
	mu    sync.Mutex
	limit float64
	spent float64
}

func (b *budget) ok() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.limit <= 0 || b.spent < b.limit
}

func (b *budget) add(v *float64, fallback float64) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if v != nil {
		b.spent += *v
	} else {
		b.spent += fallback
	}
}

// warnedRoomZones keeps a bad stored room zone to one log line per room and zone.
var warnedRoomZones sync.Map

// IndexRooms runs one pass: resync leaves of each room, fill vectors from the cache, embed the rest,
// then (if enabled) summarize and embed summaries, and finally reload the in-memory index.
func (e *Engine) IndexRooms(ctx context.Context, opts IndexOptions, progress Progress) (IndexReport, error) {
	e.indexMu.Lock()
	defer e.indexMu.Unlock()
	if e.retired.Load() {
		return IndexReport{}, errRetired
	}
	start := time.Now()
	if progress == nil {
		progress = func(string, int, int) {}
	}
	rep := IndexReport{}
	rooms, err := e.rooms(opts)
	if err != nil {
		return rep, err
	}
	roomTZ := map[string]*time.Location{}
	roomTZErr := map[string]error{}
	if rs, err := e.Store.Rooms(); err == nil {
		for _, r := range rs {
			if r.NaiveTimezone != "" {
				if loc, err := time.LoadLocation(r.NaiveTimezone); err == nil {
					roomTZ[r.Room.Room] = loc
				} else {
					roomTZErr[r.Room.Room] = err
				}
			}
		}
	}
	var readyRooms []string
	for i, room := range rooms {
		if e.retired.Load() {
			return rep, errRetired
		}
		tz := roomTZ[room]
		if err := roomTZErr[room]; err != nil {
			// the room keeps being indexed: its messages are read in the instance's zone until the stored
			// zone is corrected, and the report says so
			msg := "unknown room timezone, using the instance timezone: " + err.Error()
			if rep.Warnings == nil {
				rep.Warnings = map[string]string{}
			}
			rep.Warnings[room] = msg
			if _, warned := warnedRoomZones.LoadOrStore(room+"|"+err.Error(), true); !warned {
				slog.Warn("memory: "+msg, "room", room)
			}
		}
		if tz == nil {
			tz = e.Settings.NaiveTZ
		}
		st, err := e.Store.SyncLeaves(room, e.Settings.Recipe, time.Now(), tz)
		if err != nil {
			// one room's failure (a dropped connection, a damaged row) must not hold every other room back
			if rep.FailedRooms == nil {
				rep.FailedRooms = map[string]string{}
			}
			rep.FailedRooms[room] = err.Error()
			continue
		}
		rep.Rooms = append(rep.Rooms, st)
		readyRooms = append(readyRooms, room)
		progress("sync", i+1, len(rooms))
	}
	rooms = readyRooms
	space := e.EmbedSpace()
	if rep.Adapted, err = e.adaptSpace(); err != nil {
		return rep, err
	}
	leafDims, err := e.pendingLeafDims()
	if err != nil {
		return rep, err
	}
	pending, err := e.Store.PendingAtWidth(rooms, space, e.Settings.Summaries, opts.Limit, leafDims, e.Settings.Recipe.Version())
	if err != nil {
		return rep, err
	}
	rep.Pending = len(pending)
	// reuse pass (free): a leaf of the same text already embedded in this space (with its summary),
	// or a vector kept in the embedding cache
	var needEmbed []store.PendingLeaf
	for _, p := range pending {
		if !p.NeedLeaf {
			continue
		}
		if l, ok := e.Store.LeafWithBody(p.Body, space, p.LeafID); ok && !p.RefreshLeaf && reusableLeafWidth(l.LeafVec, leafDims) {
			if err := e.Store.SaveLeafVector(p.LeafID, space, l.LeafVec, p.Body); err != nil {
				return rep, err
			}
			if l.Summary != "" && len(l.SummaryVec) == len(l.LeafVec) {
				if err := e.Store.SaveSummary(p.LeafID, l.Summary, l.SummaryVec, p.Body); err != nil {
					return rep, err
				}
			}
			rep.FromCache++
		} else if v, ok := e.cachedDocument(p.Body); ok && !p.RefreshLeaf && reusableLeafWidth(v, leafDims) {
			if err := e.Store.SaveLeafVector(p.LeafID, space, v, p.Body); err != nil {
				return rep, err
			}
			rep.FromCache++
		} else {
			needEmbed = append(needEmbed, p)
		}
	}
	// message vectors (memory.message_vectors, on unless set to false): the messages of closed leaves that have no vector in the message
	// space yet, or whose vector was made with another speaker label
	var msgNeed []store.PendingMessage
	var msgSpace string // only worked out when the setting is on: a pass without it does no extra work
	if e.Settings.MessageVectors {
		dims, _, err := e.messageVectorState()
		if err != nil {
			return rep, err
		}
		msgSpace = e.messageSpaceAt(dims)
		if msgNeed, err = e.Store.PendingMessages(rooms, msgSpace, opts.Limit, e.Settings.Recipe.Version()); err != nil {
			return rep, err
		}
	}
	est := e.estimate(needEmbed, pending, msgNeed)
	rep.Estimate = &est
	if opts.DryRun {
		rep.Elapsed = time.Since(start).Round(time.Millisecond).String()
		return rep, nil
	}
	if e.LLM == nil || !e.LLM.Configured() {
		if len(needEmbed) > 0 || len(msgNeed) > 0 {
			rep.StoppedBy = "provider_not_configured"
		}
	} else {
		b := &budget{limit: opts.BudgetUSD}
		n, stop := e.embedLeaves(ctx, needEmbed, space, b, progress)
		rep.LeafVectors = n
		if stop != "" {
			rep.StoppedBy = stop
		}
		if e.Settings.Summaries && rep.StoppedBy == "" {
			leafDims, err = e.pendingLeafDims()
			if err != nil {
				return rep, err
			}
			pending, err = e.Store.PendingAtWidth(rooms, space, true, opts.Limit, leafDims, e.Settings.Recipe.Version())
			if err != nil {
				return rep, err
			}
			s, sv, stop := e.summarize(ctx, pending, b, progress)
			rep.Summaries, rep.SummaryVectors = s, sv
			if stop != "" {
				rep.StoppedBy = stop
			}
		}
		if e.Settings.MessageVectors && rep.StoppedBy == "" {
			// Leaf embedding may have established or changed a non-MRL width since the estimate.
			// Refresh both the space and its pending rows before writing any message vectors.
			dims, _, err := e.messageVectorState()
			if err != nil {
				return rep, err
			}
			// Only an indexing pass releases a short-message wait after the stored leaf width changes.
			// Dimension and diagnostic reads leave the wait intact.
			e.messageWidthMu.Lock()
			if dims > 0 && e.messageShortExpected != dims {
				e.messageShortExpected, e.messageShortReturned = 0, 0
			}
			e.messageWidthMu.Unlock()
			if current := e.messageSpaceAt(dims); current != msgSpace {
				msgSpace = current
				if msgSpace != "" {
					msgNeed, err = e.Store.PendingMessages(rooms, msgSpace, opts.Limit, e.Settings.Recipe.Version())
					if err != nil {
						return rep, err
					}
				} else {
					msgNeed = nil
				}
			}
			n, stop := e.embedMessages(ctx, msgNeed, msgSpace, b, progress)
			rep.MessageVectors = n
			if stop != "" {
				rep.StoppedBy = stop
			}
		}
		rep.SpentUSD = b.spent
	}
	if !e.Models.EmbedTruncatable && rep.StoppedBy == "" {
		lo, hi, err := e.Store.LeafVectorWidths(space, e.Settings.Recipe.Version())
		if err != nil {
			return rep, err
		}
		if lo != hi {
			rep.StoppedBy = "leaf_width_mixed"
		}
	}
	// A full rebuild costs time and memory in proportion to the whole history. A pass that embedded
	// nothing and changed no stored leaf (the usual case in a live channel, whose last leaf stays open)
	// leaves the loaded index exactly as it is, so the rebuild is skipped.
	if e.Index() == nil || indexChanged(rep) || (e.Settings.MessageVectors && e.Index().MessageSpace != e.MessageSpace()) {
		reload, err := e.Reload()
		if err != nil && !errors.Is(err, errRetired) { // retired: the successor engine rebuilds
			return rep, err
		}
		rep.Reload = reload
	}
	rep.Elapsed = time.Since(start).Round(time.Millisecond).String()
	return rep, nil
}

// pendingLeafDims repairs non-MRL widths against a leaf result, or the most common stored width
// before the first result. Matching leaves remain reusable across restarts.
func (e *Engine) pendingLeafDims() (int, error) {
	if e.Models.EmbedTruncatable {
		return 0, nil
	}
	lo, hi, err := e.Store.LeafVectorWidths(e.EmbedSpace(), e.Settings.Recipe.Version())
	if err != nil {
		return 0, err
	}
	if dims := int(e.leafReturnDims.Load()); dims > 0 {
		return dims, nil
	}
	if lo != hi {
		return e.Store.LeafVectorMode(e.EmbedSpace(), e.Settings.Recipe.Version())
	}
	return 0, nil
}

func reusableLeafWidth(vec []byte, dims int) bool {
	return dims == 0 || len(vec) == 2*dims
}

// LeafWidthRepairRooms finds quiet rooms whose active leaves still need width repair.
func (e *Engine) LeafWidthRepairRooms() ([]string, error) {
	dims, err := e.pendingLeafDims()
	if err != nil || dims == 0 {
		return nil, err
	}
	return e.Store.LeafWidthRepairRooms(e.EmbedSpace(), e.Settings.Recipe.Version(), dims)
}

// indexChanged reports whether a pass changed anything the loaded index holds: new vectors or
// summaries, or stored leaves re-cut or removed (their old vectors are still in the index).
func indexChanged(rep IndexReport) bool {
	if rep.LeafVectors+rep.FromCache+rep.Summaries+rep.SummaryVectors+rep.Adapted+rep.MessageVectors > 0 {
		return true
	}
	for _, st := range rep.Rooms {
		if st.New+st.Changed+st.Removed > 0 || st.RecipeChanged {
			return true
		}
	}
	return false
}

// Fallback prices when a model's price is unknown: qwen3-embedding-8b is $0.01 per million tokens and
// one flash-model summary costs about $0.00012.
const (
	fallbackEmbedPerTok  = 0.01 / 1e6
	fallbackSummaryCall  = 0.00012
	summaryPromptExtra   = 60 // tokens of the summary instruction around the body
	summaryOutputTokens  = 80 // a one-sentence summary
	summaryBodyRuneLimit = 2800
)

func (e *Engine) price(model string) (float64, float64, bool) {
	a := e.LLM
	if model != "" && model == e.Models.Summary {
		a = e.summaryLLM()
	}
	if a == nil || a.Price == nil || model == "" {
		return 0, 0, false
	}
	return a.Price(model)
}

func (e *Engine) estimate(needEmbed, pending []store.PendingLeaf, msgs []store.PendingMessage) Estimate {
	tokens := 0
	for _, p := range needEmbed {
		tokens += approxTokens(p.Body)
	}
	msgTokens := 0
	for _, p := range msgs {
		msgTokens += approxTokens(p.Text)
	}
	var sumPrompt, sums, sumVecs int
	if e.Settings.Summaries {
		for _, p := range pending {
			if p.NeedSumm {
				sums++
				body := []rune(p.Body)
				sumPrompt += approxTokens(string(body[:min(summaryBodyRuneLimit, len(body))])) + summaryPromptExtra
			} else if p.NeedSumVec {
				sumVecs++
			}
		}
	}
	est := Estimate{LeafEmbeds: len(needEmbed), CachedLeaves: len(pending) - len(needEmbed), Summaries: sums, EmbedTokens: tokens,
		MessageEmbeds: len(msgs), MessageTokens: msgTokens, SummaryVectors: sumVecs, PriceBasis: "prices", Note: "estimate; the run records the provider-reported cost"}
	embIn, _, embOK := e.price(e.Models.Embed)
	if !embOK {
		embIn, est.PriceBasis = fallbackEmbedPerTok, "fallback"
	}
	// a summary vector embeds the stored summary, about summaryOutputTokens long
	est.EmbedUSD = float64(tokens+msgTokens+sumVecs*summaryOutputTokens) * embIn
	if sums > 0 {
		if in, out, ok := e.price(e.Models.Summary); ok {
			est.SummaryUSD = float64(sumPrompt)*in + float64(sums*summaryOutputTokens)*out
		} else {
			est.SummaryUSD, est.PriceBasis = float64(sums)*fallbackSummaryCall, "fallback"
		}
		est.SummaryUSD += float64(sums*summaryOutputTokens) * embIn // the summary's own vector
	}
	est.TotalUSD = est.EmbedUSD + est.SummaryUSD
	r := e.rates()
	workers := float64(max(1, e.Settings.Workers))
	batches := (len(needEmbed) + e.Settings.EmbedBatch - 1) / e.Settings.EmbedBatch
	msgBatches := (len(msgs) + e.Settings.EmbedBatch - 1) / e.Settings.EmbedBatch
	// summary vectors are one embeddings call each (summarize does not batch them)
	secs := math.Ceil(float64(batches)/workers)*r.EmbedBatchSec + math.Ceil(float64(sums)/workers)*r.SummarySec +
		math.Ceil(float64(msgBatches)/workers)*r.EmbedBatchSec + math.Ceil(float64(sumVecs)/workers)*r.EmbedBatchSec
	est.Seconds = int(math.Ceil(secs))
	return est
}

func (e *Engine) embedLeaves(ctx context.Context, items []store.PendingLeaf, space string, b *budget, progress Progress) (int, string) {
	batch := e.Settings.EmbedBatch
	var batches [][]store.PendingLeaf
	for i := 0; i < len(items); i += batch {
		batches = append(batches, items[i:min(len(items), i+batch)])
	}
	var mu sync.Mutex
	var failed, storeFailed atomic.Bool
	done, stop := 0, ""
	sem := make(chan struct{}, e.Settings.Workers)
	var wg sync.WaitGroup
	for _, bt := range batches {
		if failed.Load() {
			break
		}
		if !b.ok() {
			stop = "budget"
			break
		}
		if ctx.Err() != nil {
			stop = "cancelled"
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(bt []store.PendingLeaf) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					_ = crash.Error(nil, "memory embedding", r)
					failed.Store(true)
				}
			}()
			texts := make([]string, len(bt))
			for i, p := range bt {
				texts[i] = p.Body
			}
			t0 := time.Now()
			res, err := e.embedDocs(ctx, texts, 0.00002*float64(len(texts)))
			if err != nil {
				failed.Store(true)
				return
			}
			if len(texts) == batch {
				e.observe(time.Since(t0), 0)
			}
			b.add(res.CostUSD, 0.00002*float64(len(texts)))
			e.spend("embed", res.CostUSD, 0.00002*float64(len(texts)))
			// stored on the leaf only: the cache keeps a vector once its leaf moves to another space
			saved := 0
			for i, p := range bt {
				v := e.fitDims(res.Vectors[i])
				e.observeLeafWidth(len(v))
				search.Normalize(v)
				if err := e.Store.SaveLeafVector(p.LeafID, space, search.EncodeFloat16(v), p.Body); err != nil {
					storeFailed.Store(true) // the leaf stays pending and is embedded again on a later pass
					continue
				}
				saved++
			}
			mu.Lock()
			done += saved
			progress("embed", done, len(items))
			mu.Unlock()
		}(bt)
	}
	wg.Wait()
	if failed.Load() && stop == "" {
		stop = "provider_error"
	}
	if storeFailed.Load() && stop == "" {
		stop = "store_error"
	}
	return done, stop
}

func (e *Engine) embedDocs(ctx context.Context, texts []string, fallback float64) (openai.EmbedResult, error) {
	if e.LLM == nil {
		return openai.EmbedResult{}, errors.New("memory: embedding provider not configured")
	}
	var last error
	for attempt := 0; attempt < 3; attempt++ {
		began := time.Now()
		res, err := e.LLM.Embed(ctx, e.Models.Embed, texts)
		latency := res.Elapsed
		if latency <= 0 {
			latency = time.Since(began)
		}
		if e.LLM.Configured() && e.Models.Embed != "" && len(texts) > 0 {
			e.recordCall(ctx, "embed", e.Models.Embed, e.CallProviders.Embed, res.CostUSD, fallback, latency, res.PromptTokens, 0, 0, err)
		}
		if err == nil {
			return res, nil
		}
		last = err
		select {
		case <-ctx.Done():
			return openai.EmbedResult{}, ctx.Err()
		case <-time.After(time.Duration(attempt+1) * time.Second):
		}
	}
	return openai.EmbedResult{}, last
}

func normalizeSummary(text, body string) string {
	s := strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if r := []rune(s); len(r) > 400 {
		s = strings.TrimRight(string(r[:400]), " ") + "…"
	}
	if s == "" {
		r := []rune(body)
		s = string(r[:min(180, len(r))])
	}
	return s
}

func (e *Engine) summarize(ctx context.Context, items []store.PendingLeaf, b *budget, progress Progress) (int, int, string) {
	var todo []store.PendingLeaf
	for _, p := range items {
		if p.NeedSumm || p.NeedSumVec {
			todo = append(todo, p)
		}
	}
	var mu sync.Mutex
	sums, vecs, stop := 0, 0, ""
	// short chit-chat often gets the same summary: embed each distinct text once per pass
	var vecMu sync.Mutex
	var failed, storeFailed atomic.Bool
	// a reply without text leaves its leaf unsummarized for the next pass (nothing stands in for it)
	var empties, written atomic.Int64
	summaryVecs := map[string][]byte{}
	sem := make(chan struct{}, e.Settings.Workers)
	var wg sync.WaitGroup
	temp := 0.1
	for i, p := range todo {
		if !b.ok() {
			stop = "budget"
			break
		}
		if ctx.Err() != nil {
			stop = "cancelled"
			break
		}
		sem <- struct{}{}
		wg.Add(1)
		go func(i int, p store.PendingLeaf) {
			defer wg.Done()
			defer func() { <-sem }()
			defer crash.Recover(nil, "memory summary")
			t0 := time.Now()
			fresh := false
			defer func() {
				if fresh {
					e.observe(0, time.Since(t0))
				}
			}()
			summary := p.Summary
			if summary == "" {
				if v, ok := e.Store.CachedVector(store.CacheKey(e.Models.Summary, "summary-text", p.Body)); ok && string(v) != normalizeSummary("", p.Body) {
					summary = string(v)
				} else {
					body := []rune(p.Body)
					a := e.summaryLLM()
					if a == nil {
						failed.Store(true)
						return
					}
					began := time.Now()
					res, err := a.Chat(ctx, openai.ChatRequest{Model: e.Models.Summary, MaxTokens: 120, Temperature: &temp,
						Effort:   "none", // a one-line summary needs no thinking, which would eat the 120 tokens
						Messages: []openai.Message{{Role: "user", Content: lang.Fill(e.Settings.SummaryPrompt, "body", string(body[:min(2800, len(body))]))}}})
					latency := res.Elapsed
					if latency <= 0 {
						latency = time.Since(began)
					}
					if a.Configured() && e.Models.Summary != "" {
						logged := err
						if err == nil && strings.TrimSpace(res.Content) == "" {
							logged = emptyReply{res.FinishReason}
						}
						e.recordCall(ctx, "summary", e.Models.Summary, e.CallProviders.Summary, res.CostUSD, fallbackSummaryCall, latency,
							res.PromptTokens, res.CompletionTokens, res.CachedTokens, logged)
					}
					if err != nil {
						failed.Store(true)
						return
					}
					fresh = true
					b.add(res.CostUSD, fallbackSummaryCall)
					e.spend("summary", res.CostUSD, fallbackSummaryCall)
					if strings.TrimSpace(res.Content) == "" {
						empties.Add(1)
						return
					}
					written.Add(1)
					summary = normalizeSummary(res.Content, p.Body)
					_ = e.Store.PutSummaryText(store.CacheKey(e.Models.Summary, "summary-text", p.Body), p.LeafID, p.Body, summary)
				}
				mu.Lock()
				sums++
				mu.Unlock()
			}
			vecMu.Lock()
			vec := summaryVecs[summary]
			vecMu.Unlock()
			if vec == nil {
				if v, ok := e.cachedDocument(summary); ok {
					vec = v
				} else {
					res, err := e.embedDocs(ctx, []string{summary}, 0.000003)
					if err != nil {
						failed.Store(true)
						if err := e.Store.SaveSummary(p.LeafID, summary, nil, p.Body); err != nil {
							storeFailed.Store(true)
						}
						return
					}
					b.add(res.CostUSD, 0.000003)
					e.spend("embed", res.CostUSD, 0.000003)
					v := e.fitDims(res.Vectors[0])
					search.Normalize(v)
					vec = search.EncodeFloat16(v)
				}
				vecMu.Lock()
				summaryVecs[summary] = vec
				vecMu.Unlock()
			}
			if err := e.Store.SaveSummary(p.LeafID, summary, vec, p.Body); err != nil {
				storeFailed.Store(true)
				return
			}
			mu.Lock()
			vecs++
			progress("summaries", vecs, len(todo))
			mu.Unlock()
		}(i, p)
	}
	wg.Wait()
	if (failed.Load() || empties.Load() > 0 && written.Load() == 0) && stop == "" {
		stop = "provider_error" // a model that answers nothing at all is as stuck as one that errors
	}
	if storeFailed.Load() && stop == "" {
		stop = "store_error"
	}
	return sums, vecs, stop
}
