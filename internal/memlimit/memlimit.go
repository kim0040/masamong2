// Package memlimit reserves space for SQLite allocations outside the Go heap on small machines.
package memlimit

import (
	"log/slog"
	"math"
	"os"
	"runtime/debug"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
)

// Status is process-wide, including when a caller supplied GOMEMLIMIT. Zero bytes means unlimited.
type Status struct {
	Bytes     int64  `json:"bytes"`
	Source    string `json:"source"`
	Automatic bool   `json:"automatic"`
}

var configured atomic.Pointer[Status]
var once sync.Once

// ApplyAutomatic is called once by the daemon before opening its store. SQLite's allocator and FTS
// databases are outside the Go heap, so leave 40 percent of the detected process memory for them.
func ApplyAutomatic(log *slog.Logger) {
	once.Do(func() {
		st := configure(os.LookupEnv, detect, debug.SetMemoryLimit)
		configured.Store(&st)
		log.Info("Go soft memory limit", "bytes", st.Bytes, "source", st.Source, "automatic", st.Automatic)
	})
}

func configure(env func(string) (string, bool), memory func() (uint64, string), set func(int64) int64) Status {
	if _, present := env("GOMEMLIMIT"); present {
		return status(set(-1), "GOMEMLIMIT", false)
	}
	available, source := memory()
	if available == 0 {
		return status(set(-1), "unset", false)
	}
	// Divide before multiplying: physical and cgroup limits need not fit in a 32-bit int.
	limit := available/5*3 + available%5*3/5
	if limit > math.MaxInt64 {
		limit = math.MaxInt64
	}
	if limit == 0 {
		return status(set(-1), "unset", false)
	}
	set(int64(limit))
	return status(int64(limit), source, true)
}

func status(n int64, source string, automatic bool) Status {
	if n == math.MaxInt64 {
		n = 0
	}
	return Status{Bytes: n, Source: source, Automatic: automatic}
}

// Current never reads environment values or machine files on an Action request.
func Current() Status {
	if st := configured.Load(); st != nil {
		return *st
	}
	source := "unset"
	if _, present := os.LookupEnv("GOMEMLIMIT"); present {
		source = "GOMEMLIMIT"
	}
	return status(debug.SetMemoryLimit(-1), source, false)
}

func parseBytes(raw string) (uint64, bool) {
	n, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
	return n, err == nil && n > 0
}

func parseCgroupLimit(raw string) (uint64, bool) {
	n, ok := parseBytes(raw)
	// v2 uses "max"; v1 commonly uses PAGE_COUNTER_MAX close to MaxInt64 for unlimited.
	return n, ok && n < 1<<60
}

// AvailableBytes is the memory this process may use, in bytes. Zero means the
// platform could not be read. hostprofile calls this instead of parsing cgroup
// files itself: the ceiling (cgroup v2, then v1, then physical RAM, including a
// parent's tighter limit) has to stay the same number the soft limit is based on.
func AvailableBytes() uint64 {
	n, _ := detect()
	return n
}

func parseMemTotal(raw string) (uint64, bool) {
	for _, line := range strings.Split(raw, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 3 || fields[0] != "MemTotal:" || fields[2] != "kB" {
			continue
		}
		n, ok := parseBytes(fields[1])
		if !ok || n > math.MaxUint64/1024 {
			return 0, false
		}
		return n * 1024, true
	}
	return 0, false
}
