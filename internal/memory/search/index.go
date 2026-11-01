package search

import (
	"errors"
	"sort"
	"sync"
	"sync/atomic"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
	"masamong/internal/memory/lexical"
)

// Set is a set of leaf IDs. nil means "no restriction" (the sandbox's None); an empty non-nil set
// means "nothing allowed". The two are never mixed up.
type Set map[string]struct{}

func (s Set) Has(id string) bool { _, ok := s[id]; return ok }

func setOf(ids ...string) Set {
	out := make(Set, len(ids))
	for _, id := range ids {
		out[id] = struct{}{}
	}
	return out
}

func intersect(a, b Set) Set {
	out := Set{}
	for id := range a {
		if b.Has(id) {
			out[id] = struct{}{}
		}
	}
	return out
}

// Summary is the derived one-line summary of a leaf (search signal only, never evidence).
type Summary struct {
	DocID string
	Text  string
}

// MessageVectorRow identifies the stored bytes behind an immutable message row. UpdatedAt also
// detects re-embedding the same text: equal text alone does not promise identical provider output.
type MessageVectorRow struct {
	TextHash, Label, UpdatedAt string
	Format, Bytes              int
	Row                        int32
}

// Index is one loaded memory index: room-ordered messages, their leaves, vectors, lexical indexes
// and time/identity lookups. It is immutable after Build except for lazily built caches.
type Index struct {
	Cfg      Config
	Messages []corpus.Message
	Leaves   []leaves.Leaf

	leafIDs  []string
	leafPos  map[string]int
	LeafVecs Matrix
	leafLex  *lexical.Index

	summaryIDs  []string
	summaryText map[string]string
	SummaryVecs Matrix
	summaryLex  *lexical.Index

	leafEndTS  map[string]float64
	byRecency  []string
	corpusTMax float64

	once      sync.Once
	lazyDone  atomic.Bool // set when once finished: the footprint estimate reads the lazy tables only then
	identity  *identityTable
	spkOrder  []string
	spkLeaves map[string][]string
	msgByObs  map[string]int

	rules *textRules

	// Space and Stamps let the next build of the same embedding space reuse this index's rows: the
	// engine sets them before publishing the index (a stamp changes whenever a leaf's vectors can).
	Space      string
	Stamps     map[string]uint64
	summaryPos map[string]int
	// RoomSigs lets the next build reuse a room's messages from this index while the room is unchanged
	RoomSigs      map[string]string
	RoomRevisions map[string]int64
	Recipe        string // only this segmentation recipe belongs to the index

	// MsgVecs holds one row per embedded message (in no particular order). The pairs (msgRow[k], msgLeaf[k])
	// say which row belongs to which leaf position in Leaves, in the canonical order of ProjectMessages: the
	// leaf order, then the message order inside the leaf. A message that sits in several leaves (overlapping
	// recipes) appears once per leaf. Empty unless memory.message_vectors is on.
	MsgVecs      Matrix
	msgRow       []int32
	msgLeaf      []int32
	MessageSpace string
	MessageRows  map[string]MessageVectorRow
}

// SetMessageVectors attaches message vectors: m holds the rows and the pair k links row rowOf[k] of m to
// the leaf at position leafOf[k] in Leaves (the canonical order, see ProjectMessages). Call it right
// after Build and before the index is shared; Build's signature stays as it was so an index without
// message vectors is built exactly as before.
func (ix *Index) SetMessageVectors(m Matrix, rowOf, leafOf []int32) error {
	if len(rowOf) != len(leafOf) {
		return errors.New("search: message vector rows and their leaves differ in length")
	}
	for k, l := range leafOf {
		if l < 0 || int(l) >= len(ix.Leaves) {
			return errors.New("search: message vector points outside the leaves")
		}
		if rowOf[k] < 0 || int(rowOf[k]) >= m.Rows() {
			return errors.New("search: message vector pair points outside the rows")
		}
	}
	ix.MsgVecs, ix.msgRow, ix.msgLeaf = m, rowOf, leafOf
	return nil
}

// HasMessageVectors reports whether message vectors are loaded.
func (ix *Index) HasMessageVectors() bool { return ix.MsgVecs.Rows() > 0 && len(ix.msgLeaf) > 0 }

// Build indexes messages/leaves/vectors. leafVecs rows align with leaves; summaries (optional) align
// with summaryVecs rows. Leaves must reference positions in msgs.
func Build(cfg Config, msgs []corpus.Message, lv []leaves.Leaf, leafVecs Matrix, summaries []Summary, summaryVecs Matrix) (*Index, error) {
	if leafVecs.Rows() != len(lv) {
		return nil, errors.New("search: leaf vectors and leaves differ in length")
	}
	if len(summaries) > 0 && summaryVecs.Rows() != len(summaries) {
		return nil, errors.New("search: summary vectors and summaries differ in length")
	}
	ix := &Index{Cfg: cfg, Messages: msgs, Leaves: lv, LeafVecs: leafVecs, SummaryVecs: summaryVecs,
		leafPos: make(map[string]int, len(lv)), summaryText: map[string]string{}, leafEndTS: map[string]float64{},
		rules: newTextRules(cfg.Language)}
	docs := make([]lexical.Doc, len(lv))
	for i, l := range lv {
		ix.leafIDs = append(ix.leafIDs, l.DocID)
		ix.leafPos[l.DocID] = i
		docs[i] = lexical.Doc{ID: l.DocID, Text: l.Body}
	}
	var err error
	if ix.leafLex, err = lexical.NewIndexWith(ix.rules.tok); err != nil {
		return nil, err
	}
	// each lexical index is an in-memory SQLite database: a failed build must not leave one open
	built := false
	defer func() {
		if !built {
			ix.Close()
		}
	}()
	if err := ix.leafLex.AddMany(docs); err != nil {
		return nil, err
	}
	if len(summaries) > 0 {
		sdocs := make([]lexical.Doc, len(summaries))
		ix.summaryPos = make(map[string]int, len(summaries))
		for i, s := range summaries {
			ix.summaryPos[s.DocID] = i
			ix.summaryIDs = append(ix.summaryIDs, s.DocID)
			ix.summaryText[s.DocID] = s.Text
			sdocs[i] = lexical.Doc{ID: s.DocID, Text: s.Text}
		}
		if ix.summaryLex, err = lexical.NewIndexWith(ix.rules.tok); err != nil {
			return nil, err
		}
		if err := ix.summaryLex.AddMany(sdocs); err != nil {
			return nil, err
		}
	}
	n := len(msgs)
	var withTS []string
	for _, l := range lv {
		end := l.End - 1
		ts, ok := 0.0, false
		if end >= 0 && end < n {
			ts, ok = corpus.Timestamp(msgs[end].ParsedTime)
		}
		if !ok && l.Start >= 0 && l.Start < n {
			ts, ok = corpus.Timestamp(msgs[l.Start].ParsedTime)
		}
		if ok {
			ix.leafEndTS[l.DocID] = ts
			withTS = append(withTS, l.DocID)
			if ts > ix.corpusTMax {
				ix.corpusTMax = ts
			}
		}
	}
	sort.SliceStable(withTS, func(a, b int) bool { return ix.leafEndTS[withTS[a]] > ix.leafEndTS[withTS[b]] })
	ix.byRecency = withTS
	built = true
	return ix, nil
}

// Close releases the in-memory lexical databases.
func (ix *Index) Close() {
	if ix.leafLex != nil {
		_ = ix.leafLex.Close()
	}
	if ix.summaryLex != nil {
		_ = ix.summaryLex.Close()
	}
}

// Closer returns what Close does without holding on to the index: a replaced index's vectors can be
// freed as soon as the searches using it finish, while its lexical databases close a little later.
func (ix *Index) Closer() func() {
	leaf, summary := ix.leafLex, ix.summaryLex
	return func() {
		if leaf != nil {
			_ = leaf.Close()
		}
		if summary != nil {
			_ = summary.Close()
		}
	}
}

// RoomMessages returns the index's messages of each room (Messages is ordered room by room).
func (ix *Index) RoomMessages() map[string][]corpus.Message {
	out := map[string][]corpus.Message{}
	for lo := 0; lo < len(ix.Messages); {
		hi := lo + 1
		for hi < len(ix.Messages) && ix.Messages[hi].Room == ix.Messages[lo].Room {
			hi++
		}
		out[ix.Messages[lo].Room] = ix.Messages[lo:hi:hi]
		lo = hi
	}
	return out
}

// QuantizedLeaf returns a leaf's int8 vector and scale, when the index holds quantized vectors.
func (ix *Index) QuantizedLeaf(id string) ([]int8, float32, bool) {
	i, ok := ix.leafPos[id]
	if !ok || ix.LeafVecs.Q == nil {
		return nil, 0, false
	}
	q, sc := ix.LeafVecs.RowQ(i)
	return q, sc, true
}

// QuantizedSummary returns a leaf summary's int8 vector and scale, when the index has it.
func (ix *Index) QuantizedSummary(id string) ([]int8, float32, bool) {
	i, ok := ix.summaryPos[id]
	if !ok || ix.SummaryVecs.Q == nil {
		return nil, 0, false
	}
	q, sc := ix.SummaryVecs.RowQ(i)
	return q, sc, true
}

// Leaf returns a leaf by ID.
func (ix *Index) Leaf(id string) (leaves.Leaf, bool) {
	i, ok := ix.leafPos[id]
	if !ok {
		return leaves.Leaf{}, false
	}
	return ix.Leaves[i], true
}

// SummaryOf returns the derived summary text of a leaf ("" when none).
func (ix *Index) SummaryOf(id string) string { return ix.summaryText[id] }

// HasSummaries reports whether the summary channel is loaded.
func (ix *Index) HasSummaries() bool { return ix.summaryLex != nil && ix.SummaryVecs.Rows() > 0 }

// LeafEndTime returns the leaf's end timestamp (epoch seconds, naive times read as UTC).
func (ix *Index) LeafEndTime(id string) (float64, bool) {
	ts, ok := ix.leafEndTS[id]
	return ts, ok
}

// Rooms lists rooms that have leaves, sorted.
func (ix *Index) Rooms() []string {
	seen := map[string]bool{}
	var out []string
	for _, l := range ix.Leaves {
		if !seen[l.Room] {
			seen[l.Room] = true
			out = append(out, l.Room)
		}
	}
	sort.Strings(out)
	return out
}

// roomLeaves returns the leaf IDs of the given rooms.
func (ix *Index) roomLeaves(rooms ...string) Set {
	want := map[string]bool{}
	for _, r := range rooms {
		want[r] = true
	}
	out := Set{}
	for _, l := range ix.Leaves {
		if want[l.Room] {
			out[l.DocID] = struct{}{}
		}
	}
	return out
}

func (ix *Index) lazy() {
	ix.once.Do(func() {
		if ix.identity == nil {
			ix.identity = buildIdentityTable(ix)
		}
		ix.spkLeaves = map[string][]string{}
		for _, l := range ix.Leaves {
			for _, sp := range l.Speakers {
				if _, ok := ix.spkLeaves[sp]; !ok {
					ix.spkOrder = append(ix.spkOrder, sp)
				}
				ix.spkLeaves[sp] = append(ix.spkLeaves[sp], l.DocID)
			}
		}
		ix.msgByObs = make(map[string]int, len(ix.Messages))
		for i, m := range ix.Messages {
			if _, ok := ix.msgByObs[m.ObservationID]; !ok && m.ObservationID != "" {
				ix.msgByObs[m.ObservationID] = i
			}
		}
		ix.lazyDone.Store(true)
	})
}

func (ix *Index) leafLexicalSearch(q string, limit int) []lexical.Hit {
	return ix.leafLex.Search(q, limit)
}
