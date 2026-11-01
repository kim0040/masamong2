package main

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"masamong/internal/application"
)

// recorder keeps every operation's latency and outcome per kind (a few hundred thousand entries at most).
type recorder struct {
	mu   sync.Mutex
	ops  map[string][]time.Duration
	outc map[string]map[string]int
}

func newRecorder() *recorder {
	return &recorder{ops: map[string][]time.Duration{}, outc: map[string]map[string]int{}}
}

func (r *recorder) add(kind, outcome string, d time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if outcome == "ok" {
		r.ops[kind] = append(r.ops[kind], d)
	}
	if r.outc[kind] == nil {
		r.outc[kind] = map[string]int{}
	}
	r.outc[kind][outcome]++
}

type opStats struct {
	Kind     string         `json:"kind"`
	OK       int            `json:"ok"`
	Outcomes map[string]int `json:"outcomes"`
	P50      float64        `json:"p50_ms"`
	P95      float64        `json:"p95_ms"`
	P99      float64        `json:"p99_ms"`
	Max      float64        `json:"max_ms"`
}

func (r *recorder) stats() []opStats {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []opStats
	for kind, oc := range r.outc {
		d := append([]time.Duration(nil), r.ops[kind]...)
		sort.Slice(d, func(a, b int) bool { return d[a] < d[b] })
		s := opStats{Kind: kind, OK: len(d), Outcomes: map[string]int{}}
		for k, v := range oc {
			s.Outcomes[k] = v
		}
		if len(d) > 0 {
			s.P50, s.P95, s.P99, s.Max = ms(pct(d, 0.50)), ms(pct(d, 0.95)), ms(pct(d, 0.99)), ms(d[len(d)-1])
		}
		out = append(out, s)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Kind < out[b].Kind })
	return out
}

func pct(d []time.Duration, p float64) time.Duration {
	i := int(float64(len(d)-1) * p)
	return d[i]
}

func ms(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }

// sample is one point of the resource time series.
type sample struct {
	T          float64            `json:"t_sec"`
	Goroutines int                `json:"goroutines"`
	HeapMB     float64            `json:"heap_inuse_mb"`
	RSSMB      float64            `json:"rss_mb"` // whole process: also SQLite's own memory (the full-text indexes)
	HeapObjs   uint64             `json:"heap_objects"`
	SysMB      float64            `json:"sys_mb"`
	NumGC      uint32             `json:"num_gc"`
	FDs        int                `json:"open_fds"`
	DBMB       float64            `json:"db_mb"`
	WALMB      float64            `json:"wal_mb"`
	LogMB      float64            `json:"logs_mb"`
	Rows       map[string]int     `json:"rows"`
	Turns      int64              `json:"turns_done"`
	LLMFlight  int64              `json:"llm_inflight"`
	ReloadMS   int64              `json:"index_rebuild_ms"`
	TableMB    map[string]float64 `json:"table_mb,omitempty"`
}

var watchedTables = []string{"turn_log", "traces", "audit_log", "memory_embed_cache", "tool_usage",
	"memory_observations", "memory_leaves", "chat_turns", "memory_spend", "polls", "jobs"}

func takeSample(start time.Time, svc *application.Service, dataRoot string, turns, llm int64) sample {
	db := svc.Store.DB
	var m runtime.MemStats
	runtime.ReadMemStats(&m)
	s := sample{T: time.Since(start).Seconds(), Goroutines: runtime.NumGoroutine(), HeapMB: mb(m.HeapInuse),
		RSSMB: rssMB(), HeapObjs: m.HeapObjects, SysMB: mb(m.Sys), NumGC: m.NumGC, FDs: openFDs(), Turns: turns, LLMFlight: llm, Rows: map[string]int{}}
	if svc.Store.IsSQLite() {
		s.DBMB = fileMB(filepath.Join(dataRoot, "masamong.db"))
		s.WALMB = fileMB(filepath.Join(dataRoot, "masamong.db-wal"))
	} else {
		s.DBMB = tidbSizeMB(db)
	}
	s.LogMB = dirMB(filepath.Join(dataRoot, "logs"))
	for _, t := range watchedTables {
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM ` + t).Scan(&n); err == nil {
			s.Rows[t] = n
		} else {
			s.Rows[t] = -1
		}
	}
	if eng, err := svc.MemoryEngine(); err == nil {
		s.ReloadMS = eng.LastReload().Milliseconds()
	}
	return s
}

// payload is the column that carries each table's bulk (vectors, bodies, texts).
var payload = map[string]string{"turn_log": "body", "traces": "body", "audit_log": "summary", "memory_embed_cache": "vec",
	"memory_observations": "content", "chat_turns": "content"}

func tableMB(db *sql.DB) map[string]float64 {
	out := map[string]float64{}
	for t, c := range payload {
		var n sql.NullFloat64
		if err := db.QueryRow(`SELECT SUM(LENGTH(` + c + `)) FROM ` + t).Scan(&n); err == nil {
			out[t] = n.Float64 / (1 << 20)
		}
	}
	// leaf vectors and summaries: every BLOB/TEXT column of memory_leaves
	var n sql.NullFloat64
	if err := db.QueryRow(`SELECT SUM(COALESCE(LENGTH(leaf_vec),0)+COALESCE(LENGTH(summary_vec),0)+COALESCE(LENGTH(body),0)+COALESCE(LENGTH(summary),0)) FROM memory_leaves`).Scan(&n); err == nil {
		out["memory_leaves"] = n.Float64 / (1 << 20)
	}
	return out
}

func mb(b uint64) float64 { return float64(b) / (1 << 20) }

func fileMB(p string) float64 {
	st, err := os.Stat(p)
	if err != nil {
		return 0
	}
	return float64(st.Size()) / (1 << 20)
}

func dirMB(p string) float64 {
	var total int64
	_ = filepath.Walk(p, func(_ string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return float64(total) / (1 << 20)
}

// openFDs counts this process's open file descriptors (Linux /proc; open handles
// on Windows; -1 elsewhere).
// rssMB is the process's resident memory (Linux; working set on Windows; -1 elsewhere).
func rssMB() float64 {
	raw, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return platformRSSMB()
	}
	for _, line := range strings.Split(string(raw), "\n") {
		if rest, ok := strings.CutPrefix(line, "VmRSS:"); ok {
			var kb float64
			if _, err := fmt.Sscan(strings.TrimSuffix(strings.TrimSpace(rest), " kB"), &kb); err == nil {
				return kb / 1024
			}
		}
	}
	return -1
}

func openFDs() int {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return platformHandles()
	}
	return len(ents)
}

// slope is the least-squares slope of y over x (units of y per unit of x).
func slope(x, y []float64) float64 {
	n := float64(len(x))
	if n < 2 {
		return 0
	}
	var sx, sy, sxx, sxy float64
	for i := range x {
		sx += x[i]
		sy += y[i]
		sxx += x[i] * x[i]
		sxy += x[i] * y[i]
	}
	den := n*sxx - sx*sx
	if den == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / den
}
