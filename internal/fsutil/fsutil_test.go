package fsutil

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestWriteAtomicReplaces(t *testing.T) {
	p := filepath.Join(t.TempDir(), "sub", "config.yaml")
	for _, body := range []string{"a: 1\n", "a: 2\n"} {
		if err := WriteAtomic(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := VerifyPrivateFile(p); err != nil {
			t.Fatalf("atomic replacement is not private: %v", err)
		}
		got, err := os.ReadFile(p)
		if err != nil || string(got) != body {
			t.Fatalf("%q %v", got, err)
		}
	}
	if _, err := os.Stat(p + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("temporary file left behind")
	}
}

func TestFreeBytes(t *testing.T) {
	n, err := FreeBytes(t.TempDir())
	if err != nil || n == 0 {
		t.Fatalf("%d %v", n, err)
	}
}

func TestPrivateDirTightensAFolderOpenToOthers(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(dir, 0o755)
	f := filepath.Join(dir, "db")
	if err := os.WriteFile(f, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPrivateFile(f); err != nil {
		t.Fatalf("existing child was not protected: %v", err)
	}
	if err := PrivateFile(f); err != nil {
		t.Fatal(err)
	}
	if err := VerifyPrivateFile(f); err != nil {
		t.Fatal(err)
	}
	if err := PrivateFile(filepath.Join(dir, "missing")); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateDirDoesNotFollowExternalSymlink(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	external := filepath.Join(t.TempDir(), "external")
	if err := os.WriteFile(external, []byte("outside the data root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(external, filepath.Join(dir, "external-link")); err != nil {
		t.Skipf("symbolic links unavailable: %v", err)
	}
	if err := PrivateDir(dir); err != nil {
		t.Fatalf("protect root without following its external link: %v", err)
	}
	if err := VerifyPrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(external); err != nil || string(got) != "outside the data root" {
		t.Fatalf("external target changed: %q, %v", got, err)
	}
}

func TestPrivateDirTightensNestedChildrenKeepsOwnerBitsAndSkipsLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX modes")
	}
	root := filepath.Join(t.TempDir(), "data")
	sub := filepath.Join(root, "logs")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(root, 0o755)
	_ = os.Chmod(sub, 0o755)
	script := filepath.Join(sub, "run.sh")
	if err := os.WriteFile(script, nil, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(script, 0o755)
	outside := filepath.Join(t.TempDir(), "shared.txt")
	if err := os.WriteFile(outside, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(outside, 0o644)
	if err := os.Symlink(outside, filepath.Join(root, "link")); err != nil {
		t.Fatal(err)
	}
	if err := PrivateDir(root); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]os.FileMode{sub: 0o700, script: 0o700, outside: 0o644} {
		fi, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if got := fi.Mode().Perm(); got != want {
			t.Errorf("%s mode %04o, want %04o", filepath.Base(path), got, want)
		}
	}
}
