package logfile

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRotatingCapsSizeAndKeepsNewest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "logs", "daemon.log")
	r, err := Open(path, 1000, 3)
	if err != nil {
		t.Fatal(err)
	}
	line := strings.Repeat("x", 99) + "\n"
	for i := 0; i < 100; i++ { // 10,000 bytes through a 1,000-byte file
		if _, err := fmt.Fprintf(r, "%03d%s", i, line[3:]); err != nil {
			t.Fatal(err)
		}
	}
	_ = r.Close()
	var total int64
	for _, name := range []string{path, path + ".1", path + ".2", path + ".3"} {
		st, err := os.Stat(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if st.Size() > 1000 {
			t.Fatalf("%s is %d bytes", name, st.Size())
		}
		total += st.Size()
	}
	if _, err := os.Stat(path + ".4"); !os.IsNotExist(err) {
		t.Fatal("only Keep rotated files may remain")
	}
	cur, _ := os.ReadFile(path)
	if !strings.HasPrefix(strings.Split(strings.TrimSpace(string(cur)), "\n")[len(strings.Split(strings.TrimSpace(string(cur)), "\n"))-1], "099") {
		t.Fatalf("newest line must be in the live file: %q", cur)
	}
	if total > 4000 {
		t.Fatalf("logs use %d bytes, cap is 4000", total)
	}
}

func TestOpenRotatesAnOversizedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "daemon.log")
	if err := os.WriteFile(path, make([]byte, 5000), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := Open(path, 1000, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if st, _ := os.Stat(path); st.Size() != 0 {
		t.Fatalf("live file not rotated on open: %d bytes", st.Size())
	}
	if st, err := os.Stat(path + ".1"); err != nil || st.Size() != 5000 {
		t.Fatalf("old content must move to .1: %v", err)
	}
}
