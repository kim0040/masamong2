package launcher

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestAutostartWritesAndRemovesUserEntry(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("registry entry is exercised on Windows only by hand")
	}
	bin := t.TempDir()
	for _, n := range []string{"masamong", "masamongd"} {
		if err := os.WriteFile(filepath.Join(bin, n), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	old := executable
	executable = func() (string, error) { return filepath.Join(bin, "masamongd"), nil }
	defer func() { executable = old }()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("DISPLAY", ":0") // desktop session on Linux
	t.Setenv("MASAMONG_AUTOSTART_NO_EXEC", "1")
	root := filepath.Join(t.TempDir(), "data folder")
	if st := AutostartStatus(root); st.Enabled {
		t.Fatal("not registered yet")
	}
	st, err := SetAutostart(root, true)
	if err != nil || !st.Enabled {
		t.Fatalf("enable: %+v %v", st, err)
	}
	raw, err := os.ReadFile(st.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "data folder") || !strings.Contains(string(raw), filepath.Join(bin, "masamong")) {
		t.Fatalf("entry must run masamong for this data folder:\n%s", raw)
	}
	if other := AutostartStatus(root + "2"); other.Path == st.Path {
		t.Fatal("two data folders must not share one entry")
	}
	if st, err := SetAutostart(root, false); err != nil || st.Enabled {
		t.Fatalf("disable: %+v %v", st, err)
	}
}
