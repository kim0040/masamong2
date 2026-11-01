package memlimit

import (
	"bytes"
	"errors"
	"log/slog"
	"math"
	"strings"
	"testing"
)

func TestMemoryParsers(t *testing.T) {
	for _, tt := range []struct {
		raw  string
		want uint64
		ok   bool
	}{
		{"536870912\n", 1 << 29, true}, {"4294967296", 1 << 32, true}, {"max", 0, false}, {"0", 0, false},
		{"-1", 0, false}, {"garbled", 0, false}, {"18446744073709551616", 0, false}, {"9223372036854771712", 0, false},
	} {
		n, ok := parseCgroupLimit(tt.raw)
		if ok != tt.ok || ok && n != tt.want {
			t.Fatalf("cgroup %q: %d %v", tt.raw, n, ok)
		}
	}
	for _, tt := range []struct {
		raw  string
		want uint64
		ok   bool
	}{
		{"MemFree: 123 kB\nMemTotal:       524288 kB\n", 1 << 29, true}, {"MemTotal: 8388608 kB", 1 << 33, true},
		{"MemTotal: 123 MB", 0, false}, {"MemTotal: 18446744073709551615 kB", 0, false}, {"MemTotal: -1 kB", 0, false}, {"MemTotal: 0 kB", 0, false}, {"", 0, false},
	} {
		n, ok := parseMemTotal(tt.raw)
		if ok != tt.ok || n != tt.want {
			t.Fatalf("meminfo: %d %v", n, ok)
		}
	}
	if n, ok := parseBytes("17179869184\n"); !ok || n != 1<<34 {
		t.Fatal("sysctl byte count truncated")
	}
}

func TestLinuxProcessLimits(t *testing.T) {
	const mounts = "1 0 0:1 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n2 0 0:2 / /sys/fs/cgroup/memory rw - cgroup cgroup rw,memory"
	cases := []struct {
		name   string
		files  map[string]string
		want   uint64
		source string
	}{
		{"v2 leaf", map[string]string{"/proc/self/cgroup": "0::/user.slice/app", "/proc/self/mountinfo": mounts, "/sys/fs/cgroup/user.slice/app/memory.max": "536870912", "/proc/meminfo": "MemTotal: 8388608 kB"}, 1 << 29, "cgroup v2 memory.max"},
		{"v2 parent", map[string]string{"/proc/self/cgroup": "0::/user.slice/app", "/proc/self/mountinfo": mounts, "/sys/fs/cgroup/user.slice/app/memory.max": "max", "/sys/fs/cgroup/user.slice/memory.max": "1073741824"}, 1 << 30, "cgroup v2 memory.max"},
		{"container mount root", map[string]string{"/proc/self/cgroup": "0::/docker/container/app", "/proc/self/mountinfo": "1 0 0:1 /docker/container /sys/fs/cgroup rw - cgroup2 cgroup rw", "/sys/fs/cgroup/app/memory.max": "536870912"}, 1 << 29, "cgroup v2 memory.max"},
		{"v1 fallback", map[string]string{"/proc/self/cgroup": "2:cpu,memory:/app", "/proc/self/mountinfo": mounts, "/sys/fs/cgroup/memory/app/memory.limit_in_bytes": "1073741824"}, 1 << 30, "cgroup v1 memory.limit_in_bytes"},
		{"v1 unlimited", map[string]string{"/sys/fs/cgroup/memory/memory.limit_in_bytes": "9223372036854771712", "/proc/meminfo": "MemTotal: 524288 kB"}, 1 << 29, "/proc/meminfo MemTotal"},
		{"physical ceiling", map[string]string{"/sys/fs/cgroup/memory.max": "4294967296", "/proc/meminfo": "MemTotal: 1048576 kB"}, 1 << 30, "/proc/meminfo MemTotal"},
		{"unknown", map[string]string{}, 0, "unset"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			read := func(p string) ([]byte, error) {
				raw, ok := tt.files[p]
				if !ok {
					return nil, errors.New("fixture absent")
				}
				return []byte(raw), nil
			}
			n, source := linuxMemory(read)
			if n != tt.want || source != tt.source {
				t.Fatalf("%d %s, want %d %s", n, source, tt.want, tt.source)
			}
		})
	}
}

func TestAutomaticMemoryLimit(t *testing.T) {
	for _, tt := range []struct {
		name           string
		env            bool
		memory         uint64
		existing, want int64
		automatic      bool
	}{
		{"512 MiB", false, 1 << 29, math.MaxInt64, 322122547, true},
		{"1 GiB", false, 1 << 30, math.MaxInt64, 644245094, true},
		{"32 bit host count", false, 1 << 34, math.MaxInt64, 10307921510, true},
		{"explicit environment", true, 1 << 29, 123456789, 123456789, false},
		{"unknown", false, 0, math.MaxInt64, 0, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			n := tt.existing
			detected := false
			applied := false
			st := configure(func(string) (string, bool) { return "never logged", tt.env }, func() (uint64, string) { detected = true; return tt.memory, "fixture" }, func(v int64) int64 {
				old := n
				if v >= 0 {
					n = v
					applied = true
				}
				return old
			})
			if st.Bytes != tt.want || st.Automatic != tt.automatic || applied != tt.automatic {
				t.Fatalf("%+v applied=%v", st, applied)
			}
			if tt.env && (detected || st.Source != "GOMEMLIMIT") {
				t.Fatal("explicit GOMEMLIMIT was not respected")
			}
		})
	}
}

func TestAutomaticLimitLogsOnceWithoutEnvironmentValue(t *testing.T) {
	t.Setenv("GOMEMLIMIT", "unprinted fixture value")
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	ApplyAutomatic(logger)
	ApplyAutomatic(logger)
	if strings.Count(output.String(), "\n") != 1 || strings.Contains(output.String(), "unprinted fixture value") {
		t.Fatal("memory limit logging repeated or exposed an environment value")
	}
	st := Current()
	if st.Source != "GOMEMLIMIT" || st.Automatic {
		t.Fatalf("status: %+v", st)
	}
}
