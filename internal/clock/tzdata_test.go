package clock

import (
	"os"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Only a host without its own zoneinfo can prove the embedded data: Linux and macOS keep one under
// /usr/share/zoneinfo (or similar), which answers LoadLocation before the embedded copy is consulted,
// so the check below would pass there without the embed. Windows has none.

func TestEmbeddedTimezoneDataWithoutHostPaths(t *testing.T) {
	if os.Getenv("MASAMONG_TZDATA_CHILD") == "1" {
		seoul, err := time.LoadLocation("Asia/Seoul")
		if err != nil {
			t.Fatal(err)
		}
		_, seoulOffset := time.Date(2026, time.January, 1, 12, 0, 0, 0, seoul).Zone()
		if seoulOffset != 9*60*60 {
			t.Fatalf("Asia/Seoul offset = %d, want +09:00", seoulOffset)
		}
		newYork, err := time.LoadLocation("America/New_York")
		if err != nil {
			t.Fatal(err)
		}
		_, winter := time.Date(2026, time.January, 1, 12, 0, 0, 0, newYork).Zone()
		_, summer := time.Date(2026, time.July, 1, 12, 0, 0, 0, newYork).Zone()
		if winter == summer {
			t.Fatalf("America/New_York has no DST offset change: winter=%d summer=%d", winter, summer)
		}
		return
	}

	if runtime.GOOS != "windows" {
		t.Skip("the host's own zoneinfo would answer instead of the embedded copy; TestEmbeddedTimezoneDataIsLinked covers this host")
	}
	missing := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestEmbeddedTimezoneDataWithoutHostPaths$")
	cmd.Env = withoutEnv(os.Environ(), "GOROOT", "ZONEINFO", "MASAMONG_TZDATA_CHILD")
	cmd.Env = append(cmd.Env,
		"GOROOT="+missing+string(os.PathSeparator)+"missing-goroot",
		"ZONEINFO="+missing+string(os.PathSeparator)+"missing-zoneinfo",
		"MASAMONG_TZDATA_CHILD=1",
	)
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("embedded timezone subprocess failed: %v\n%s", err, output)
	}
}

func withoutEnv(env []string, names ...string) []string {
	filtered := make([]string, 0, len(env))
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		remove := false
		for _, target := range names {
			if strings.EqualFold(name, target) {
				remove = true
				break
			}
		}
		if !remove {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// TestEmbeddedTimezoneDataIsLinked checks on every OS that this package pulls in time/tzdata, which is
// what makes the product's timezones work on a machine with no zoneinfo (Windows, minimal containers).
func TestEmbeddedTimezoneDataIsLinked(t *testing.T) {
	goTool, err := exec.LookPath("go")
	if err != nil {
		t.Skip("go tool not available")
	}
	out, err := exec.Command(goTool, "list", "-deps", ".").CombinedOutput()
	if err != nil {
		t.Skipf("go list failed: %v: %s", err, out)
	}
	for _, pkg := range strings.Fields(string(out)) {
		if pkg == "time/tzdata" {
			return
		}
	}
	t.Fatalf("time/tzdata is not linked into internal/clock: %s", out)
}
