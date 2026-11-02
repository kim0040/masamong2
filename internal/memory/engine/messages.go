package engine

import (
	"context"
	"errors"
	"fmt"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"masamong/internal/crash"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
)

// Message vectors (settings memory.message_vectors) are a second, optional index over single messages.
// They are cut to at most defaultMaxMessageDims dimensions for MRL models — Matryoshka truncation and a new L2 normalisation,
// the way fitDims shrinks any embedding — and kept as int8 rows with one scale each, in the database and in
// memory alike, so a 512-dimensional message costs about 0.5 KB of vector data. Non-MRL models keep their
// installation's leaf vector width. They live in
// their own embedding space ("model:dims:l2" with the message dimensionality) next to the leaf space, and
// in their own table only: the table is keyed by message and text hash, so it needs no embedding cache.

const (
	defaultMaxMessageDims  = 512
	maxMeasuredMessageDims = 1024
)

var errColdMessageDims = errors.New("memory: cold vector dimensions require canonical scan")

// messageDims is the dimensionality message vectors are kept at: for MRL models, a set leaf width up to
// 512, else 512; for other models, the actual leaf width (0 while no leaf has established it). A measurement override
// (Settings.MessageDims) comes first and may request 1..1024 dimensions.
func (e *Engine) messageDims() int {
	d, _, _ := e.messageVectorState()
	return d
}

// messageVectorState is the checked resolver used by indexing and loading. Public dimension helpers
// keep their existing signatures, but database errors must never turn an indexing pass into success.
func (e *Engine) messageVectorState() (int, string, error) {
	if d := e.Settings.MessageDims; d > 0 && d <= maxMeasuredMessageDims {
		return d, "", nil
	}
	if !e.Models.EmbedTruncatable {
		dims := e.Models.EmbedDims
		// Read metadata before the loaded index: a leaf rebuild can establish a new width while the
		// previous snapshot is still serving searches. Never infer the width from old message rows.
		if e.Store != nil {
			lo, hi, err := e.Store.LeafVectorWidths(e.EmbedSpace(), e.Settings.Recipe.Version())
			if err != nil {
				return 0, "", err
			}
			if lo != hi {
				return 0, "leaf_width_mixed", nil
			}
			if lo > 0 {
				dims = lo // stored width wins even when the provider returned less than explicit dims
			}
		} else if ix := e.Index(); ix != nil && ix.Space == e.EmbedSpace() {
			dims = ix.LeafVecs.Dims
		}
		e.messageWidthMu.Lock()
		expected, returned := e.messageShortExpected, e.messageShortReturned
		e.messageWidthMu.Unlock()
		if expected > 0 && expected == dims {
			return 0, fmt.Sprintf("provider_width_short:%d/%d", returned, expected), nil
		}
		if dims <= 0 {
			return 0, "leaf_width_unknown", nil
		}
		return dims, "", nil
	}
	if d := e.Models.EmbedDims; d > 0 && d <= defaultMaxMessageDims {
		return d, "", nil
	}
	return defaultMaxMessageDims, "", nil
}

// MessageVectorWaitReason explains why messages are not embedded; it contains only widths and codes.
func (e *Engine) MessageVectorWaitReason() (string, error) {
	_, reason, err := e.messageVectorState()
	return reason, err
}

func (e *Engine) observeLeafWidth(dims int) {
	if e.Models.EmbedTruncatable || dims <= 0 {
		return
	}
	e.leafReturnDims.Store(int64(dims))
}

// MessageVectorDims reports the message width. Zero means no leaf has established the width yet.
func (e *Engine) MessageVectorDims() int { return e.messageDims() }

// MessageSpace identifies message vectors that are comparable: a change of model or dimensionality
// starts a new space and the messages are embedded again.
func (e *Engine) MessageSpace() string {
	return e.messageSpaceAt(e.messageDims())
}

func (e *Engine) messageSpaceAt(d int) string {
	if d > 0 {
		return fmt.Sprintf("%s:%d:l2", e.Models.Embed, d)
	}
	return "" // unresolved; no vector may be saved or loaded in this space
}

// fitMessageVec matches the selected message width and normalises it. A non-MRL model follows the
// installation's leaf width, including existing explicit dimensions (see BACKLOG WIDTH).
func (e *Engine) fitMessageVec(v []float32) []float32 {
	return fitMessageVec(v, e.messageDims())
}

func fitMessageVec(v []float32, d int) []float32 {
	if d > 0 && len(v) > d {
		v = v[:d]
	}
	search.Normalize(v)
	return v
}

// messageSaveChunk is how many message vectors are written to the store in one transaction.
const messageSaveChunk = 500

// embedMessages embeds the pending messages in batches and stores them. It follows embedLeaves: batch size EmbedBatch, Workers batches in flight, the budget checked
// before each batch, the provider-reported cost recorded as "embed" spend.
func (e *Engine) embedMessages(ctx context.Context, items []store.PendingMessage, space string, b *budget, progress Progress) (int, string) {
	dims, wait, err := e.messageVectorState()
	if err != nil {
		return 0, "store_error"
	}
	if dims == 0 || space == "" {
		if wait != "" && wait != "leaf_width_unknown" {
			return 0, "message_width_wait"
		}
		return 0, "" // wait for the first leaf embedding; do not call the provider
	}
	batch := e.Settings.EmbedBatch
	var batches [][]store.PendingMessage
	for i := 0; i < len(items); i += batch {
		batches = append(batches, items[i:min(len(items), i+batch)])
	}
	var mu sync.Mutex
	var failed, storeFailed, shortWidth atomic.Bool
	done, stop := 0, ""
	sem := make(chan struct{}, e.Settings.Workers)
	var wg sync.WaitGroup
	// stopped re-checks everything that can end the run. It runs before a batch is queued and again once a
	// worker slot is held: waiting for a slot is when an earlier batch finishes, spends the budget or fails,
	// or the run is cancelled, and a batch that has not started must not run after that.
	stopped := func() (bool, string) {
		switch {
		case failed.Load():
			return true, ""
		case shortWidth.Load():
			return true, ""
		case !b.ok():
			return true, "budget"
		case ctx.Err() != nil:
			return true, "cancelled"
		}
		return false, ""
	}
queue:
	for _, bt := range batches {
		if end, why := stopped(); end {
			stop = why
			break
		}
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			stop = "cancelled"
			break queue
		}
		if end, why := stopped(); end {
			<-sem
			stop = why
			break
		}
		wg.Add(1)
		go func(bt []store.PendingMessage) {
			defer wg.Done()
			defer func() { <-sem }()
			defer func() {
				if r := recover(); r != nil {
					_ = crash.Error(nil, "memory message embedding", r)
					failed.Store(true)
				}
			}()
			texts := messageInputs(bt)
			t0 := time.Now()
			res, err := e.embedDocs(ctx, texts, 0.00002*float64(len(texts)))
			if err != nil || len(res.Vectors) != len(texts) {
				failed.Store(true)
				return
			}
			if len(texts) == batch {
				e.observe(time.Since(t0), 0)
			}
			b.add(res.CostUSD, 0.00002*float64(len(texts)))
			e.spend("embed", res.CostUSD, 0.00002*float64(len(texts)))
			vecs := make([]store.MessageVector, len(bt))
			for i, p := range bt {
				if !e.Models.EmbedTruncatable && len(res.Vectors[i]) < dims {
					// Keep the batch pending without retrying it against an unchanged leaf width.
					returned := len(res.Vectors[i])
					e.messageWidthMu.Lock()
					e.messageShortExpected, e.messageShortReturned = dims, returned
					e.messageWidthMu.Unlock()
					shortWidth.Store(true)
					return
				}
				vecs[i] = store.MessageVector{ObservationID: p.ObservationID, Space: space, TextHash: p.TextHash,
					Vec: search.EncodeQuantizedRow(fitMessageVec(res.Vectors[i], dims))}
			}
			saved, err := e.Store.SaveMessageVectors(vecs)
			if err != nil {
				storeFailed.Store(true) // the messages stay pending and are embedded again on a later pass
			}
			mu.Lock()
			done += saved
			progress("messages", done, len(items))
			mu.Unlock()
		}(bt)
	}
	wg.Wait()
	if failed.Load() && stop == "" {
		stop = "provider_error"
	}
	if shortWidth.Load() && stop == "" {
		stop = "message_width_wait"
	}
	if storeFailed.Load() && stop == "" {
		stop = "store_error"
	}
	return done, stop
}

// messageInputs are the embedding inputs of a group of messages, live (embedMessages) and in a batch
// alike: each message's stored text (speaker label and content).
func messageInputs(bt []store.PendingMessage) []string {
	texts := make([]string, len(bt))
	for i, p := range bt {
		texts[i] = p.Text
	}
	return texts
}

type loadedMessageVectors struct {
	matrix              search.Matrix
	rowOf, leafOf       []int32
	rows                map[string]search.MessageVectorRow
	stale, reused, read int
	streamed            int
}

func messageRow(v store.MessageVector) search.MessageVectorRow {
	return search.MessageVectorRow{TextHash: v.TextHash, Label: v.Label, UpdatedAt: v.UpdatedAt,
		Format: v.Format, Bytes: v.Bytes, Row: -1}
}

// loadMessageVectors builds the int8 message matrix for the leaves of an index under construction. The
// matrix holds one row per stored message vector of the space whose message sits in one of lv's leaves,
// in no particular order; rowOf and leafOf are the (message, leaf) pairs in the canonical order of
// search.ProjectMessages — the leaf order, then the message order inside the leaf — so a message in
// several leaves (overlapping recipes) is projected onto every one of them and ties are settled by that
// order, never by observation ID. A vector that no longer describes its message (the scan joins the live
// message and compares the speaker label it was made with) is dropped, so a vector never describes text
// it was not made from, even behind a running engine's message snapshot. stale counts the dropped rows.
func (e *Engine) loadMessageVectors(rooms []string, lv []leaves.Leaf, prev *search.Index) (loadedMessageVectors, error) {
	var out loadedMessageVectors
	expectedDims, _, err := e.messageVectorState()
	if err != nil {
		return out, err
	}
	space := e.messageSpaceAt(expectedDims)
	if space == "" {
		return out, nil
	}
	_, expected, err := e.Store.MessageVectorCounts(space)
	if err != nil || expected == 0 {
		return out, err
	}
	if prev != nil && prev.MessageSpace != space {
		prev = nil
	}
	leafObs := make([][]string, len(lv))
	inLeaf := make(map[string]struct{}, len(lv)*8)
	for i, l := range lv {
		leafObs[i] = l.ObservationIDs
		for _, id := range l.ObservationIDs {
			inLeaf[id] = struct{}{}
		}
	}
	if len(inLeaf) == 0 {
		return out, nil
	}
	capacity := min(expected, len(inLeaf))
	out.rows = make(map[string]search.MessageVectorRow, capacity)
	var changed []string
	coldLoaded := prev == nil
	if coldLoaded {
		// Cold loads have nothing to share: read bytes and identity in one live
		// join instead of reading every identity twice. Keep only final int8 rows,
		// never a history-sized BLOB map; the projection below preserves ties.
		var scratch []float32
		err = e.Store.ScanColdMessageVectors(rooms, space, func(v store.MessageVector) error {
			if _, ok := inLeaf[v.ObservationID]; !ok {
				return nil
			}
			dims := 0
			switch v.Format {
			case store.MessageFormatInt8:
				dims = v.Bytes - 4
			case store.MessageFormatFloat16:
				if v.Bytes%2 == 0 {
					dims = v.Bytes / 2
				}
			}
			if !v.Current || dims <= 0 {
				out.stale++
				return nil
			}
			// Uniform configured widths are the normal case. If stored widths
			// differ, fall back to the ordered metadata path: it historically
			// chooses the first eligible observation's width, not the config's.
			if expectedDims > 0 && dims != expectedDims {
				return errColdMessageDims
			}
			if out.matrix.Dims == 0 {
				out.matrix = search.NewSharedQuantized(dims, capacity)
			}
			if dims != out.matrix.Dims {
				return errColdMessageDims
			}
			stamp := messageRow(v)
			stamp.Row = int32(out.matrix.Rows())
			if v.Format == store.MessageFormatInt8 {
				q, scale, _ := search.DecodeQuantizedRow(v.Vec, nil)
				out.matrix.ShareRowQ(q, scale)
			} else {
				if cap(scratch) < dims {
					scratch = make([]float32, dims)
				}
				out.matrix.AppendQuantized(search.DecodeFloat16(v.Vec, scratch[:dims]))
			}
			out.rows[v.ObservationID] = stamp
			out.read++
			out.streamed++
			return nil
		})
		if errors.Is(err, errColdMessageDims) {
			out = loadedMessageVectors{rows: make(map[string]search.MessageVectorRow, capacity)}
			coldLoaded = false
			err = nil
		}
	}
	if !coldLoaded {
		err = e.Store.ReadMessageVectors(func(reader *store.MessageVectorReader) error {
			err := reader.ScanMessageVectorMeta(rooms, space, func(v store.MessageVector) error {
				if _, ok := inLeaf[v.ObservationID]; !ok {
					return nil // its leaf is open, re-cut or not embedded
				}
				var dims int
				switch v.Format {
				case store.MessageFormatInt8:
					dims = v.Bytes - 4
				case store.MessageFormatFloat16:
					if v.Bytes%2 == 0 {
						dims = v.Bytes / 2
					}
				}
				if !v.Current || dims <= 0 {
					out.stale++
					return nil
				}
				if out.matrix.Dims == 0 {
					out.matrix = search.NewSharedQuantized(dims, capacity)
				}
				if dims != out.matrix.Dims {
					out.stale++
					return nil
				}
				stamp := messageRow(v)
				if prev != nil && prev.MsgVecs.Dims == dims {
					if old, ok := prev.MessageRows[v.ObservationID]; ok {
						row := old.Row
						old.Row = -1
						if old == stamp {
							q, scale := prev.MsgVecs.RowQ(int(row))
							stamp = prev.MessageRows[v.ObservationID] // share its strings too
							stamp.Row = int32(out.matrix.Rows())
							out.matrix.ShareRowQ(q, scale)
							out.rows[v.ObservationID] = stamp
							out.reused++
							return nil
						}
					}
				}
				out.rows[v.ObservationID] = stamp
				changed = append(changed, v.ObservationID)
				return nil
			})
			if err != nil {
				return err
			}
			var scratch []float32
			var q8 []int8
			for lo := 0; lo < len(changed); lo += 500 {
				ids := changed[lo:min(len(changed), lo+500)]
				fetched, err := reader.MessageVectors(ids, space)
				if err != nil {
					return err
				}
				for _, id := range ids {
					v, ok := fetched[id]
					stamp := out.rows[id]
					if !ok || !v.Current || messageRow(v) != stamp {
						delete(out.rows, id) // disappeared or changed since the metadata scan
						out.stale++
						continue
					}
					stamp.Row = int32(out.matrix.Rows())
					if v.Format == store.MessageFormatInt8 {
						var scale float32
						q8, scale, ok = search.DecodeQuantizedRow(v.Vec, q8)
						if !ok {
							delete(out.rows, id)
							out.stale++
							continue
						}
						out.matrix.AppendRowQ(q8, scale)
					} else {
						dims := out.matrix.Dims
						if cap(scratch) < dims {
							scratch = make([]float32, dims)
						}
						out.matrix.AppendQuantized(search.DecodeFloat16(v.Vec, scratch[:dims]))
					}
					out.rows[id] = stamp
					out.read++
				}
			}
			return nil
		})
	}
	if err != nil {
		return out, err
	}
	ids, leafOf := search.ProjectMessages(leafObs, func(id string) bool { _, ok := out.rows[id]; return ok })
	out.leafOf = leafOf
	out.rowOf = make([]int32, len(ids))
	for k, id := range ids {
		out.rowOf[k] = out.rows[id].Row
	}
	if len(out.rows) == 0 {
		out.rows = nil
	}
	return out, nil
}

// MessageEstimate previews turning message vectors on for rooms: how many messages would be embedded and
// what that would cost. It reads, calls no provider and works whether or not Settings.MessageVectors is
// on, so the console can show it before the operator decides.
type MessageEstimate struct {
	Messages   int     `json:"messages"` // to embed (paid)
	Tokens     int     `json:"tokens"`
	USD        float64 `json:"usd"`
	Seconds    int     `json:"seconds"`
	Dimensions int     `json:"dimensions"`  // 0 means full native width is not known until embedding
	DiskBytes  int64   `json:"disk_bytes"`  // quantized vector payload; excludes database row/index overhead
	PriceBasis string  `json:"price_basis"` // prices | fallback (the embedding model's price is unknown)
	// Unprepared is an upper bound of the messages the estimate cannot see yet: eligible messages that are
	// not in a closed leaf of the active recipe (text imported but not split into leaves, a recipe that
	// changed, the open tail). Preparing the blocks puts them into leaves and then embeds them, so they are
	// extra on top of Messages; the console says so instead of showing the figures as exact.
	Unprepared int `json:"unprepared"`
}

// EstimateMessageVectors previews the pending messages of the rooms (empty = every room).
func (e *Engine) EstimateMessageVectors(rooms []string) (MessageEstimate, error) {
	dims, _, err := e.messageVectorState()
	if err != nil {
		return MessageEstimate{}, err
	}
	space := e.messageSpaceAt(dims)
	pending, err := e.Store.PendingMessages(rooms, space, 0, e.Settings.Recipe.Version())
	if err != nil {
		return MessageEstimate{}, err
	}
	est := MessageEstimate{PriceBasis: "prices"}
	if est.Unprepared, err = e.Store.CountUnleafedMessages(rooms, e.Settings.Recipe.Version()); err != nil {
		return MessageEstimate{}, err
	}
	for _, p := range pending {
		est.Messages++
		est.Tokens += approxTokens(p.Text)
	}
	est.Dimensions = dims
	if est.Dimensions > 0 {
		est.DiskBytes = int64(est.Messages) * int64(est.Dimensions+4)
	}
	in, _, ok := e.price(e.Models.Embed)
	if !ok {
		in, est.PriceBasis = fallbackEmbedPerTok, "fallback"
	}
	est.USD = float64(est.Tokens) * in
	batches := (est.Messages + e.Settings.EmbedBatch - 1) / e.Settings.EmbedBatch
	est.Seconds = int(math.Ceil(math.Ceil(float64(batches)/float64(max(1, e.Settings.Workers))) * e.rates().EmbedBatchSec))
	return est, nil
}
