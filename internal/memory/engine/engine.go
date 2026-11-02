// Package engine is the product memory runtime: it owns the durable store, the loaded search index
// (rebuilt from the store and swapped atomically), query embedding with a cache and hedged requests,
// background indexing (leaf sync → embeddings → summaries) under a budget, imports and live capture.
//
// Model calls go through an OpenAI-compatible adapter (OpenRouter by default). Costs are recorded per
// call in memory_spend. Message bodies are never logged.
package engine

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"masamong/internal/lang"
	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/retrieval"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
	"masamong/internal/providers/openai"
)

// Models are the model IDs per role.
type Models struct {
	Embed            string
	EmbedDims        int  // 0 = model default; smaller keeps the first dims (MRL)
	EmbedTruncatable bool // the model is listed as Matryoshka/MRL and may be cut safely
	Rerank           string
	Summary          string
	Response         string
	Planner          string
}

// Settings are the runtime knobs (config.yaml `memory`).
type Settings struct {
	Search           search.Config
	Recipe           leaves.Recipe
	Summaries        bool
	Languages        []string       // corpus languages (lang packs); first is primary. Empty = lang.Default
	BotNames         []string       // the bot's own names: never person targets
	QueryInstruction string         // "" = the primary language pack's instruction
	SummaryPrompt    string         // "" = the primary language pack's prompt ({body})
	RefTZ            *time.Location // request clock zone (day boundaries)
	NaiveTZ          *time.Location // declared zone of naive (Kakao) times; nil = undeclared
	HedgeAfter       time.Duration
	EmbedBatch       int
	Workers          int
	// MessageVectors turns on the message-level index (memory.message_vectors): each message of a closed
	// leaf is embedded on its own, and search may use those vectors as one more candidate source
	// (search.Config Retrieval.MessageDense). The product turns it on unless memory.message_vectors is false;
	// off changes nothing.
	MessageVectors bool
	// MessageDims overrides the message-vector width for measurements only (tools/evaluation/memory_dims
	// -msgdims): it is no config setting and the product never sets it. 0 keeps the rule in messageDims.
	MessageDims int
	// ResidentRoomsForMeasurement is an independent runner knob only. nil keeps
	// the product's full residency; non-nil empty selects no rooms. Identity stays global.
	ResidentRoomsForMeasurement []string
}

// Engine is safe for concurrent use.
type Engine struct {
	Store *store.Store
	// LLM embeds (the embed role's provider). RerankLLM and SummaryLLM are the rerank and summary
	// roles' providers when those differ from it; nil means LLM serves that role too.
	LLM        *openai.Adapter
	RerankLLM  *openai.Adapter
	SummaryLLM *openai.Adapter
	Models     Models
	Settings   Settings
	// CallProviders names the configured provider for each memory model role. OnCall is optional and
	// receives one body-free record for each provider request; cache hits never reach it.
	CallProviders CallProviders
	OnCall        func(ctx context.Context, c CallInfo)
	// EmbedBatcher and SummaryBatcher replace the embed and summary roles' batch API (batch.go) when set
	// (tests); nil uses the role's adapter.
	EmbedBatcher, SummaryBatcher BatchClient
	// ResolveBatcher returns the batch client of the provider a submitted batch went to (by its config id,
	// address and family), for its polls, results, cancel and cleanup; nil uses the role's current client
	// when it is still that provider
	ResolveBatcher BatchResolver

	idx      atomic.Pointer[search.Index]
	loadedAt atomic.Int64
	reloadMu sync.Mutex
	indexMu  sync.Mutex
	// Observed fitted width repairs non-MRL leaves without changing their space names.
	leafReturnDims                             atomic.Int64
	messageWidthMu                             sync.Mutex
	messageShortExpected, messageShortReturned int
	qmu                                        sync.Mutex
	qcache                                     map[string][]float32
	qorder                                     []string
	dirtyRoom                                  sync.Map
	// OnDirty, when set, is called once when a captured message queues a room that was not waiting
	// yet, so the queue can outlive the process (set it before the engine is shared).
	OnDirty func(room string)
	rateMu  sync.Mutex
	rate    Rates
	// lastReload is how long the latest full index build took (it grows with stored history).
	lastReload atomic.Int64
	// lastReused is how many leaves that build copied from the previous index instead of reading them
	lastReused atomic.Int64
	// lastRead is the part of lastReload spent reading the database and filling the vector matrices
	// (the rest builds the full-text indexes and lookups)
	lastRead atomic.Int64
	// retired is set when a newer engine replaced this one (a config change): a rebuild still running
	// here must not install an index nobody will ever close.
	retired atomic.Bool
	// adapted is set once stored vectors of a larger dimensionality were moved to this space
	adaptMu sync.Mutex
	adapted bool
	// revised holds the latest changed revision per room. It survives reloads:
	// an in-flight search can still hold an older index after the swap.
	revised sync.Map
	// OnRevised, when set, is called with the rooms whose stored messages an edit or deletion changed,
	// so the caller can resync them soon (Resync); set it before the engine is shared.
	OnRevised func(rooms []string)
	closers   *indexClosers
	// lastMeasure is what the latest rebuild sampled at its stage boundaries (see ReloadMeasurement).
	lastMeasure atomic.Pointer[ReloadMeasurement]
	// OnReloadMeasurement, when set, receives a copy of every rebuild's stage-boundary samples as it
	// ends. nil (the product default) only keeps the latest one for ResidencyMeasurement; set it before
	// the engine is shared. It runs on the rebuilding goroutine: keep it short.
	OnReloadMeasurement func(ReloadMeasurement)
}

// New builds an engine. llm may be nil (offline): search then needs vectors provided by the caller.
func New(st *store.Store, llm *openai.Adapter, m Models, s Settings) *Engine {
	if len(s.Languages) == 0 {
		s.Languages = []string{lang.Default}
	}
	primary := lang.For(s.Languages...)
	if s.QueryInstruction == "" {
		s.QueryInstruction = primary.Memory.QueryInstruction
	}
	if s.SummaryPrompt == "" {
		s.SummaryPrompt = primary.Memory.SummaryPrompt
	}
	if s.Search.Language == nil {
		merged := lang.MergeSearch(s.Languages, s.BotNames)
		s.Search.Language = &merged
	}
	if s.Recipe.Size == 0 {
		s.Recipe = leaves.DefaultRecipe()
	}
	if s.EmbedBatch <= 0 {
		s.EmbedBatch = 32
	}
	if s.Workers <= 0 {
		s.Workers = 8
	}
	if s.HedgeAfter == 0 {
		s.HedgeAfter = 800 * time.Millisecond
	}
	return &Engine{Store: st, LLM: llm, Models: m, Settings: s, qcache: map[string][]float32{}, closers: &indexClosers{timers: map[*time.Timer]func(){}}}
}

// EmbedSpace identifies vectors that are comparable (model + dims + normalization).
func (e *Engine) EmbedSpace() string {
	return fmt.Sprintf("%s:%d:l2", e.Models.Embed, e.Models.EmbedDims)
}

// SpaceModel is the embedding model of a space ("model:dims:l2"; model names may hold colons).
func SpaceModel(space string) string {
	m, _, _ := splitSpace(space)
	return m
}

func splitSpace(space string) (model string, dims int, ok bool) {
	rest, found := strings.CutSuffix(space, ":l2")
	if !found {
		return space, 0, false
	}
	i := strings.LastIndexByte(rest, ':')
	if i < 0 {
		return rest, 0, false
	}
	d, err := strconv.Atoi(rest[i+1:])
	if err != nil {
		return rest, 0, false
	}
	return rest[:i], d, true
}

// adaptSpace runs once per engine, before its first load or index pass: when models.embed.dimensions
// went down, leaves stored for a larger dimensionality of the same model (or its native size) are
// truncated to this one instead of being embedded again.
func (e *Engine) adaptSpace() (int, error) {
	e.adaptMu.Lock()
	defer e.adaptMu.Unlock()
	dims := e.Models.EmbedDims
	if e.adapted || dims <= 0 || e.retired.Load() {
		return 0, nil
	}
	spaces, err := e.Store.Spaces()
	if err != nil {
		return 0, err
	}
	target := e.EmbedSpace()
	moved := 0
	for _, sp := range spaces {
		m, d, ok := splitSpace(sp)
		if !ok || m != e.Models.Embed || sp == target || (d != 0 && d < dims) {
			continue
		}
		n, err := e.Store.ShrinkSpace(sp, target, func(raw []byte) []byte { return shrinkVector(raw, dims) })
		moved += n
		if err != nil {
			return moved, err
		}
	}
	e.adapted = true
	return moved, nil
}

// shrinkVector keeps the first dims components of a float16 vector and re-normalizes it.
func shrinkVector(raw []byte, dims int) []byte {
	if len(raw)/2 <= dims {
		return raw
	}
	v := search.DecodeFloat16(raw[:2*dims], nil)
	search.Normalize(v)
	return search.EncodeFloat16(v)
}

// cachedDocument returns a stored embedding of text for this space: one kept when its leaf moved to
// another space, or (from earlier versions, keyed by model only) one fitted to this dimensionality.
func (e *Engine) cachedDocument(text string) ([]byte, bool) {
	if v, ok := e.Store.CachedVector(store.CacheKey(e.EmbedSpace(), "document", text)); ok {
		return v, true
	}
	v, ok := e.Store.CachedVector(store.CacheKey(e.Models.Embed, "document", text))
	if !ok {
		return nil, false
	}
	dims := e.Models.EmbedDims
	switch {
	case dims <= 0 || len(v) == 2*dims:
		return v, true
	case len(v) > 2*dims:
		return shrinkVector(v, dims), true
	}
	return nil, false
}

// Index returns the currently loaded index (nil before the first successful Reload).
func (e *Engine) Index() *search.Index { return e.idx.Load() }

// LastReloadRead is how long the latest build spent reading the database and filling the vector
// matrices; the rest of LastReload builds the full-text indexes and lookups in memory.
func (e *Engine) LastReloadRead() time.Duration { return time.Duration(e.lastRead.Load()) }

// LastReused is how many leaves the latest build reused from the index it replaced.
func (e *Engine) LastReused() int { return int(e.lastReused.Load()) }

// LoadedAt is when the current index was built.
func (e *Engine) LoadedAt() time.Time { return time.Unix(0, e.loadedAt.Load()) }

// Inherit takes over what the engine built for the previous config revision knew: rooms still
// waiting for a resync and, while the embedding space is unchanged, its loaded index until the next
// Reload replaces it. Without it a config change would drop pending resyncs and leave chat turns
// without memory until a full rebuild finished.
func (e *Engine) Inherit(old *Engine) {
	if old == nil {
		return
	}
	e.closers = old.closers
	if m := old.lastMeasure.Load(); m != nil && e.lastMeasure.Load() == nil {
		e.lastMeasure.Store(m)
	}
	old.dirtyRoom.Range(func(k, v any) bool {
		e.dirtyRoom.Store(k, v)
		return true
	})
	old.revised.Range(func(k, v any) bool {
		e.revised.Store(k, v)
		return true
	})
	if ix := old.idx.Load(); ix != nil && old.Settings.Recipe.Version() != e.Settings.Recipe.Version() {
		for room := range ix.RoomSigs {
			e.queueRoom(room)
		}
	}
	// an index built without message vectors is not handed to an engine that wants them (or the reverse):
	// its search config would keep the old setting until the next rebuild
	if ix := old.idx.Load(); ix != nil && old.EmbedSpace() == e.EmbedSpace() && ix.Recipe == e.Settings.Recipe.Version() &&
		old.Settings.MessageVectors == e.Settings.MessageVectors && old.MessageSpace() == e.MessageSpace() {
		e.idx.Store(ix)
		e.loadedAt.Store(old.loadedAt.Load())
	}
}

// Retire marks the engine as replaced by next (a config change). An index that next did not take over
// (Inherit shares it while the embedding space is unchanged) is closed once in-flight searches are
// done, and a rebuild still running here closes its result instead of installing it.
func (e *Engine) Retire(next *Engine) {
	if e == nil {
		return
	}
	e.retired.Store(true)
	// the index stays attached for turns that still hold this engine; it is closed after they finish
	if ix := e.idx.Load(); ix != nil && (next == nil || next.idx.Load() != ix) {
		e.closeLater(ix)
	}
}

var errRetired = errors.New("memory: engine replaced by a newer configuration")

// closeDelay is how long a replaced index stays open for searches that already hold it.
var closeDelay = time.Minute

// closeLater closes a replaced index's lexical databases after searches that already hold it have
// finished. It keeps only the closer, not the index: the vectors are freed as soon as those searches
// end, so two full indexes are not held for the whole delay.
type indexClosers struct {
	mu     sync.Mutex
	timers map[*time.Timer]func()
	closed bool
}

func (e *Engine) closeLater(ix *search.Index) {
	c, closeFn := e.closers, ix.Closer()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		closeFn()
		return
	}
	var timer *time.Timer
	timer = time.AfterFunc(closeDelay, func() { c.mu.Lock(); defer c.mu.Unlock(); closeFn(); delete(c.timers, timer) })
	c.timers[timer] = closeFn
}

// Close releases the current and retired lexical databases after the owner has stopped its work.
func (e *Engine) Close() {
	e.retired.Store(true)
	e.indexMu.Lock()
	defer e.indexMu.Unlock()
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()
	c := e.closers
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	for timer, closeFn := range c.timers {
		timer.Stop()
		closeFn()
		delete(c.timers, timer)
	}
	if ix := e.idx.Swap(nil); ix != nil {
		ix.Close()
	}
}

// loadMessages returns every message of the rooms in room order (as Store.Messages does). Messages are
// reused only while the durable room revision and message count match the loaded snapshot.
func (e *Engine) loadMessages(rooms []store.RoomStats) ([]corpus.Message, map[string]string, error) {
	sigs := make(map[string]string, len(rooms))
	var have map[string][]corpus.Message
	prev := e.idx.Load()
	if prev != nil && prev.RoomSigs != nil {
		have = prev.RoomMessages()
	}
	var names, changed []string
	isChanged := map[string]bool{}
	total, changedTotal := 0, 0
	for _, r := range rooms {
		sig := fmt.Sprintf("%d|%d", r.Messages, r.Revision)
		sigs[r.Room.Room] = sig
		names = append(names, r.Room.Room)
		total += r.Messages
		if prev == nil || prev.RoomSigs[r.Room.Room] != sig || len(have[r.Room.Room]) != r.Messages {
			changed = append(changed, r.Room.Room)
			changedTotal += r.Messages
			isChanged[r.Room.Room] = true
		}
	}
	fresh := map[string][]corpus.Message{}
	if len(changed) > 0 {
		msgs, err := e.Store.LoadMessages(changed, changedTotal)
		if err != nil {
			return nil, nil, err
		}
		// A cold load already has the complete ordered history. Return it directly
		// instead of making one more full-size copy merely to join changed rooms.
		if len(changed) == len(rooms) {
			return msgs, sigs, nil
		}
		for lo := 0; lo < len(msgs); {
			hi := lo + 1
			for hi < len(msgs) && msgs[hi].Room == msgs[lo].Room {
				hi++
			}
			fresh[msgs[lo].Room] = msgs[lo:hi]
			lo = hi
		}
	}
	sort.Strings(names) // Store.Messages orders rooms by name
	out := make([]corpus.Message, 0, total)
	for _, room := range names {
		if m, ok := fresh[room]; ok {
			out = append(out, m...)
		} else if !isChanged[room] {
			out = append(out, have[room]...)
		}
	}
	return out, sigs, nil
}

// leafStamp changes whenever a stored leaf's vectors can: new text, new summary, or a vector added,
// dropped or re-embedded at another size. Equal stamps let a rebuild reuse the loaded rows.
func leafStamp(l store.StoredLeaf) uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s\x1f%s\x1f%d\x1f%d", l.BodyHash, l.Summary, l.LeafLen, l.SummaryLen)
	return h.Sum64()
}

// EnsureLoaded builds the index if none is loaded. Callers that arrive while a build is running wait
// for it and reuse the result instead of each rebuilding the whole index.
func (e *Engine) EnsureLoaded() error {
	if e.idx.Load() != nil {
		return nil
	}
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()
	if e.idx.Load() != nil {
		return nil
	}
	_, err := e.reload()
	return err
}

// Reload rebuilds the in-memory index from every room's embedded leaves and swaps it in.
func (e *Engine) Reload() (map[string]int, error) {
	e.reloadMu.Lock()
	defer e.reloadMu.Unlock()
	return e.reload()
}

// LastReload is how long the latest index build took (0 before the first).
func (e *Engine) LastReload() time.Duration { return time.Duration(e.lastReload.Load()) }

func (e *Engine) reload() (map[string]int, error) {
	if e.retired.Load() {
		return nil, errRetired // its successor rebuilds; building here would be thrown away
	}
	rec := &reloadRecorder{started: time.Now()}
	info, err := e.reloadSampled(rec)
	switch {
	case err != nil:
		e.finishMeasurement(rec, ReloadFailed)
	case len(rec.samples) == 0 || rec.samples[len(rec.samples)-1].Stage != StagePublish:
		e.finishMeasurement(rec, ReloadDiscarded) // replaced while building: the result was closed, not published
	default:
		e.finishMeasurement(rec, ReloadPublished)
	}
	return info, err
}

// reloadSampled is the rebuild; rec takes a memory sample at each stage boundary.
func (e *Engine) reloadSampled(rec *reloadRecorder) (map[string]int, error) {
	started := time.Now()
	defer func() { e.lastReload.Store(int64(time.Since(started))) }()
	if _, err := e.adaptSpace(); err != nil {
		return nil, err
	}
	rooms, err := e.Store.Rooms()
	if err != nil {
		return nil, err
	}
	var names []string
	for _, r := range rooms {
		names = append(names, r.Room.Room)
		if r.Messages > 0 && r.ActiveRecipe != e.Settings.Recipe.Version() {
			// Upgraded mixed indexes and changed recipes need a recut even
			// when no new message arrives in an otherwise idle room.
			e.queueRoom(r.Room.Room)
		}
	}
	loadRooms := rooms
	if e.Settings.ResidentRoomsForMeasurement != nil {
		selected := map[string]bool{}
		for _, room := range e.Settings.ResidentRoomsForMeasurement {
			selected[room] = true
		}
		loadRooms = nil
		for _, room := range rooms {
			if selected[room.Room.Room] {
				loadRooms = append(loadRooms, room)
			}
		}
	}
	msgs, roomSigs, err := e.loadMessages(loadRooms)
	if err != nil {
		return nil, err
	}
	space := e.EmbedSpace()
	var identityStarted time.Time
	if e.Settings.ResidentRoomsForMeasurement != nil {
		identityStarted = time.Now()
	}
	stored, err := e.Store.LoadLeafMeta(names, space, e.Settings.Recipe.Version())
	if err != nil {
		return nil, err
	}
	var out []placedLeaf
	stale, dims := 0, 0
	if e.Settings.ResidentRoomsForMeasurement == nil {
		out, stale, dims = placeLeafMeta(msgs, stored)
	}
	var identity *search.IdentityInputs
	identityPrepare, identityRows := 0, 0
	if e.Settings.ResidentRoomsForMeasurement != nil {
		globalMsgs, err := e.Store.LoadIdentityInputs(names)
		if err != nil {
			return nil, err
		}
		globalPlaced, _, globalDims := placeLeafMeta(globalMsgs, stored)
		dims = globalDims
		var eligible []leaves.Leaf
		for _, p := range globalPlaced {
			if p.vecLen == 2*dims {
				eligible = append(eligible, p.l)
			}
		}
		identity = search.NewIdentityInputs(globalMsgs, eligible)
		identityRows = len(globalMsgs)
		identityPrepare = int(time.Since(identityStarted).Microseconds())
		want := map[string]bool{}
		for _, room := range e.Settings.ResidentRoomsForMeasurement {
			want[room] = true
		}
		var resident []store.RoomStats
		names = nil
		for _, room := range rooms {
			if want[room.Room.Room] {
				resident = append(resident, room)
				names = append(names, room.Room.Room)
			}
		}
		rooms = resident
		var residentMeta []store.StoredLeaf
		for _, sl := range stored {
			if want[sl.Room] {
				residentMeta = append(residentMeta, sl)
			}
		}
		out, stale, _ = placeLeafMeta(msgs, residentMeta)
	}
	if dims > 0 && !e.Models.EmbedTruncatable {
		// Share the uniform-width resolver with messages. While mixed, leaf search retains
		// its historical first-Start width and messages wait for leaf repair.
		uniform, err := e.Store.LeafVectorDims(space, e.Settings.Recipe.Version())
		if err != nil {
			return nil, err
		}
		if uniform > 0 {
			dims = uniform
		}
	}
	wantSum := func(p placedLeaf) bool { return e.Settings.Summaries && p.sum != "" && p.svLen == 2*dims }

	// Rows of leaves the loaded index already holds, unchanged, are shared with it; only new or changed
	// leaves' vectors are read from the database.
	prev := e.idx.Load()
	if prev != nil && (prev.Space != space || prev.Stamps == nil || prev.LeafVecs.Dims != dims) {
		prev = nil
	}
	reusable := func(p placedLeaf) (leafOK, sumOK bool) {
		if prev == nil || prev.Stamps[p.l.DocID] != p.stamp {
			return false, false
		}
		_, _, leafOK = prev.QuantizedLeaf(p.l.DocID)
		_, _, sumOK = prev.QuantizedSummary(p.l.DocID)
		return leafOK, sumOK
	}

	var lv []leaves.Leaf
	var leafM, sumM search.Matrix
	var sums []search.Summary
	// The matrices are the bulk of the index. They hold int8 rows (4096 dims: 4 KB per leaf and per
	// summary instead of 16 KB as float32), sized up front so append never reserves twice that.
	if len(out) > 0 {
		lv = make([]leaves.Leaf, 0, len(out))
		leafM = search.NewSharedQuantized(dims, len(out))
		withSum := 0
		for _, p := range out {
			if wantSum(p) {
				withSum++
			}
		}
		if withSum > 0 {
			sums = make([]search.Summary, 0, withSum)
			sumM = search.NewSharedQuantized(dims, withSum)
		}
	}
	var scratch []float32 // decoded once, quantized into the matrix: no per-leaf garbage during a rebuild
	decode := func(raw []byte) []float32 {
		if n := len(raw) / 2; cap(scratch) < n {
			scratch = make([]float32, n)
		}
		return search.DecodeFloat16(raw, scratch[:len(raw)/2])
	}
	stamps := make(map[string]uint64, len(out))
	reused, fetchPeak := 0, 0
	// A cold build needs every row, so stream the table once instead of seeking
	// each leaf's BLOB by its random hash ID. Only quantized final rows survive a
	// batch; sorting/placement below still defines precisely the old search order.
	var coldRows map[string][2]search.QuantizedRow
	if prev == nil && len(out) > 0 {
		wanted := make(map[string]bool, len(out))
		for _, p := range out {
			if p.vecLen == 2*dims {
				wanted[p.l.DocID] = wantSum(p)
			}
		}
		coldRows = make(map[string][2]search.QuantizedRow, len(wanted))
		err := e.Store.ScanLeafVectorBatches(names, space, e.Settings.Recipe.Version(), func(batch []store.StoredLeaf) error {
			fetchPeak = max(fetchPeak, len(batch))
			var raw [][]byte
			var ids []string
			var channels []int
			for _, v := range batch {
				summary, ok := wanted[v.LeafID]
				if !ok {
					continue
				}
				for ch, blob := range [2][]byte{v.LeafVec, v.SummaryVec} {
					if len(blob) == 2*dims && (ch == 0 || summary) {
						raw = append(raw, blob)
						ids = append(ids, v.LeafID)
						channels = append(channels, ch)
					}
				}
			}
			for i, row := range search.QuantizeFloat16Rows(raw) {
				pair := coldRows[ids[i]]
				pair[channels[i]] = row
				coldRows[ids[i]] = pair
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	// Keep float16 blobs for at most one batch, including the first load. Reading all blobs into
	// one map before quantizing doubled the vector footprint even when there was no old index.
	for lo := 0; lo < len(out); lo += 500 {
		batch := out[lo:min(len(out), lo+500)]
		var fetch []string
		for _, p := range batch {
			leafOK, sumOK := reusable(p)
			if coldRows == nil && p.vecLen == 2*dims && (!leafOK || (wantSum(p) && !sumOK)) {
				fetch = append(fetch, p.l.DocID)
			}
		}
		fetchPeak = max(fetchPeak, len(fetch))
		fetched, err := e.Store.LeafVectors(fetch)
		if err != nil {
			return nil, err
		}
		for _, p := range batch {
			id := p.l.DocID
			leafOK, sumOK := reusable(p)
			vecs := fetched[id]
			switch {
			case p.vecLen != 2*dims:
				stale++
				continue
			case leafOK:
				q, sc, _ := prev.QuantizedLeaf(id)
				leafM.ShareRowQ(q, sc)
				reused++
			case coldRows != nil && len(coldRows[id][0].Q) == dims:
				row := coldRows[id][0]
				leafM.ShareRowQ(row.Q, row.Scale)
			case len(vecs[0]) == 2*dims:
				leafM.AppendQuantized(decode(vecs[0]))
			default:
				stale++ // changed since the metadata was read; the next rebuild picks it up
				continue
			}
			lv = append(lv, p.l)
			stamps[id] = p.stamp
			// a summary vector from another space (another dimensionality) is left out, never mixed in
			if sumM.Dims > 0 && wantSum(p) {
				if sumOK {
					q, sc, _ := prev.QuantizedSummary(id)
					sumM.ShareRowQ(q, sc)
					sums = append(sums, search.Summary{DocID: id, Text: p.sum})
				} else if coldRows != nil && len(coldRows[id][1].Q) == dims {
					row := coldRows[id][1]
					sumM.ShareRowQ(row.Q, row.Scale)
					sums = append(sums, search.Summary{DocID: id, Text: p.sum})
				} else if len(vecs[1]) == 2*dims {
					sumM.AppendQuantized(decode(vecs[1]))
					sums = append(sums, search.Summary{DocID: id, Text: p.sum})
				}
			}
		}
	}
	// WHY release before Build: matrices now own/share the immutable rows; the
	// cold lookup map must not stay live across FTS and message-vector allocation.
	streamedLeafVectors := len(coldRows)
	coldRows = nil
	e.lastReused.Store(int64(reused))
	e.lastRead.Store(int64(time.Since(started)))
	rec.sample(StageRead)
	cfg := e.Settings.Search
	cfg.SummaryEnabled = e.Settings.Summaries && len(sums) > 0
	if !cfg.SummaryEnabled {
		sums, sumM = nil, search.Matrix{}
	}
	var ix *search.Index
	if identity == nil {
		ix, err = search.Build(cfg, msgs, lv, leafM, sums, sumM)
	} else {
		ix, err = search.BuildWithIdentity(cfg, msgs, lv, leafM, sums, sumM, identity)
	}
	if err != nil {
		return nil, err
	}
	rec.sample(StageBuild)
	info := map[string]int{"rooms": len(names), "messages": len(msgs), "leaves": len(lv), "summaries": len(sums), "stale": stale, "vector_fetch_peak": fetchPeak, "leaf_vectors_streamed": streamedLeafVectors}
	if identity != nil {
		info["identity_prepare_us"], info["identity_input_rows"] = identityPrepare, identityRows
	}
	if e.Settings.MessageVectors {
		loaded, err := e.loadMessageVectors(names, lv, e.idx.Load())
		if err == nil && loaded.matrix.Rows() > 0 {
			err = ix.SetMessageVectors(loaded.matrix, loaded.rowOf, loaded.leafOf)
		}
		if err != nil {
			ix.Close()
			return nil, err
		}
		ix.MessageSpace, ix.MessageRows = e.MessageSpace(), loaded.rows
		info["message_vectors"], info["message_stale"] = loaded.matrix.Rows(), loaded.stale
		info["message_vectors_reused"], info["message_vectors_read"] = loaded.reused, loaded.read
		info["message_vectors_streamed"] = loaded.streamed
		rec.sample(StageMessageVectors)
	}
	ix.Space, ix.Stamps, ix.RoomSigs = space, stamps, roomSigs
	ix.Recipe = e.Settings.Recipe.Version()
	ix.RoomRevisions = make(map[string]int64, len(rooms))
	for _, r := range rooms {
		ix.RoomRevisions[r.Room.Room] = r.Revision
	}
	if e.retired.Load() {
		// replaced while building: the successor has its own index. Each index holds in-memory SQLite
		// databases (a goroutine and memory each), so an unused one is closed, not dropped.
		ix.Close()
		return info, nil
	}
	rec.sample(StagePublish)
	if old := e.idx.Swap(ix); old != nil {
		e.closeLater(old)
	}
	e.loadedAt.Store(time.Now().UnixNano())
	return info, nil
}

// ---------------------------------------------------------------------------
// Query embedding: memory LRU → DB cache → provider (hedged)
// ---------------------------------------------------------------------------

func (e *Engine) queryInput(text string) string {
	if strings.Contains(strings.ToLower(e.Models.Embed), "qwen3-embedding") {
		return "Instruct: " + e.Settings.QueryInstruction + "\nQuery: " + strings.TrimSpace(text)
	}
	return text
}

func (e *Engine) spend(kind string, cost *float64, fallback float64) {
	v := fallback
	if cost != nil {
		v = *cost
	}
	_ = e.Store.AddSpend(kind, v, 1)
}

// EmbedQuery returns a normalized query vector (implements search.QueryEmbedder).
func (e *Engine) EmbedQuery(ctx context.Context, text string) ([]float32, error) {
	// keyed by space and stored at its dimensionality: a quarter of the size at 1024 of 4096 dims
	key := store.CacheKey(e.EmbedSpace(), "query:"+e.Settings.QueryInstruction, text)
	e.qmu.Lock()
	if v, ok := e.qcache[key]; ok {
		e.qmu.Unlock()
		return append([]float32(nil), v...), nil
	}
	e.qmu.Unlock()
	if raw, ok := e.Store.CachedVector(key); ok {
		v := search.DecodeFloat16(raw, nil)
		e.remember(key, v)
		return append([]float32(nil), v...), nil
	}
	if e.LLM == nil || !e.LLM.Configured() {
		return nil, errors.New("memory: embedding provider not configured")
	}
	v, err := e.hedgedEmbed(ctx, e.queryInput(text))
	if err != nil {
		return nil, err
	}
	v = e.fitDims(v)
	search.Normalize(v)
	e.remember(key, v)
	_ = e.Store.PutQueryVectors(map[string][]byte{key: search.EncodeFloat16(v)})
	return append([]float32(nil), v...), nil
}

func (e *Engine) remember(key string, v []float32) {
	e.qmu.Lock()
	defer e.qmu.Unlock()
	if _, ok := e.qcache[key]; !ok {
		e.qorder = append(e.qorder, key)
	}
	e.qcache[key] = v
	for len(e.qorder) > 512 {
		delete(e.qcache, e.qorder[0])
		e.qorder = e.qorder[1:]
	}
}

// hedgedEmbed sends one request and, if it has not answered within HedgeAfter, a duplicate; the first
// success wins (provider latency varies 0.5–20s for the same input; a duplicate costs ~nothing).
func (e *Engine) hedgedEmbed(ctx context.Context, input string) ([]float32, error) {
	ctx, cancel := context.WithCancel(ctx)
	called := e.LLM != nil && e.LLM.Configured() && e.Models.Embed != ""
	var calls sync.WaitGroup
	defer func() { cancel(); calls.Wait() }()
	type res struct {
		v   []float32
		err error
	}
	ch := make(chan res, 2)
	call := func() {
		defer calls.Done()
		began := time.Now()
		r, err := e.LLM.Embed(ctx, e.Models.Embed, []string{input})
		latency := r.Elapsed
		if latency <= 0 {
			latency = time.Since(began)
		}
		if called {
			e.recordCall(ctx, "embed", e.Models.Embed, e.CallProviders.Embed, r.CostUSD, 0.00001, latency, r.PromptTokens, 0, 0, err)
		}
		if err == nil {
			e.spend("embed", r.CostUSD, 0.00001)
			ch <- res{v: r.Vectors[0]}
			return
		}
		ch <- res{err: err}
	}
	calls.Add(1)
	go call()
	pending := 1
	timer := time.NewTimer(e.Settings.HedgeAfter)
	defer timer.Stop()
	var lastErr error
	for pending > 0 {
		select {
		case <-timer.C:
			calls.Add(1)
			go call()
			pending++
		case r := <-ch:
			pending--
			if r.err == nil {
				return e.fitDims(r.v), nil
			}
			lastErr = r.err
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, lastErr
}

func (e *Engine) fitDims(v []float32) []float32 {
	if d := e.Models.EmbedDims; d > 0 && len(v) > d {
		v = v[:d]
	}
	return v
}

// ---------------------------------------------------------------------------
// Search
// ---------------------------------------------------------------------------

// rerankLLM and summaryLLM are the adapters of those roles.
func (e *Engine) rerankLLM() *openai.Adapter {
	if e.RerankLLM != nil {
		return e.RerankLLM
	}
	return e.LLM
}

func (e *Engine) summaryLLM() *openai.Adapter {
	if e.SummaryLLM != nil {
		return e.SummaryLLM
	}
	return e.LLM
}

type reranker struct{ e *Engine }

// Available is false where the rerank role's provider has no rerank endpoint (OpenAI, Gemini): search
// then ranks by fusion instead of failing a call per question.
func (r reranker) Available(remaining float64) bool {
	return RerankUsable(r.e.rerankLLM(), r.e.Models.Rerank)
}

// RerankUsable is shared by runtime availability and configuration recommendations.
func RerankUsable(a *openai.Adapter, model string) bool {
	return a != nil && a.Configured() && a.CanRerank() && model != ""
}

func (r reranker) Rerank(ctx context.Context, q string, docs []string, topN int) (search.RerankResult, error) {
	a := r.e.rerankLLM()
	called := a != nil && a.Configured() && a.CanRerank() && r.e.Models.Rerank != "" && strings.TrimSpace(q) != "" && len(docs) > 0 && topN >= 1 && topN <= len(docs)
	if a == nil {
		return search.RerankResult{}, errors.New("memory: rerank provider not configured")
	}
	began := time.Now()
	res, err := a.Rerank(ctx, r.e.Models.Rerank, q, docs, topN)
	if called {
		latency := res.Elapsed
		if latency <= 0 {
			latency = time.Since(began)
		}
		eCost := res.CostUSD
		tokens := res.TotalTokens
		r.e.recordCall(ctx, "rerank", r.e.Models.Rerank, r.e.CallProviders.Rerank, eCost, 0.0005, latency, tokens, 0, 0, err)
	}
	if err != nil {
		return search.RerankResult{}, err
	}
	r.e.spend("rerank", res.CostUSD, 0.0005)
	model := res.Model
	if model == "" {
		model = r.e.Models.Rerank
	}
	cost := 0.0
	if res.CostUSD != nil {
		cost = *res.CostUSD
	}
	return search.RerankResult{Indices: res.Indices, Scores: res.Scores, Model: model, CostUSD: cost, Latency: res.Elapsed}, nil
}

// SearchRequest is one memory search from a channel.
type SearchRequest struct {
	Query        string
	Intent       search.Intent
	Rooms        []string // scope: the channel's room plus attached blocks
	Ranking      retrieval.RankingMode
	TopK         int
	RemainingUSD float64
	Now          time.Time
	Exclude      []string // leaf ids an operator excluded (memory.review)
}

// ErrNotReady means no index is loaded yet.
var ErrNotReady = errors.New("memory: index not loaded")

// Search embeds q0 and runs the retriever inside the scope.
func (e *Engine) Search(ctx context.Context, req SearchRequest) (search.Result, error) {
	if e.Store.DB.HasPendingEvents("source-change") {
		return search.Result{}, ErrNotReady
	}

	ix := e.Index()
	if ix == nil || ix.Recipe != e.Settings.Recipe.Version() {
		return search.Result{}, ErrNotReady
	}
	vec, err := e.EmbedQuery(ctx, req.Query)
	if err != nil {
		return search.Result{}, err
	}
	nowT := req.Now
	if nowT.IsZero() {
		nowT = time.Now()
	}
	tz := e.Settings.RefTZ
	if tz == nil {
		tz = time.UTC
	}
	ref := &search.TimeReference{Mode: "live", Now: search.Moment{T: nowT.In(tz), Aware: true}, TZ: tz, NaiveTZ: e.Settings.NaiveTZ, Source: "request"}
	res, err := ix.Retrieve(ctx, search.Request{
		Query: req.Query, QueryVector: vec, Intent: req.Intent, Ranking: req.Ranking, Scope: search.Scope{Rooms: req.Rooms, Exclude: setOf(req.Exclude)},
		TopK: req.TopK, RemainingUSD: req.RemainingUSD, TimeReference: ref,
	}, e, reranker{e})
	if err == nil {
		// Recheck the durable revisions after model calls. This also covers
		// direct store mutations and the interval before OnRevised is delivered.
		rooms := map[string]bool{}
		for _, h := range res.Hits {
			rooms[h.Room] = true
		}
		var names []string
		for room := range rooms {
			names = append(names, room)
		}
		revisions, checkErr := e.Store.RoomEvidenceRevisions(names, ix.Recipe)
		if checkErr != nil {
			return search.Result{}, checkErr
		}
		out := res.Hits[:0]
		for _, h := range res.Hits {
			if revision, ok := revisions[h.Room]; ok && revision <= ix.RoomRevisions[h.Room] {
				out = append(out, h)
			}
		}
		res.Hits = e.dropRevised(out, ix)
	}
	return res, err
}

// dropRevised suppresses rooms changed after the search's snapshot and renumbers
// the rest. Guards remain so searches holding a retired snapshot stay protected.
func (e *Engine) dropRevised(hits []search.Hit, snapshot ...*search.Index) []search.Hit {
	ix := e.Index()
	if len(snapshot) > 0 {
		ix = snapshot[0]
	}
	out := hits[:0]
	for _, h := range hits {
		revision, changed := e.revised.Load(h.Room)
		if !changed || (ix != nil && ix.RoomRevisions[h.Room] >= revision.(int64)) {
			h.Rank = len(out) + 1
			out = append(out, h)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Imports and live capture
// ---------------------------------------------------------------------------

// ImportRaw loads a masamong-raw-observation/v1 file (rooms nil = all rooms in the file).
func (e *Engine) ImportRaw(path string, rooms map[string]bool, merge map[string]string) (store.InsertResult, error) {
	return e.ImportRawContext(context.Background(), path, rooms, merge)
}

func (e *Engine) ImportRawContext(ctx context.Context, path string, rooms map[string]bool, merge map[string]string) (store.InsertResult, error) {
	if err := corpus.ValidateMerge(merge); err != nil {
		return store.InsertResult{}, err
	}
	var total store.InsertResult
	var batch []corpus.Observation
	batchBytes := 0
	flush := func() error {
		r, err := e.Store.InsertObservationsContext(ctx, batch, merge)
		total.Seen += r.Seen
		total.Inserted += r.Inserted
		total.Duplicate += r.Duplicate
		total.Skipped += r.Skipped
		batch = batch[:0]
		batchBytes = 0
		return err
	}
	err := corpus.ReadRawCorpusContext(ctx, path, rooms, func(o corpus.Observation) error {
		batch = append(batch, o)
		batchBytes += observationBytes(o)
		if len(batch) >= 256 || batchBytes >= 2<<20 {
			return flush()
		}
		return nil
	})
	if err != nil {
		return total, err
	}
	return total, flush()
}

// ImportKakaoCSV loads one KakaoTalk CSV export as one room. tz declares the room's naive zone.
// exportLocale is the KakaoTalk client language of the export ("" = corpus.DefaultMarkerLocale).
func (e *Engine) ImportKakaoCSV(path, collection, room, label, tz, exportLocale string) (store.InsertResult, error) {
	return e.ImportKakaoCSVContext(context.Background(), path, collection, room, label, tz, exportLocale)
}

// Completed batches remain committed on failure/cancellation. Reimporting the same bytes skips
// their stable observation IDs and resumes; the active batch is atomic and never counted on rollback.
func (e *Engine) ImportKakaoCSVContext(ctx context.Context, path, collection, room, label, tz, exportLocale string) (store.InsertResult, error) {
	if exportLocale == "" {
		exportLocale = corpus.DefaultMarkerLocale
	}
	if !strings.HasPrefix(room, "kakao:") {
		room = "kakao:" + room
	}
	var total store.InsertResult
	batch := make([]corpus.Observation, 0, 256)
	batchBytes := 0
	flush := func() error {
		if len(batch) == 0 {
			return ctx.Err()
		}
		r, err := e.Store.InsertObservationsContext(ctx, batch, nil)
		total.Seen += r.Seen
		total.Inserted += r.Inserted
		total.Duplicate += r.Duplicate
		total.Skipped += r.Skipped
		batch = batch[:0]
		batchBytes = 0
		if err != nil {
			return err
		}
		return e.Store.DB.WithWriterContext(ctx, func(tx *sql.Tx) error {
			return e.Store.UpsertRoom(tx, store.Room{Room: room, Collection: collection, Platform: "kakao", Label: label, NaiveTimezone: tz})
		})
	}
	_, _, err := corpus.ReadKakaoCSVContext(ctx, path, collection, room, exportLocale, func(o corpus.Observation) error {
		batch = append(batch, o)
		batchBytes += observationBytes(o)
		if len(batch) >= 256 || batchBytes >= 2<<20 {
			return flush()
		}
		return nil
	})
	if err != nil {
		return total, err
	}
	return total, flush()
}

func observationBytes(o corpus.Observation) int {
	return len(o.ObservationID) + len(o.Platform) + len(o.Collection) + len(o.SourceRoom) + len(o.NativeMessageID) +
		len(o.ActorLabel) + len(o.ActorAccount) + len(o.Content) + len(o.ParsedTime)
}

// LiveMessage is one message captured from the gateway.
type LiveMessage struct {
	GuildID, ChannelID, MessageID string
	AuthorID, AuthorName          string
	Content                       string
	At                            time.Time
	Bot                           bool
}

// RoomKey is the Discord room key (discord:{guild}:{channel}; DMs use guild "0").
func RoomKey(guildID, channelID string) string {
	if guildID == "" {
		guildID = "0"
	}
	return "discord:" + guildID + ":" + channelID
}

// Capture stores one live message (idempotent by message ID) and marks the room for a leaf resync.
func (e *Engine) Capture(m LiveMessage) (bool, error) {
	return e.CaptureContext(context.Background(), m)
}

func (e *Engine) CaptureContext(ctx context.Context, m LiveMessage) (bool, error) {
	stored, err := e.capture(ctx, m)
	if err != nil {
		// The durable fallback must not depend on the failed/cancelled database transaction.
		if _, queueErr := e.Store.DB.EnqueueEvent("capture", m); queueErr != nil {
			return false, errors.Join(err, queueErr)
		}
	}
	return stored, err
}

func (e *Engine) capture(ctx context.Context, m LiveMessage) (bool, error) {
	room := RoomKey(m.GuildID, m.ChannelID)
	o := corpus.Observation{
		ObservationID: corpus.DiscordLiveObservationID(room, m.MessageID), Platform: "discord", Collection: "discord",
		SourceRoom: room, NativeMessageID: m.MessageID, ActorLabel: m.AuthorName, ActorAccount: m.AuthorID,
		Content: m.Content, ParsedTime: isoAware(m.At),
		RecordKind: corpus.Classify(m.Content, m.Bot, ""),
	}
	res, err := e.Store.InsertObservationsContext(ctx, []corpus.Observation{o}, nil)
	if err != nil {
		return false, err
	}
	if res.Inserted > 0 {
		e.queueRoom(room)
	}
	return res.Inserted > 0, nil
}

// RetryCaptures replays immutable events after a lock or crash. Insertion is idempotent by message ID
// and checks consent and deletion tombstones in the same writer transaction.
func (e *Engine) RetryCaptures(ctx context.Context) error {
	events, err := e.Store.DB.PendingEvents("capture")
	if err != nil {
		return err
	}
	for _, event := range events {
		var message LiveMessage
		if err := json.Unmarshal(event.Payload, &message); err != nil {
			return err
		}
		if err := e.Store.DB.ApplyEvent(ctx, "capture", event.ID, func() error {
			_, err := e.capture(ctx, message)
			return err
		}); err != nil {
			return err
		}
	}
	return nil
}

// Edit applies an author's edit of a captured Discord message: the stored text and the chat turn that
// quoted it change, the old text leaves search at once and the room is resynced (OnRevised). It
// reports whether anything stored changed.
func (e *Engine) Edit(messageID, content string, at time.Time) (bool, error) {
	return e.EditContext(context.Background(), messageID, content, at)
}

func (e *Engine) EditContext(ctx context.Context, messageID, content string, at time.Time) (bool, error) {
	rev, ok, err := e.Store.EditMessageContext(ctx, "discord", messageID, content, at.UTC().Format(time.RFC3339Nano))
	if err != nil || !ok {
		return false, err
	}
	e.revisedChanged([]store.Revision{rev})
	return true, nil
}

// Delete applies the deletion of captured Discord messages: their text is removed (a tombstone keeps
// an archive import from restoring it), they leave search at once and their rooms are resynced.
func (e *Engine) Delete(messageIDs []string, at time.Time) (int, error) {
	return e.DeleteContext(context.Background(), messageIDs, at)
}

func (e *Engine) DeleteContext(ctx context.Context, messageIDs []string, at time.Time) (int, error) {
	revs, err := e.Store.DeleteMessagesContext(ctx, "discord", messageIDs, at.UTC().Format(time.RFC3339Nano))
	if len(revs) > 0 {
		e.revisedChanged(revs)
	}
	return len(revs), err
}

// DeleteAccount removes every stored message of a Discord account from memory (consent withdrawn).
func (e *Engine) DeleteAccount(accountID string, at time.Time) (int, error) {
	revs, err := e.Store.DeleteAccountMessages("discord", accountID, at.UTC().Format(time.RFC3339Nano))
	if len(revs) > 0 {
		e.revisedChanged(revs)
	}
	return len(revs), err
}

func (e *Engine) DeleteObservations(ctx context.Context, ids []string, at time.Time) (int, error) {
	revs, err := e.Store.DeleteObservationsContext(ctx, ids, at.UTC().Format(time.RFC3339Nano))
	if len(revs) > 0 {
		e.revisedChanged(revs)
	}
	return len(revs), err
}

func (e *Engine) revisedChanged(revs []store.Revision) {
	rooms := map[string]bool{}
	for _, r := range revs {
		// Callbacks can arrive out of order after their writer commits.
		for {
			old, loaded := e.revised.LoadOrStore(r.Room, r.RoomRevision)
			if !loaded || old.(int64) >= r.RoomRevision || e.revised.CompareAndSwap(r.Room, old, r.RoomRevision) {
				break
			}
		}
		rooms[r.Room] = true
	}
	names := make([]string, 0, len(rooms))
	for r := range rooms {
		names = append(names, r)
		// the next index pass re-embeds the leaf that held the message
		e.queueRoom(r)
	}
	sort.Strings(names)
	if e.OnRevised != nil {
		e.OnRevised(names)
	}
}

// Resync re-cuts the rooms' leaves from their stored messages and rebuilds the loaded index, without
// any model call: a leaf whose text changed loses its vectors (the next index pass embeds it again) and
// leaves the index until then. It is how an edit or deletion reaches search.
func (e *Engine) Resync(rooms []string) error {
	e.indexMu.Lock()
	if e.retired.Load() {
		e.indexMu.Unlock()
		return nil
	}
	for _, room := range rooms {
		if _, err := e.Store.SyncLeaves(room, e.Settings.Recipe, time.Now(), e.Settings.NaiveTZ); err != nil {
			e.MarkDirty(rooms...)
			e.indexMu.Unlock()
			return err
		}
	}
	e.indexMu.Unlock()
	if e.Index() == nil {
		return nil
	}
	_, err := e.Reload()
	if errors.Is(err, errRetired) {
		return nil
	}
	return err
}

// MarkDirty queues rooms for the next resync again (a pass that failed or stopped early, or rooms a
// previous process left waiting). It does not call OnDirty: the caller owns that bookkeeping.
func (e *Engine) MarkDirty(rooms ...string) {
	for _, r := range rooms {
		e.dirtyRoom.Store(r, true)
	}
}

func (e *Engine) queueRoom(room string) {
	if _, queued := e.dirtyRoom.LoadOrStore(room, true); !queued && e.OnDirty != nil {
		e.OnDirty(room)
	}
}

// QueuedRooms lists rooms waiting for a resync without taking them.
func (e *Engine) QueuedRooms() []string {
	var out []string
	e.dirtyRoom.Range(func(k, _ any) bool {
		out = append(out, k.(string))
		return true
	})
	sort.Strings(out)
	return out
}

// DirtyRooms returns and clears rooms with new captured messages.
func (e *Engine) DirtyRooms() []string {
	if rooms, err := e.Store.PendingSyncRooms(); err == nil {
		e.MarkDirty(rooms...)
	}
	var out []string
	e.dirtyRoom.Range(func(k, _ any) bool {
		out = append(out, k.(string))
		e.dirtyRoom.Delete(k)
		return true
	})
	sort.Strings(out)
	return out
}

// isoAware renders like Python datetime.isoformat() for a UTC time (the raw corpus format).
func isoAware(t time.Time) string {
	t = t.UTC()
	if t.Nanosecond()/1000 != 0 {
		return t.Format("2006-01-02T15:04:05.000000+00:00")
	}
	return t.Format("2006-01-02T15:04:05+00:00")
}

func setOf(ids []string) search.Set {
	if len(ids) == 0 {
		return nil
	}
	out := search.Set{}
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

type placedLeaf struct {
	l             leaves.Leaf
	sum           string
	vecLen, svLen int
	stamp         uint64
}

// placeLeafMeta is the shared eligibility/ordering path. Dimensions must be
// selected before any resident-room filter, from the first globally placed row.
func placeLeafMeta(msgs []corpus.Message, stored []store.StoredLeaf) ([]placedLeaf, int, int) {
	pos := make(map[string]int, len(msgs))
	for i, m := range msgs {
		pos[m.ObservationID] = i
	}
	var out []placedLeaf
	stale := 0
	for _, sl := range stored {
		if len(sl.ObservationIDs) == 0 {
			stale++
			continue
		}
		start, ok := pos[sl.ObservationIDs[0]]
		end := start + len(sl.ObservationIDs)
		if !ok || end > len(msgs) || msgs[end-1].ObservationID != sl.ObservationIDs[len(sl.ObservationIDs)-1] {
			stale++ // messages were inserted inside the leaf since it was cut; a resync fixes it
			continue
		}
		out = append(out, placedLeaf{l: leaves.Leaf{DocID: sl.LeafID, Kind: "leaf", Start: start, End: end, Body: sl.Body, Room: sl.Room,
			ObservationIDs: sl.ObservationIDs, Speakers: sl.Speakers, N: len(sl.ObservationIDs)}, sum: sl.Summary,
			vecLen: sl.LeafLen, svLen: sl.SummaryLen, stamp: leafStamp(sl)})
	}
	sort.Slice(out, func(a, b int) bool { return out[a].l.Start < out[b].l.Start })
	dims := 0
	if len(out) > 0 {
		dims = out[0].vecLen / 2
	}
	return out, stale, dims
}
