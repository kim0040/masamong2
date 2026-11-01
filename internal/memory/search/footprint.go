package search

import (
	"context"
	"sort"
	"unsafe"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
)

// ResidentAll names the resident set of stage zero: every room an index holds is resident. A smaller
// set belongs to the unloading stages and is not product behavior yet.
const ResidentAll = "all"

// Sizes of the Go values the estimate counts. They are the 64-bit layout; a smaller word makes the
// real heap smaller, so the estimate stays an upper-leaning guess there.
const (
	strHeader   = int64(unsafe.Sizeof(""))
	sliceHeader = int64(unsafe.Sizeof([]byte(nil)))
	messageSize = int64(unsafe.Sizeof(corpus.Message{}))
	leafSize    = int64(unsafe.Sizeof(leaves.Leaf{}))
	msgRowSize  = int64(unsafe.Sizeof(MessageVectorRow{}))
	scaleSize   = int64(unsafe.Sizeof(float32(0)))
)

// mapEntry is what one map entry costs on top of the data its key and value point to. A Go map keeps its
// entries in groups with spare slots and grows by doubling, so about half again as much as the entry
// itself is a fair average; this is a guess to be checked against HeapAlloc, not a measured constant.
func mapEntry(keyBytes, valueBytes int64) int64 { return (keyBytes+valueBytes)*3/2 + 1 }

// FootprintParts splits an estimate by what holds the memory.
type FootprintParts struct {
	LeafVectors    int64 // int8 rows of the leaf matrix, with their scales and slice headers
	SummaryVectors int64
	MessageVectors int64
	Messages       int64 // the message array: structs and the strings they point to
	MessageIndex   int64 // message-vector pairs, the per-row map and the observation-ID position map
	Leaves         int64 // leaf structs and bodies, ID lists, position and time maps, recency order
	Summaries      int64 // summary texts and their ID lists and position maps
	Identity       int64 // identity and speaker tables, only once a search has built them
}

// Sum is the total of all parts.
func (p FootprintParts) Sum() int64 {
	return p.LeafVectors + p.SummaryVectors + p.MessageVectors + p.Messages + p.MessageIndex + p.Leaves + p.Summaries + p.Identity
}

func (p *FootprintParts) add(o FootprintParts) {
	p.LeafVectors += o.LeafVectors
	p.SummaryVectors += o.SummaryVectors
	p.MessageVectors += o.MessageVectors
	p.Messages += o.Messages
	p.MessageIndex += o.MessageIndex
	p.Leaves += o.Leaves
	p.Summaries += o.Summaries
	p.Identity += o.Identity
}

// RoomFootprint is the estimate of one room's share of an index. Identity is not split by room (one
// table serves every room), so a room's Parts.Identity is always 0.
type RoomFootprint struct {
	Messages       int
	Leaves         int
	MessageVectors int
	Parts          FootprintParts
	Bytes          int64
}

// Footprint is an estimate of what an Index holds in the Go heap. It counts the sizes of the structures
// the index owns, not what the runtime reports: allocator rounding, garbage not yet collected and
// slice capacity beyond length are outside it. The lexical (FTS5) databases are not in it either: SQLite
// allocates outside the Go heap (see FTSStorage).
type Footprint struct {
	// Resident is the resident set the index holds: ResidentAll in stage zero.
	Resident string
	Rooms    int
	Messages int
	Leaves   int
	// MessageVectors is the number of message-vector rows.
	MessageVectors int
	Parts          FootprintParts
	Total          int64
	// PerRoom is keyed by room. Its Bytes plus Parts.Identity add up to Total.
	PerRoom map[string]RoomFootprint
}

// RowCounter remembers which shared int8 rows an estimate already counted. One counter passed to every
// index of the process (the current one and the older ones still held by searches) counts a row that
// two indexes share once, as the process holds it once.
type RowCounter struct{ seen map[*int8]struct{} }

// NewRowCounter returns an empty counter.
func NewRowCounter() *RowCounter { return &RowCounter{seen: map[*int8]struct{}{}} }

// row is the bytes of row i of m that the process did not count yet. Rows of a shared matrix are
// separate allocations (their data counts once); the matrix itself keeps a header and a scale per row.
func (c *RowCounter) row(m Matrix, i int) int64 {
	switch {
	case m.qRows != nil:
		q := m.qRows[i]
		n := sliceHeader + scaleSize
		if len(q) == 0 {
			return n
		}
		p := unsafe.SliceData(q)
		if c != nil {
			if _, dup := c.seen[p]; dup {
				return n
			}
			c.seen[p] = struct{}{}
		}
		return n + int64(len(q))
	case m.Q != nil:
		return int64(m.Dims) + scaleSize
	}
	return 4 * int64(m.Dims)
}

func stringBytes(s string) int64 { return int64(len(s)) }

// EstimatedBytes estimates the index's Go heap, in total and by room, in stage zero for the whole
// resident set. Shared int8 rows count once within the index; use EstimatedBytesWith to count them once
// across several indexes. It reads only the immutable parts and the lazy tables a search already built
// (it never builds them), so it is safe next to searches and cheap enough for a diagnostics call.
func (ix *Index) EstimatedBytes() Footprint { return ix.EstimatedBytesWith(NewRowCounter()) }

// EstimatedBytesWith is EstimatedBytes with a caller-owned counter, see RowCounter.
func (ix *Index) EstimatedBytesWith(rc *RowCounter) Footprint {
	if ix == nil {
		return Footprint{Resident: ResidentAll}
	}
	f := Footprint{Resident: ResidentAll, Messages: len(ix.Messages), Leaves: len(ix.Leaves), PerRoom: map[string]RoomFootprint{}}
	room := func(name string) RoomFootprint { return f.PerRoom[name] }
	put := func(name string, r RoomFootprint) { f.PerRoom[name] = r }

	for i := 0; i < len(ix.Messages); {
		name := ix.Messages[i].Room
		r := room(name)
		for ; i < len(ix.Messages) && ix.Messages[i].Room == name; i++ {
			m := ix.Messages[i]
			r.Messages++
			r.Parts.Messages += messageSize + stringBytes(m.ObservationID) + stringBytes(m.Room) + stringBytes(m.RoomOriginal) +
				stringBytes(m.ActorLabel) + stringBytes(m.ActorAccount) + stringBytes(m.Content) + stringBytes(m.ParsedTime)
		}
		put(name, r)
	}

	// leaf i is row i of the leaf matrix; the ID list shares the strings the leaf holds
	for i, l := range ix.Leaves {
		r := room(l.Room)
		r.Leaves++
		b := leafSize + stringBytes(l.DocID) + stringBytes(l.Kind) + stringBytes(l.Body) + stringBytes(l.Room) +
			int64(len(l.ObservationIDs))*strHeader + int64(len(l.Speakers))*strHeader
		for _, id := range l.ObservationIDs {
			b += stringBytes(id)
		}
		for _, sp := range l.Speakers {
			b += stringBytes(sp)
		}
		b += 2*strHeader + // leafIDs and the lexical index's own ID list
			mapEntry(strHeader, 8) // leafPos
		if _, ok := ix.leafEndTS[l.DocID]; ok {
			b += mapEntry(strHeader, 8) + strHeader // leafEndTS and byRecency
		}
		r.Parts.Leaves += b
		if i < ix.LeafVecs.Rows() {
			r.Parts.LeafVectors += rc.row(ix.LeafVecs, i)
		}
		put(l.Room, r)
	}

	// summaries belong to the room of the leaf they describe
	for i, id := range ix.summaryIDs {
		li, ok := ix.leafPos[id]
		if !ok {
			continue
		}
		name := ix.Leaves[li].Room
		r := room(name)
		r.Parts.Summaries += 2*strHeader + mapEntry(strHeader, 8) + mapEntry(strHeader, strHeader) + stringBytes(ix.summaryText[id])
		if i < ix.SummaryVecs.Rows() {
			r.Parts.SummaryVectors += rc.row(ix.SummaryVecs, i)
		}
		put(name, r)
	}

	// A message vector row is counted once, in the room of the first leaf that points at it. The
	// observation-keyed row map is split in proportion to those rows: its keys are not tied to a room.
	rows := ix.MsgVecs.Rows()
	f.MessageVectors = rows
	if rows > 0 {
		seen := make([]bool, rows)
		perRoomRows := map[string]int{}
		for k := range ix.msgLeaf {
			row, leaf := int(ix.msgRow[k]), int(ix.msgLeaf[k])
			name := ix.Leaves[leaf].Room
			r := room(name)
			r.Parts.MessageIndex += 8 // the msgRow and msgLeaf pair
			if !seen[row] {
				seen[row] = true
				perRoomRows[name]++
				r.MessageVectors++
				r.Parts.MessageVectors += rc.row(ix.MsgVecs, row)
			}
			put(name, r)
		}
		var rowMap int64
		for key, v := range ix.MessageRows {
			rowMap += mapEntry(strHeader+stringBytes(key), msgRowSize) + stringBytes(v.TextHash) + stringBytes(v.Label) + stringBytes(v.UpdatedAt)
		}
		names := make([]string, 0, len(perRoomRows))
		for name := range perRoomRows {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			r := room(name)
			r.Parts.MessageIndex += rowMap * int64(perRoomRows[name]) / int64(rows)
			put(name, r)
		}
	}
	// lazyDone first: the atomic load orders the read of msgByObs after lazy() finished writing it
	if ix.lazyDone.Load() && len(ix.msgByObs) > 0 {
		// built on first use; the observation IDs themselves are the messages' own strings
		perMsg := mapEntry(strHeader, 8)
		for name, r := range f.PerRoom {
			r.Parts.MessageIndex += perMsg * int64(r.Messages)
			f.PerRoom[name] = r
		}
	}

	for name, r := range f.PerRoom {
		r.Bytes = r.Parts.Sum()
		f.PerRoom[name] = r
		f.Parts.add(r.Parts)
	}
	f.Rooms = len(f.PerRoom)
	if ix.lazyDone.Load() {
		f.Parts.Identity = ix.identityBytes()
	}
	f.Total = f.Parts.Sum()
	return f
}

// identityBytes is the size of the identity and speaker tables a search built. The caller checks lazyDone.
func (ix *Index) identityBytes() int64 {
	var b int64
	if t := ix.identity; t != nil {
		for k, labels := range t.labelsByKey {
			b += mapEntry(3*strHeader+stringBytes(k.Kind)+stringBytes(k.Platform)+stringBytes(k.Value), 8)
			for label := range labels {
				b += mapEntry(strHeader+stringBytes(label), 1)
			}
		}
		for label, keys := range t.keysByLabel {
			b += mapEntry(strHeader+stringBytes(label), 8)
			b += int64(len(keys)) * mapEntry(3*strHeader, 1)
		}
		for _, set := range t.leavesByKey {
			b += mapEntry(3*strHeader, 8)
			b += int64(len(set)) * mapEntry(strHeader, 0)
		}
	}
	for sp, ids := range ix.spkLeaves {
		b += mapEntry(strHeader+stringBytes(sp), sliceHeader) + int64(len(ids))*strHeader
	}
	b += int64(len(ix.spkOrder)) * strHeader
	return b
}

// FTSStorage is the size of the database pages of the index's two full-text databases. Pages are the
// databases' own data, not SQLite's whole allocation, and not the process's memory (see lexical.StorageStats).
type FTSStorage struct {
	Leaf    int64
	Summary int64
	Total   int64
	// Source says how the number was read (lexical.StorageSource).
	Source string
}

// FTSStorageBytes reads the page bytes of the leaf and summary databases. It returns an error when a
// database was closed in the meantime or ctx ended; searching is not affected either way.
func (ix *Index) FTSStorageBytes(ctx context.Context) (FTSStorage, error) {
	var out FTSStorage
	if ix == nil || ix.leafLex == nil {
		return out, nil
	}
	leaf, err := ix.leafLex.StorageBytes(ctx)
	if err != nil {
		return FTSStorage{}, err
	}
	out.Leaf, out.Source = leaf.Bytes, leaf.Source
	if ix.summaryLex != nil {
		sum, err := ix.summaryLex.StorageBytes(ctx)
		if err != nil {
			return FTSStorage{}, err
		}
		out.Summary = sum.Bytes
	}
	out.Total = out.Leaf + out.Summary
	return out, nil
}
