package main

// ADR-18 stage-zero footprint measurements (C-4). Both commands open a stopped scratch COPY of a data
// root with an independent engine (the way dump-candidates does), never change the product config and
// never call a provider. They print numbers only: no room IDs (rooms are labelled by their size rank),
// no message text, no names.
//
//	measure-reload  one process: cold load, a warm-up search (builds the lazy tables), one unchanged hot
//	                rebuild. Heap, runtime and process working-set readings at each step, polled peaks.
//	measure-rooms   one process: rebuild {rest} then {rest + one room} -reps times, the cost of bringing
//	                one room up (the first-question delay of ADR-18 stage 1).
//
// The emulation of a small box is the environment of the process: GOMAXPROCS=1 and GOMEMLIMIT.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"runtime/pprof"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"masamong/internal/memory/engine"
	"masamong/internal/memory/search"
	"masamong/internal/memory/store"
)

type snapshot struct {
	HeapAlloc    uint64 `json:"heap_alloc"`
	HeapInuse    uint64 `json:"heap_inuse"`
	HeapSys      uint64 `json:"heap_sys"`
	HeapIdle     uint64 `json:"heap_idle"`
	HeapReleased uint64 `json:"heap_released"`
	StackInuse   uint64 `json:"stack_inuse"`
	Sys          uint64 `json:"sys"`
	TotalAlloc   uint64 `json:"total_alloc"`
	NumGC        uint32 `json:"num_gc"`
	PauseTotalNs uint64 `json:"pause_total_ns"`
	WorkingSet   uint64 `json:"working_set"`
	PeakWS       uint64 `json:"peak_working_set"`
	Private      uint64 `json:"private_bytes"`
}

func takeSnapshot() snapshot {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	pm := readProcMemory()
	return snapshot{ms.HeapAlloc, ms.HeapInuse, ms.HeapSys, ms.HeapIdle, ms.HeapReleased, ms.StackInuse, ms.Sys, ms.TotalAlloc,
		ms.NumGC, ms.PauseTotalNs, pm.WorkingSet, pm.PeakWorkingSet, pm.Private}
}

// settled collects garbage twice (a finalizer or a deferred close can free more on the second pass) and
// reads the counters: the live heap the process holds at rest.
func settled() snapshot {
	runtime.GC()
	runtime.GC()
	return takeSnapshot()
}

// poller samples the heap and the working set while a step runs. Go's own counters come from
// runtime/metrics (no stop-the-world); the working set from the operating system. It sees what its
// interval lets it see, so the maxima are lower bounds of the true peak.
type poller struct {
	stop, done chan struct{}
	mu         sync.Mutex
	objects    uint64 // /memory/classes/heap/objects:bytes
	total      uint64 // /memory/classes/total:bytes (all memory the Go runtime maps)
	ws         uint64
	samples    int
}

func startPoller(every time.Duration) *poller {
	p := &poller{stop: make(chan struct{}), done: make(chan struct{})}
	read := func() {
		s := []metrics.Sample{{Name: "/memory/classes/heap/objects:bytes"}, {Name: "/memory/classes/total:bytes"}}
		metrics.Read(s)
		ws := readProcMemory().WorkingSet
		p.mu.Lock()
		if s[0].Value.Kind() == metrics.KindUint64 {
			p.objects = max(p.objects, s[0].Value.Uint64())
		}
		if s[1].Value.Kind() == metrics.KindUint64 {
			p.total = max(p.total, s[1].Value.Uint64())
		}
		p.ws = max(p.ws, ws)
		p.samples++
		p.mu.Unlock()
	}
	read()
	go func() {
		defer close(p.done)
		t := time.NewTicker(every)
		defer t.Stop()
		for {
			select {
			case <-p.stop:
				read()
				return
			case <-t.C:
				read()
			}
		}
	}()
	return p
}

func (p *poller) finish() (objects, total, ws uint64, samples int) {
	close(p.stop)
	<-p.done
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.objects, p.total, p.ws, p.samples
}

type stagePoint struct {
	Stage     string  `json:"stage"`
	Seconds   float64 `json:"seconds"`
	HeapAlloc uint64  `json:"heap_alloc"`
	HeapInuse uint64  `json:"heap_inuse"`
	NumGC     uint32  `json:"num_gc"`
}

type phaseResult struct {
	Name          string         `json:"name"`
	Seconds       float64        `json:"seconds"`
	Outcome       string         `json:"outcome,omitempty"`
	Before        snapshot       `json:"before"`
	AfterNoGC     snapshot       `json:"after_before_gc"`
	PeakObjects   uint64         `json:"poll_peak_heap_objects"`
	PeakGoTotal   uint64         `json:"poll_peak_go_total"`
	PeakWS        uint64         `json:"poll_peak_working_set"`
	PollSamples   int            `json:"poll_samples"`
	GCCycles      uint32         `json:"gc_cycles"`
	GCPauseMs     float64        `json:"gc_pause_total_ms"`
	TotalAllocMiB float64        `json:"allocated_mib"`
	Stages        []stagePoint   `json:"stage_samples,omitempty"`
	Counts        map[string]int `json:"counts,omitempty"`
}

// runPhase runs f with the poller on and the engine's stage-boundary samples read afterwards.
func runPhase(name string, e *engine.Engine, f func() (map[string]int, error)) (phaseResult, error) {
	r := phaseResult{Name: name, Before: takeSnapshot()}
	p := startPoller(5 * time.Millisecond)
	t0 := time.Now()
	counts, err := f()
	r.Seconds = time.Since(t0).Seconds()
	r.PeakObjects, r.PeakGoTotal, r.PeakWS, r.PollSamples = p.finish()
	r.AfterNoGC = takeSnapshot()
	r.Counts = counts
	r.GCCycles = r.AfterNoGC.NumGC - r.Before.NumGC
	r.GCPauseMs = float64(r.AfterNoGC.PauseTotalNs-r.Before.PauseTotalNs) / 1e6
	r.TotalAllocMiB = float64(r.AfterNoGC.TotalAlloc-r.Before.TotalAlloc) / (1 << 20)
	if m, ok := e.ResidencyMeasurement(); ok {
		r.Outcome = m.Outcome
		for _, s := range m.Samples {
			r.Stages = append(r.Stages, stagePoint{s.Stage, s.Since.Seconds(), s.HeapAlloc, s.HeapInuse, s.NumGC})
		}
	}
	return r, err
}

type footprintOut struct {
	TotalBytes int64            `json:"estimated_total_bytes"`
	Rooms      int              `json:"rooms"`
	Messages   int              `json:"messages"`
	Leaves     int              `json:"leaves"`
	MsgVectors int              `json:"message_vector_rows"`
	Parts      map[string]int64 `json:"parts_bytes"`
	FTSLeaf    int64            `json:"fts_leaf_page_bytes"`
	FTSSummary int64            `json:"fts_summary_page_bytes"`
	FTSTotal   int64            `json:"fts_total_page_bytes"`
	FTSSource  string           `json:"fts_source"`
	FTSError   string           `json:"fts_error,omitempty"`
}

func footprintOf(ix *search.Index) footprintOut {
	fp := ix.EstimatedBytes()
	o := footprintOut{TotalBytes: fp.Total, Rooms: fp.Rooms, Messages: fp.Messages, Leaves: fp.Leaves, MsgVectors: fp.MessageVectors,
		Parts: map[string]int64{"leaf_vectors": fp.Parts.LeafVectors, "summary_vectors": fp.Parts.SummaryVectors, "message_vectors": fp.Parts.MessageVectors,
			"messages": fp.Parts.Messages, "message_index": fp.Parts.MessageIndex, "leaves": fp.Parts.Leaves, "summaries": fp.Parts.Summaries, "identity": fp.Parts.Identity}}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	if st, err := ix.FTSStorageBytes(ctx); err != nil {
		o.FTSError = err.Error()
	} else {
		o.FTSLeaf, o.FTSSummary, o.FTSTotal, o.FTSSource = st.Leaf, st.Summary, st.Total, st.Source
	}
	return o
}

type stubEmbedder struct{ v []float32 }

func (s stubEmbedder) EmbedQuery(context.Context, string) ([]float32, error) { return s.v, nil }

// warmSearch runs one search over every room so the index builds the tables it builds lazily on the first
// search (identity, speaker lists, observation positions). The query vector is a fixed unit-ish vector:
// the point is the tables, not the hits.
func warmSearch(ix *search.Index) error {
	dims := ix.LeafVecs.Dims
	if dims == 0 {
		dims = 16
	}
	v := make([]float32, dims)
	for i := range v {
		v[i] = float32((i*7)%13+1) / 13
	}
	_, err := ix.Retrieve(context.Background(), search.Request{Query: "measurement warm up", QueryVector: v, Scope: search.Scope{All: true},
		Ranking: "hybrid", TopK: 5, RemainingUSD: 0}, stubEmbedder{v}, disabledRerank{})
	return err
}

type envOut struct {
	GOMAXPROCS   int    `json:"gomaxprocs"`
	GOMEMLIMIT   string `json:"gomemlimit_env"`
	MemoryLimit  int64  `json:"memory_limit_bytes"`
	GOGC         string `json:"gogc_env"`
	GoVersion    string `json:"go_version"`
	MessageSpace string `json:"message_space"`
	EmbedSpace   string `json:"embed_space"`
	MessageDims  int    `json:"message_dims"`
	DBBytes      int64  `json:"db_file_bytes"`
}

func currentEnv(e *engine.Engine, root string) envOut {
	o := envOut{GOMAXPROCS: runtime.GOMAXPROCS(0), GOMEMLIMIT: os.Getenv("GOMEMLIMIT"), MemoryLimit: debug.SetMemoryLimit(-1),
		GOGC: os.Getenv("GOGC"), GoVersion: runtime.Version(), MessageSpace: e.MessageSpace(), EmbedSpace: e.EmbedSpace(), MessageDims: e.MessageVectorDims()}
	if st, err := os.Stat(filepath.Join(root, "masamong.db")); err == nil {
		o.DBBytes = st.Size()
	}
	return o
}

func writeJSONFile(path string, v any) error {
	if err := scratchPath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	raw, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, raw, 0o600)
}

func mib(b uint64) float64 { return float64(b) / (1 << 20) }

// writeProfile writes the named pprof profile ("allocs" or "heap") as of a collection that has just
// completed (the heap profile reflects the last finished cycle).
func writeProfile(name, path string) error {
	if err := scratchPath(path); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	runtime.GC()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return pprof.Lookup(name).WriteTo(f, 0)
}

// measureReload: cold load, warm-up search, one unchanged hot rebuild.
func measureReload(root, out, profDir string, settleClose bool) error {
	if err := scratchPath(root); err != nil {
		return err
	}
	e, closeFn, _, _, err := independentGateEngine(root, false)
	if err != nil {
		return err
	}
	defer closeFn()
	res := map[string]any{"env": currentEnv(e, root)}
	if profDir != "" {
		runtime.MemProfileRate = 1 // every allocation, so the profiles show each site's real bytes (slow)
	}
	res["baseline_after_open"] = settled()

	cold, err := runPhase("cold_load", e, func() (map[string]int, error) { return e.Reload() })
	if err != nil {
		return fmt.Errorf("cold load: %w", err)
	}
	res["cold"] = cold
	ix := e.Index()
	res["cold_settled"] = settled()
	res["footprint_before_search"] = footprintOf(ix)
	if err := warmSearch(ix); err != nil {
		return fmt.Errorf("warm search: %w", err)
	}
	steady := settled()
	res["steady_after_search"] = steady
	res["footprint_after_search"] = footprintOf(ix)
	rooms, _ := e.Store.Rooms()
	var roomRows []map[string]any
	for _, r := range rooms {
		roomRows = append(roomRows, map[string]any{"messages": r.Messages, "leaves": r.Leaves})
	}
	sort.Slice(roomRows, func(a, b int) bool { return roomRows[a]["messages"].(int) > roomRows[b]["messages"].(int) })
	res["room_sizes"] = roomRows

	oldFTS := footprintOf(ix).FTSTotal
	var oldIx *search.Index
	if profDir != "" {
		// the live heap by allocation site, with only the current generation in it; the allocation
		// profile so far (cold load included) is the base for the hot rebuild's own allocations
		if err := writeProfile("allocs", filepath.Join(profDir, "allocs-before-hot.pb.gz")); err != nil {
			return err
		}
		if err := writeProfile("heap", filepath.Join(profDir, "heap-before-hot.pb.gz")); err != nil {
			return err
		}
		oldIx = e.Index() // held across the rebuild: the profile after it then holds both generations
	}
	hot, err := runPhase("hot_rebuild_unchanged", e, func() (map[string]int, error) { return e.Reload() })
	if err != nil {
		return fmt.Errorf("hot rebuild: %w", err)
	}
	res["hot"] = hot
	res["hot_old_index_fts_page_bytes"] = oldFTS
	if profDir != "" {
		// the new generation's own footprint, counting the int8 rows both generations share once
		rc := search.NewRowCounter()
		_ = oldIx.EstimatedBytesWith(rc)
		nf := e.Index().EstimatedBytesWith(rc)
		res["hot_new_generation_only_estimate"] = map[string]any{"total_bytes": nf.Total, "parts_bytes": map[string]int64{"leaf_vectors": nf.Parts.LeafVectors,
			"message_vectors": nf.Parts.MessageVectors, "messages": nf.Parts.Messages, "message_index": nf.Parts.MessageIndex, "leaves": nf.Parts.Leaves, "identity": nf.Parts.Identity}}
		if err := writeProfile("allocs", filepath.Join(profDir, "allocs-after-hot.pb.gz")); err != nil {
			return err
		}
		if err := writeProfile("heap", filepath.Join(profDir, "heap-after-hot-with-old.pb.gz")); err != nil {
			return err
		}
		res["hot_settled_both_generations_held"] = settled()
		oldIx = nil
	}
	// right after the swap the replaced index's lexical databases are still open (closeLater): that is the
	// "retired" copy of the model
	res["hot_settled_old_fts_still_open"] = settled()
	if settleClose {
		time.Sleep(65 * time.Second) // the engine closes a replaced index's lexical databases after a minute
		s := settled()
		res["hot_settled_after_close_delay"] = s
		debug.FreeOSMemory()
		res["hot_settled_after_free_os"] = takeSnapshot()
	}
	res["footprint_after_hot"] = footprintOf(e.Index())
	raw, _ := json.Marshal(res)
	if out != "" {
		if err := writeJSONFile(out, res); err != nil {
			return err
		}
	}
	// one line of numbers for the log
	fmt.Printf("measure-reload ok bytes=%d out=%s\n", len(raw), filepath.Base(out))
	printReloadSummary(res)
	return nil
}

func printReloadSummary(res map[string]any) {
	cold, hot := res["cold"].(phaseResult), res["hot"].(phaseResult)
	st := res["steady_after_search"].(snapshot)
	base := res["baseline_after_open"].(snapshot)
	fp := res["footprint_after_search"].(footprintOut)
	fmt.Printf("cold seconds=%.1f peak_heap_mib=%.0f peak_ws_mib=%.0f gc=%d\n", cold.Seconds, mib(cold.PeakObjects), mib(cold.PeakWS), cold.GCCycles)
	fmt.Printf("steady heap_alloc_mib=%.0f (baseline %.0f) heap_inuse_mib=%.0f sys_mib=%.0f ws_mib=%.0f estimate_mib=%.0f fts_page_mib=%.0f\n",
		mib(st.HeapAlloc), mib(base.HeapAlloc), mib(st.HeapInuse), mib(st.Sys), mib(st.WorkingSet), float64(fp.TotalBytes)/(1<<20), float64(fp.FTSTotal)/(1<<20))
	fmt.Printf("hot seconds=%.1f peak_heap_mib=%.0f peak_ws_mib=%.0f gc=%d pause_ms=%.0f peak_over_steady=%.2f\n", hot.Seconds, mib(hot.PeakObjects), mib(hot.PeakWS),
		hot.GCCycles, hot.GCPauseMs, float64(hot.PeakObjects)/float64(st.HeapAlloc))
}

// measureRooms: for each rep, rebuild {rest}, settle, then rebuild {rest + target}. rank is the target's
// 1-based position in the rooms sorted by message count (largest first); restRanks are the ranks of the
// rooms in {rest} ("" = every other room).
func measureRooms(root, out string, rank int, restRanks string, reps int) error {
	if err := scratchPath(root); err != nil {
		return err
	}
	e, closeFn, _, _, err := independentGateEngine(root, false)
	if err != nil {
		return err
	}
	defer closeFn()
	rooms, err := e.Store.Rooms()
	if err != nil {
		return err
	}
	sort.SliceStable(rooms, func(a, b int) bool { return rooms[a].Messages > rooms[b].Messages })
	if rank < 1 || rank > len(rooms) {
		return errors.New("rank out of range")
	}
	target := rooms[rank-1]
	var rest []store.RoomStats
	if restRanks == "" {
		for i, r := range rooms {
			if i != rank-1 {
				rest = append(rest, r)
			}
		}
	} else {
		for _, tok := range strings.Split(restRanks, ",") {
			k, err := strconv.Atoi(strings.TrimSpace(tok))
			if err != nil || k < 1 || k > len(rooms) || k == rank {
				return errors.New("invalid rest rank")
			}
			rest = append(rest, rooms[k-1])
		}
	}
	names := func(rs []store.RoomStats, extra ...string) []string {
		out := []string{}
		for _, r := range rs {
			out = append(out, r.Room.Room)
		}
		return append(out, extra...)
	}
	restMessages := 0
	for _, r := range rest {
		restMessages += r.Messages
	}
	res := map[string]any{"env": currentEnv(e, root), "target_rank": rank, "target_messages": target.Messages, "target_leaves": target.Leaves,
		"rest_rooms": len(rest), "rest_messages": restMessages, "estimate_room_bytes_model": e.EstimateRoomBytes(target.Messages)}
	var repsOut []map[string]any
	for rep := 0; rep < reps; rep++ {
		row := map[string]any{"rep": rep + 1}
		e.Settings.ResidentRoomsForMeasurement = names(rest)
		if _, err := e.Reload(); err != nil {
			return fmt.Errorf("rest rebuild: %w", err)
		}
		if err := warmSearch(e.Index()); err != nil {
			return fmt.Errorf("warm search: %w", err)
		}
		before := settled()
		row["rest_settled"] = before
		restEstimate := e.Index().EstimatedBytes().Total
		row["rest_estimate_bytes"] = restEstimate
		e.Settings.ResidentRoomsForMeasurement = names(rest, target.Room.Room)
		ph, err := runPhase("add_room", e, func() (map[string]int, error) { return e.Reload() })
		if err != nil {
			return fmt.Errorf("add room: %w", err)
		}
		row["add"] = ph
		if err := warmSearch(e.Index()); err != nil {
			return fmt.Errorf("warm search: %w", err)
		}
		after := settled()
		row["after_settled"] = after
		fp := e.Index().EstimatedBytes()
		row["after_estimate_bytes"] = fp.Total
		if rf, ok := fp.PerRoom[target.Room.Room]; ok {
			row["target_room_estimate_bytes"] = rf.Bytes
		}
		row["heap_alloc_delta_bytes"] = int64(after.HeapAlloc) - int64(before.HeapAlloc)
		row["estimate_delta_bytes"] = fp.Total - restEstimate
		repsOut = append(repsOut, row)
	}
	res["reps"] = repsOut
	if out != "" {
		if err := writeJSONFile(out, res); err != nil {
			return err
		}
	}
	for _, row := range repsOut {
		ph := row["add"].(phaseResult)
		fmt.Printf("rank=%d target_messages=%d rest_messages=%d rep=%v seconds=%.1f peak_heap_mib=%.0f peak_ws_mib=%.0f heap_delta_mib=%.1f estimate_delta_mib=%.1f gc=%d\n",
			rank, target.Messages, restMessages, row["rep"], ph.Seconds, mib(ph.PeakObjects), mib(ph.PeakWS),
			float64(row["heap_alloc_delta_bytes"].(int64))/(1<<20), float64(row["estimate_delta_bytes"].(int64))/(1<<20), ph.GCCycles)
	}
	return nil
}
