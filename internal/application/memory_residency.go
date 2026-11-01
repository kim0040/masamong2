package application

import (
	"context"
	"fmt"
	"sync"
	"time"

	"masamong/internal/action"
	"masamong/internal/memory/engine"
)

// residencyReadTimeout bounds the diagnostics' wait for the full-text databases: they share one
// connection with searches and rebuilds, and a diagnostics call must not hold up a doctor reply.
const residencyReadTimeout = 2 * time.Second

const residencyMiB = 1 << 20

// memoryResidency is the numbers of the memory_residency doctor row (ADR-18 stage 0). It is measurement
// only: nothing is unloaded, the resident set is always every room of the loaded index. Only counts and
// sizes: no room names, texts or accounts.
type memoryResidency struct {
	Loaded        bool
	ResidentSet   string // search.ResidentAll in stage zero
	ResidentRooms int    // rooms in the loaded index
	IndexedRooms  int    // rooms with at least one embedded leaf in the database (prepared, not necessarily loaded)
	TotalRooms    int    // rooms with stored messages
	EstimatedByte int64  // Go heap the index structures are estimated to hold; an estimate, not a measurement
	FTSBytes      int64  // database pages of both full-text databases; outside the Go heap
	FTSSource     string
	FTSKnown      bool

	SampleKnown bool
	SampleMax   uint64 // largest HeapAlloc of the last rebuild's stage-boundary samples; not the true peak
	SampleBasis string
	Outcome     string // the last rebuild's outcome (engine.ReloadPublished, ...)
	LastStage   string
}

// residencyCache keeps the last measurement for memDoctorFresh while the same index stays loaded: the
// numbers only move when the index reloads, and reading them walks every room and both full-text
// databases.
type residencyCache struct {
	mu  sync.Mutex
	key int64
	at  time.Time
	val memoryResidency
}

// readMemoryResidency gathers the row's numbers. A failed read leaves its field unknown and never
// fails the doctor or touches a search.
func (s *Service) readMemoryResidency(ctx context.Context) memoryResidency {
	eng, err := s.MemoryEngine()
	if err != nil {
		return memoryResidency{}
	}
	key := eng.LoadedAt().UnixNano()
	c := &s.residency
	c.mu.Lock()
	if c.key == key && time.Since(c.at) < memDoctorFresh {
		out := c.val
		c.mu.Unlock()
		return out
	}
	c.mu.Unlock()
	out := s.measureMemoryResidency(ctx, eng)
	if out.FTSKnown || !out.Loaded { // a timed-out full-text read is measured again next time
		c.mu.Lock()
		c.key, c.at, c.val = key, time.Now(), out
		c.mu.Unlock()
	}
	return out
}

func (s *Service) measureMemoryResidency(ctx context.Context, eng *engine.Engine) memoryResidency {
	out := memoryResidency{}
	if rooms, err := eng.Store.Rooms(); err == nil {
		for _, r := range rooms {
			if r.Messages > 0 {
				out.TotalRooms++
			}
			if r.Embedded > 0 {
				out.IndexedRooms++
			}
		}
	}
	if m, ok := eng.ResidencyMeasurement(); ok {
		out.SampleKnown, out.SampleMax, out.SampleBasis = len(m.Samples) > 0, m.MaxHeapAlloc(), engine.MeasurementBasis
		out.Outcome, out.LastStage = m.Outcome, m.LastStage()
	}
	ix := eng.Index()
	if ix == nil {
		return out
	}
	fp := ix.EstimatedBytes()
	out.Loaded, out.ResidentSet, out.ResidentRooms, out.EstimatedByte = true, fp.Resident, fp.Rooms, fp.Total
	rctx, cancel := context.WithTimeout(ctx, residencyReadTimeout)
	defer cancel()
	if fts, err := ix.FTSStorageBytes(rctx); err == nil {
		out.FTSBytes, out.FTSSource, out.FTSKnown = fts.Total, fts.Source, true
	}
	return out
}

// mibText is a size in MiB with one decimal: a small instance's rooms are a few MiB, which a whole number hides.
func mibText(bytes float64) string { return fmt.Sprintf("%.1f", bytes/residencyMiB) }

// putMemoryResidency adds the memory_residency feature. "Prepared" rooms are the ones with embedded
// leaves in the database; "resident" rooms are the ones in the index that search reads right now, which
// can lag behind until the next rebuild. In stage zero every room of the index is resident.
func (s *Service) putMemoryResidency(ctx context.Context, req action.Request, features map[string]any, put func(name, status, reason string)) {
	r := s.readMemoryResidency(ctx)
	if !r.Loaded {
		put("memory_residency", "pending", "memory_residency_none")
		item := features["memory_residency"].(map[string]any)
		item["resident_rooms"], item["indexed_rooms"], item["total_rooms"] = 0, r.IndexedRooms, r.TotalRooms
		return
	}
	put("memory_residency", "ready", "memory_residency")
	item := features["memory_residency"].(map[string]any)
	item["resident_set"] = r.ResidentSet
	item["resident_rooms"], item["indexed_rooms"], item["total_rooms"] = r.ResidentRooms, r.IndexedRooms, r.TotalRooms
	item["estimated_bytes"], item["estimate_kind"] = r.EstimatedByte, "go_heap_structures"
	if r.FTSKnown {
		item["fts_page_bytes"], item["fts_source"] = r.FTSBytes, r.FTSSource
	}
	fts, peak := "–", "–"
	if r.FTSKnown {
		fts = mibText(float64(r.FTSBytes))
	}
	if r.SampleKnown {
		peak = mibText(float64(r.SampleMax))
		item["sample_max_heap_bytes"], item["sample_basis"] = r.SampleMax, r.SampleBasis
	}
	if r.Outcome != "" {
		item["last_rebuild"], item["last_rebuild_stage"] = r.Outcome, r.LastStage
	}
	text := s.msg(req, "features.reason.memory_residency",
		"resident", r.ResidentRooms, "total", r.TotalRooms, "indexed", r.IndexedRooms,
		"estimate", mibText(float64(r.EstimatedByte)), "fts", fts, "peak", peak)
	if r.Outcome != "" && r.Outcome != engine.ReloadPublished {
		stage := r.LastStage
		if stage == "" {
			stage = "–"
		}
		text += " " + s.msg(req, "features.reason.memory_residency_unfinished", "stage", stage)
	}
	item["reason_text"] = text
}
