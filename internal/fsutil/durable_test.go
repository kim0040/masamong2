package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteDurableWritesAndReplaces(t *testing.T) {
	p := filepath.Join(t.TempDir(), "state.json")
	for _, body := range []string{"one", "two"} {
		if err := WriteDurable(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(p)
		if err != nil || string(got) != body {
			t.Fatalf("%q %v", got, err)
		}
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("perm %04o", fi.Mode().Perm())
		}
	}
	entries, err := os.ReadDir(filepath.Dir(p))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Fatalf("temporary file left behind: %q", e.Name())
		}
	}
}

func TestWriteDurableFailureLeavesNothing(t *testing.T) {
	dir := t.TempDir()
	if runtime.GOOS != "windows" {
		if err := os.Chmod(dir, 0o500); err != nil {
			t.Fatal(err)
		}
		defer func() { _ = os.Chmod(dir, 0o700) }()
	} else {
		t.Skip("POSIX modes")
	}
	err := WriteDurable(filepath.Join(dir, "x"), []byte("v"), 0o600)
	if err == nil {
		t.Fatal("a write into a read-only folder reported success")
	}
	entries, rerr := os.ReadDir(dir)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if len(entries) != 0 {
		t.Fatalf("leftovers: %v", entries)
	}
}

func TestRemoveDurable(t *testing.T) {
	dir := t.TempDir()
	if err := RemoveDurable(filepath.Join(dir, "missing")); err != nil {
		t.Fatalf("a missing file is not an error: %v", err)
	}
	p := filepath.Join(dir, "present")
	if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RemoveDurable(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Fatal("the file was not removed")
	}
}

func TestRenameDurableSyncsBothFolders(t *testing.T) {
	srcDir, dstDir := t.TempDir(), t.TempDir()
	src := filepath.Join(srcDir, "tmp-file")
	dst := filepath.Join(dstDir, "final")
	if err := os.WriteFile(src, []byte("v"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RenameDurable(src, dst); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatal("the source is still there")
	}
	if got, err := os.ReadFile(dst); err != nil || string(got) != "v" {
		t.Fatalf("%q %v", got, err)
	}
}
