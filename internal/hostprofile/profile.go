// Package hostprofile picks process limits from the machine so a 1-core, 512 MB
// host stays small and a large server uses the room it has. There is no user
// setting. The daemon calls Apply(Detect()) once before it opens the store.
//
// A process that never calls Apply keeps the medium limits, which are the fixed
// limits from before tiers existed. Tests and library users open stores and
// decode pictures on build machines that are larger than a 512 MB VPS; detecting
// inside Current would raise their pools and caches and change today's behavior.
package hostprofile

import (
	"fmt"
	"log/slog"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"masamong/internal/memlimit"
)

const (
	TierSmall  = "small"
	TierMedium = "medium"
	TierLarge  = "large"

	// envPerf overrides the tier. Anything else, including empty, leaves the
	// choice to the machine. The value is never written to a log.
	envPerf = "MASAMONG_PERF"

	// sqliteDefaultCacheKiB is SQLite's own default page cache. PRAGMA cache_size
	// is negative when the unit is KiB, and the engine default is -2000.
	// WHY: store.Open does not set cache_size, so medium (and a small host, which
	// should not spend more RAM than today) must stay at 2000. The tier sheet
	// said 2048 for small and 8192 for medium. 2048 is not the engine default,
	// and 8192 would grow the cache on every typical box. Only a large server
	// raises it.
	sqliteDefaultCacheKiB = 2000
	largeCacheKiB         = 32768
)

// Profile is the tier and the limits that follow from it. MemoryBytes is 0 when
// the process ceiling could not be read. Source is "env" when MASAMONG_PERF
// selected the tier and "auto" otherwise.
type Profile struct {
	Tier               string
	CPUs               int
	MemoryBytes        uint64
	Source             string
	ReaderConns        int
	SQLiteCacheKiB     int
	DecodeSlots        int
	FortuneGenerations int
	PendingRecountGap  time.Duration
}

// applied is nil until Apply. Current then returns mediumProfile, not a live
// detection, so an unset process keeps today's limits.
var applied atomic.Pointer[Profile]

var perfWarnOnce sync.Once

// Current returns the profile Apply installed, or the medium limits when none
// was installed.
func Current() Profile {
	if p := applied.Load(); p != nil {
		return *p
	}
	return mediumProfile()
}

// Apply installs p for the rest of the process. The daemon does this once with
// the result of Detect. Tests use it to pin a tier; it replaces any previous
// profile.
func Apply(p Profile) {
	cp := p
	applied.Store(&cp)
}

// Detect reads the live CPU quota, the process memory ceiling and MASAMONG_PERF.
// It does not install the profile; call Apply with the result. An invalid
// variable is ignored (the tier stays automatic) and logged once, without the
// variable's text.
func Detect() Profile {
	env := os.Getenv(envPerf)
	p := DetectFrom(runtime.GOMAXPROCS(0), memlimit.AvailableBytes(), env)
	if msg := EnvWarning(env); msg != "" {
		// WHY once: doctor and a restarted detection must not fill the log.
		// The text is fixed so a strange value cannot leak into the file.
		perfWarnOnce.Do(func() { slog.Warn(msg) })
	}
	return p
}

// DetectFrom is Detect with the machine and the variable supplied by the
// caller, so a test can cover the boundaries without reading this host.
// env is the MASAMONG_PERF value, not the whole environment.
func DetectFrom(cpus int, memoryBytes uint64, env string) Profile {
	tier := tierFor(cpus, memoryBytes)
	source := "auto"
	switch strings.TrimSpace(env) {
	case TierSmall, TierMedium, TierLarge:
		// WHY trim only: "LARGE" is not a listed value, so it stays automatic
		// and the caller logs a warning. Operators set the lowercase names.
		tier = strings.TrimSpace(env)
		source = "env"
	}
	return profileFor(tier, cpus, memoryBytes, source)
}

// EnvWarning is empty when env is empty or small, medium, or large. Otherwise
// it is the single sentence Detect logs. The raw value is not part of it.
func EnvWarning(env string) string {
	switch strings.TrimSpace(env) {
	case "", TierSmall, TierMedium, TierLarge:
		return ""
	default:
		return "MASAMONG_PERF is not small, medium, or large; choosing the tier from this machine"
	}
}

// DoctorDetail is the text after "host_profile:" in the doctor reason.
// Memory is "unknown" when the ceiling was not detected, never "0 MiB".
func (p Profile) DoctorDetail() string {
	mem := "unknown"
	if p.MemoryBytes > 0 {
		mem = fmt.Sprintf("%d MiB", p.MemoryBytes>>20)
	}
	return fmt.Sprintf("%s, %d CPU, %s, %s", p.Tier, p.CPUs, mem, p.Source)
}

func mediumProfile() Profile {
	return profileFor(TierMedium, 0, 0, "auto")
}

// tierFor maps the machine to a tier.
// WHY the order: one core, or a known ceiling under 1.5 GiB, cannot overlap a
// 125–170 MB photo decode or keep a 5-second recount off the only core. A
// large tier needs both 4+ CPUs and a known ceiling of at least 6 GiB, because
// an unread memory file must not raise limits the host might not have RAM for.
func tierFor(cpus int, memory uint64) string {
	const (
		smallMem = uint64(3) << 30 / 2 // 1.5 GiB
		largeMem = uint64(6) << 30
	)
	if cpus <= 1 || (memory > 0 && memory < smallMem) {
		return TierSmall
	}
	if cpus >= 4 && memory >= largeMem {
		return TierLarge
	}
	return TierMedium
}

func profileFor(tier string, cpus int, memory uint64, source string) Profile {
	p := Profile{Tier: tier, CPUs: cpus, MemoryBytes: memory, Source: source}
	switch tier {
	case TierSmall:
		// WHY: the 512 MB box. Two reader connections are enough for the read
		// pool; one decode and one fortune generation keep the single core and
		// the small RAM from stacking 170 MB images or queued model calls. The
		// recount gap is longer so a 5-second count cannot follow another.
		p.ReaderConns = 2
		p.SQLiteCacheKiB = sqliteDefaultCacheKiB
		p.DecodeSlots = 1
		p.FortuneGenerations = 1
		p.PendingRecountGap = 2 * time.Minute
	case TierLarge:
		// WHY: a host with RAM and cores to spare. The read pool grows with the
		// CPUs but stops at 12 so a 64-core machine does not open 128 SQLite
		// connections. Two decode slots overlap two photos (~340 MB) only when
		// the ceiling is at least 6 GiB. Four fortune generations and a 20s
		// recount use idle cores without turning the recount into a busy loop.
		p.ReaderConns = largeReaders(cpus)
		p.SQLiteCacheKiB = largeCacheKiB
		p.DecodeSlots = 2
		p.FortuneGenerations = 4
		p.PendingRecountGap = 20 * time.Second
	default:
		// WHY: today's fixed limits, so a 2-core or 4 GB host behaves as this
		// code did before tiers. The cache stays at SQLite's default (see
		// sqliteDefaultCacheKiB), not a larger medium value.
		p.Tier = TierMedium
		p.ReaderConns = 4
		p.SQLiteCacheKiB = sqliteDefaultCacheKiB
		p.DecodeSlots = 1
		p.FortuneGenerations = 2
		p.PendingRecountGap = time.Minute
	}
	return p
}

// largeReaders is min(2*CPUs, 12), and at least 1.
// WHY: SetMaxOpenConns(0) means unlimited, and 2*cpus must not overflow an int
// when a test passes a huge CPU count. An operator who forces the large tier
// on a 1-core host still gets a finite pool.
func largeReaders(cpus int) int {
	if cpus < 1 {
		return 1
	}
	if cpus > 6 {
		return 12
	}
	return 2 * cpus
}
