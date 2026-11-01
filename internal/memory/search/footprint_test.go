package search

import (
	"context"
	"fmt"
	"math"
	"runtime"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
	"masamong/internal/memory/leaves"
)

// footprintFixture is a synthetic index: rooms of perRoom messages, ten per leaf, a summary for every
// other leaf and a message vector for about every other message. Every string is its own allocation, as
// rows scanned from the database are.
func footprintFixture(t testing.TB, rooms, perRoom, dims int) *Index {
	t.Helper()
	var msgs []corpus.Message
	var lv []leaves.Leaf
	var sums []Summary
	leafM, sumM, msgM := NewSharedQuantized(dims, 0), NewSharedQuantized(dims, 0), NewSharedQuantized(dims, 0)
	vec := make([]float32, dims)
	row := func(m *Matrix, seed int) {
		for j := range vec {
			vec[j] = float32((seed+j*7)%29-14) / 127
		}
		m.AppendQuantized(vec)
	}
	var rowOf, leafOf []int32
	base := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for r := 0; r < rooms; r++ {
		room := fmt.Sprintf("discord:synthetic:%d", r)
		start := len(msgs)
		for i := 0; i < perRoom; i++ {
			msgs = append(msgs, corpus.Message{
				ObservationID: fmt.Sprintf("obs-%d-%d-%s", r, i, "0123456789abcdef0123456789abcdef"),
				Room:          fmt.Sprintf("%s", room), RoomOriginal: fmt.Sprintf("%s", room),
				ActorLabel: fmt.Sprintf("speaker-%d", i%5), ActorAccount: fmt.Sprintf("account-%d", i%5),
				Content:    fmt.Sprintf("synthetic message number %d of room %d, a line of ordinary length", i, r),
				ParsedTime: base.Add(time.Duration(i) * time.Minute).Format(time.RFC3339),
			})
		}
		for lo := 0; lo < perRoom; lo += 10 {
			hi := min(perRoom, lo+10)
			l := leaves.Leaf{DocID: fmt.Sprintf("leaf-%d-%d", r, lo), Kind: "leaf", Start: start + lo, End: start + hi, Room: room, N: hi - lo,
				Body: fmt.Sprintf("body of leaf %d in room %d: %s", lo, r, msgs[start+lo].Content)}
			for i := lo; i < hi; i++ {
				l.ObservationIDs = append(l.ObservationIDs, msgs[start+i].ObservationID)
			}
			l.Speakers = []string{fmt.Sprintf("speaker-%d", lo%5)}
			pos := len(lv)
			lv = append(lv, l)
			row(&leafM, pos)
			if pos%2 == 0 {
				sums = append(sums, Summary{DocID: l.DocID, Text: fmt.Sprintf("summary of leaf %d", pos)})
				row(&sumM, pos+3)
			}
			for i := lo; i < hi; i++ {
				if (i+r)%2 == 0 {
					rowOf = append(rowOf, int32(msgM.Rows()))
					leafOf = append(leafOf, int32(pos))
					row(&msgM, i)
				}
			}
		}
	}
	cfg := Default()
	cfg.SummaryEnabled = true
	ix, err := Build(cfg, msgs, lv, leafM, sums, sumM)
	if err != nil {
		t.Fatal(err)
	}
	if err := ix.SetMessageVectors(msgM, rowOf, leafOf); err != nil {
		t.Fatal(err)
	}
	ix.MessageRows = map[string]MessageVectorRow{}
	for _, m := range msgs[:min(len(msgs), msgM.Rows())] {
		ix.MessageRows[m.ObservationID] = MessageVectorRow{TextHash: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Label: "label", UpdatedAt: "2025-01-01T00:00:00Z", Format: 1, Bytes: dims}
	}
	t.Cleanup(ix.Close)
	return ix
}

func TestEstimatedBytesByRoomAddsUp(t *testing.T) {
	ix := footprintFixture(t, 3, 400, 64)
	f := ix.EstimatedBytes()
	if f.Resident != ResidentAll || f.Rooms != 3 || f.Messages != 1200 || f.Leaves != 120 || f.MessageVectors != ix.MsgVecs.Rows() {
		t.Fatalf("counts: %+v", f)
	}
	var sum int64
	for room, r := range f.PerRoom {
		if r.Messages != 400 || r.Leaves != 40 || r.Bytes <= 0 || r.Parts.Identity != 0 {
			t.Fatalf("room %s: %+v", room, r)
		}
		sum += r.Bytes
	}
	if sum+f.Parts.Identity != f.Total || f.Total != f.Parts.Sum() {
		t.Fatalf("total %d != rooms %d + identity %d", f.Total, sum, f.Parts.Identity)
	}
	if f.Parts.Identity != 0 {
		t.Fatalf("identity counted before a search built it: %d", f.Parts.Identity)
	}
	ix.lazy()
	if g := ix.EstimatedBytes(); g.Parts.Identity <= 0 || g.Total <= f.Total {
		t.Fatalf("lazy tables not counted once built: %d vs %d", g.Parts.Identity, f.Total)
	}
	if (*Index)(nil).EstimatedBytes().Total != 0 {
		t.Fatal("nil index")
	}
}

// The doctor can estimate while the first search builds the lazy tables: the estimate reads them only
// after lazyDone, so `go test -race` stays quiet.
func TestEstimatedBytesDuringFirstSearch(t *testing.T) {
	for i := 0; i < 20; i++ {
		ix := footprintFixture(t, 2, 200, 16)
		done := make(chan struct{})
		go func() { ix.lazy(); close(done) }()
		for {
			_ = ix.EstimatedBytes()
			select {
			case <-done:
			default:
				continue
			}
			break
		}
		if ix.EstimatedBytes().Parts.Identity <= 0 {
			t.Fatal("lazy tables not counted after the first search")
		}
	}
}

// Rows a rebuilt index shares with the previous one are one allocation: counting both indexes with one
// counter must not count the data twice.
func TestEstimatedBytesCountsSharedRowsOnce(t *testing.T) {
	a := footprintFixture(t, 1, 200, 64)
	b := footprintFixture(t, 1, 200, 64)
	next := NewSharedQuantized(64, a.LeafVecs.Rows())
	for i := 0; i < a.LeafVecs.Rows(); i++ {
		q, s := a.LeafVecs.RowQ(i)
		next.ShareRowQ(q, s)
	}
	b.LeafVecs = next
	rc := NewRowCounter()
	first := a.EstimatedBytesWith(rc)
	second := b.EstimatedBytesWith(rc)
	alone := b.EstimatedBytes()
	rows := int64(a.LeafVecs.Rows())
	if want := alone.Parts.LeafVectors - rows*64; second.Parts.LeafVectors != want {
		t.Fatalf("shared rows counted twice: %d, want %d (alone %d)", second.Parts.LeafVectors, want, alone.Parts.LeafVectors)
	}
	if first.Parts.LeafVectors != alone.Parts.LeafVectors {
		t.Fatalf("first index changed: %d vs %d", first.Parts.LeafVectors, alone.Parts.LeafVectors)
	}
}

// The estimate is a guess about the Go heap: check it is the same unit as HeapAlloc by building a 10,000
// message index and comparing it with what the runtime reports after a collection. The ratio is logged
// for the work report; the bounds only catch a unit mistake.
func TestEstimatedBytesIsTheSizeOfTheHeap(t *testing.T) {
	if testing.Short() {
		t.Skip("allocates a synthetic index")
	}
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	ix := footprintFixture(t, 2, 5000, 512)
	runtime.GC()
	runtime.ReadMemStats(&after)
	grew := int64(after.HeapAlloc) - int64(before.HeapAlloc)
	est := ix.EstimatedBytes()
	ratio := float64(est.Total) / float64(grew)
	t.Logf("messages=%d leaves=%d msgvecs=%d: estimate %.2f MiB, HeapAlloc growth %.2f MiB, estimate/heap %.2f", est.Messages, est.Leaves, est.MessageVectors,
		mib(est.Total), mib(grew), ratio)
	t.Logf("parts MiB: leafvec %.2f sumvec %.2f msgvec %.2f messages %.2f msgindex %.2f leaves %.2f summaries %.2f",
		mib(est.Parts.LeafVectors), mib(est.Parts.SummaryVectors), mib(est.Parts.MessageVectors), mib(est.Parts.Messages), mib(est.Parts.MessageIndex), mib(est.Parts.Leaves), mib(est.Parts.Summaries))
	if math.IsNaN(ratio) || ratio < 0.6 || ratio > 1.6 {
		t.Fatalf("estimate is not the heap's size: ratio %.2f", ratio)
	}
	runtime.KeepAlive(ix)
}

func mib(n int64) float64 { return float64(n) / (1 << 20) }

func TestFTSStorageBytesReadsPagesAndSurvivesClose(t *testing.T) {
	ix := footprintFixture(t, 1, 200, 16)
	s, err := ix.FTSStorageBytes(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if s.Leaf <= 0 || s.Summary <= 0 || s.Total != s.Leaf+s.Summary || s.Source != "sqlite_page_count_x_page_size" {
		t.Fatalf("storage: %+v", s)
	}
	// reading next to searches and a close must neither panic nor disturb them
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 200; i++ {
			_ = ix.leafLexicalSearch("synthetic message", 5)
		}
	}()
	for i := 0; i < 50; i++ {
		_, _ = ix.FTSStorageBytes(context.Background())
	}
	<-done
	ix.Close()
	if _, err := ix.FTSStorageBytes(context.Background()); err == nil {
		t.Fatal("a closed database was read without an error")
	}
	if _, err := (&Index{}).FTSStorageBytes(context.Background()); err != nil {
		t.Fatalf("an index without databases is not an error: %v", err)
	}
}
