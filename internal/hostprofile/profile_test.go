package hostprofile

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestDetectFromTiersAndEnv(t *testing.T) {
	const (
		gib   = uint64(1) << 30
		small = uint64(3) << 30 / 2 // 1.5 GiB
		large = uint64(6) << 30
	)
	medium := Profile{Tier: TierMedium, ReaderConns: 4, SQLiteCacheKiB: sqliteDefaultCacheKiB, DecodeSlots: 1, FortuneGenerations: 2, PendingRecountGap: time.Minute}
	smallP := Profile{Tier: TierSmall, ReaderConns: 2, SQLiteCacheKiB: sqliteDefaultCacheKiB, DecodeSlots: 1, FortuneGenerations: 1, PendingRecountGap: 2 * time.Minute}
	cases := []struct {
		name       string
		cpus       int
		mem        uint64
		env        string
		want       string
		source     string
		readers    int
		warn       bool
		checkKnobs bool
	}{
		{name: "one core even with ram", cpus: 1, mem: 8 * gib, want: TierSmall, source: "auto", checkKnobs: true},
		{name: "zero cpus", cpus: 0, mem: 8 * gib, want: TierSmall, source: "auto"},
		{name: "known memory under 1.5 GiB", cpus: 8, mem: small - 1, want: TierSmall, source: "auto", checkKnobs: true},
		{name: "exactly 1.5 GiB is not small", cpus: 2, mem: small, want: TierMedium, source: "auto", checkKnobs: true},
		{name: "two cores and 4 GiB", cpus: 2, mem: 4 * gib, want: TierMedium, source: "auto"},
		{name: "three cores and lots of ram", cpus: 3, mem: 16 * gib, want: TierMedium, source: "auto"},
		{name: "four cores but under 6 GiB", cpus: 4, mem: large - 1, want: TierMedium, source: "auto"},
		{name: "four cores and exactly 6 GiB", cpus: 4, mem: large, want: TierLarge, source: "auto", readers: 8},
		{name: "eight cores", cpus: 8, mem: 8 * gib, want: TierLarge, source: "auto", readers: 12},
		{name: "six cores caps at 12", cpus: 6, mem: 8 * gib, want: TierLarge, source: "auto", readers: 12},
		{name: "five cores", cpus: 5, mem: 8 * gib, want: TierLarge, source: "auto", readers: 10},
		{name: "unknown memory never large", cpus: 8, mem: 0, want: TierMedium, source: "auto", checkKnobs: true},
		{name: "unknown memory one core is small", cpus: 1, mem: 0, want: TierSmall, source: "auto"},
		{name: "env small on a large host", cpus: 8, mem: 8 * gib, env: "small", want: TierSmall, source: "env", checkKnobs: true},
		{name: "env large on a small host", cpus: 1, mem: 512 << 20, env: "large", want: TierLarge, source: "env", readers: 2},
		{name: "env medium", cpus: 8, mem: 8 * gib, env: "medium", want: TierMedium, source: "env", checkKnobs: true},
		{name: "trimmed env", cpus: 8, mem: 8 * gib, env: " large ", want: TierLarge, source: "env", readers: 12},
		{name: "empty env is auto", cpus: 2, mem: 2 * gib, env: "  ", want: TierMedium, source: "auto"},
		{name: "invalid env keeps auto and warns", cpus: 2, mem: 2 * gib, env: "LARGE", want: TierMedium, source: "auto", warn: true, checkKnobs: true},
		{name: "invalid env on a large host", cpus: 8, mem: 8 * gib, env: "turbo", want: TierLarge, source: "auto", readers: 12, warn: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectFrom(tc.cpus, tc.mem, tc.env)
			if got.Tier != tc.want || got.Source != tc.source || got.CPUs != tc.cpus || got.MemoryBytes != tc.mem {
				t.Fatalf("got %+v", got)
			}
			if (EnvWarning(tc.env) != "") != tc.warn {
				t.Fatalf("warning %q", EnvWarning(tc.env))
			}
			if tc.mem == 0 && !strings.Contains(got.DoctorDetail(), "unknown") {
				t.Fatalf("detail %q", got.DoctorDetail())
			}
			if tc.mem > 0 && strings.Contains(got.DoctorDetail(), "unknown") {
				t.Fatalf("detail %q", got.DoctorDetail())
			}
			wantKnobs := medium
			if tc.want == TierSmall {
				wantKnobs = smallP
			}
			if tc.want == TierLarge {
				wantKnobs = Profile{ReaderConns: tc.readers, SQLiteCacheKiB: largeCacheKiB, DecodeSlots: 2, FortuneGenerations: 4, PendingRecountGap: 20 * time.Second}
			}
			if tc.checkKnobs || tc.want == TierLarge {
				if got.ReaderConns != wantKnobs.ReaderConns || got.SQLiteCacheKiB != wantKnobs.SQLiteCacheKiB ||
					got.DecodeSlots != wantKnobs.DecodeSlots || got.FortuneGenerations != wantKnobs.FortuneGenerations ||
					got.PendingRecountGap != wantKnobs.PendingRecountGap {
					t.Fatalf("knobs %+v want readers %d cache %d slots %d gens %d gap %s",
						got, wantKnobs.ReaderConns, wantKnobs.SQLiteCacheKiB, wantKnobs.DecodeSlots, wantKnobs.FortuneGenerations, wantKnobs.PendingRecountGap)
				}
			}
		})
	}
}

func TestCurrentStaysMediumUntilApply(t *testing.T) {
	prev := Current()
	t.Cleanup(func() { Apply(prev) })
	// WHY: drop any profile a previous test installed, then read the unset default.
	applied.Store(nil)
	got := Current()
	if got.Tier != TierMedium || got.ReaderConns != 4 || got.SQLiteCacheKiB != sqliteDefaultCacheKiB ||
		got.DecodeSlots != 1 || got.FortuneGenerations != 2 || got.PendingRecountGap != time.Minute {
		t.Fatalf("unset profile %+v", got)
	}
	forced := DetectFrom(8, 8<<30, "large")
	Apply(forced)
	if Current().Tier != TierLarge || Current().ReaderConns != 12 {
		t.Fatalf("applied %+v", Current())
	}
}

func TestInvalidPerfEnvWarnsOnce(t *testing.T) {
	var buf bytes.Buffer
	old := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(old) })
	t.Setenv(envPerf, "turbo")
	perfWarnOnce = sync.Once{}
	first := Detect()
	second := Detect()
	if first.Source != "auto" || second.Source != "auto" {
		t.Fatalf("invalid env selected a tier: %+v %+v", first, second)
	}
	if strings.Contains(buf.String(), "turbo") {
		t.Fatalf("log contains the variable value: %s", buf.String())
	}
	if n := strings.Count(buf.String(), "MASAMONG_PERF"); n != 1 {
		t.Fatalf("warnings = %d\n%s", n, buf.String())
	}
}

func TestDoctorDetail(t *testing.T) {
	p := DetectFrom(8, 8<<30, "env-no")
	// invalid env, so auto on a large host
	if p.DoctorDetail() != "large, 8 CPU, 8192 MiB, auto" {
		t.Fatalf("detail %q", p.DoctorDetail())
	}
	p = DetectFrom(1, 0, "small")
	if p.DoctorDetail() != "small, 1 CPU, unknown, env" {
		t.Fatalf("detail %q", p.DoctorDetail())
	}
}
