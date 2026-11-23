package application

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// The staged copy is the file the checks opened: after the handle was taken, renaming another file
// over the spelled path changes neither what is hashed nor what is copied.
func TestStagedFileIsHashedAndCopiedFromTheCheckedHandle(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	s := &Service{DataRoot: root}
	staging := filepath.Join(root, "rag_staging")
	if err := os.MkdirAll(staging, 0o700); err != nil {
		t.Fatal(err)
	}
	spelled := filepath.Join(staging, "notes.md")
	if err := os.WriteFile(spelled, []byte("first contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := s.openStagedFile("notes.md")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	sum, size, err := hashFile(f)
	want := sha256.Sum256([]byte("first contents"))
	if err != nil || sum != hex.EncodeToString(want[:]) || size != int64(len("first contents")) {
		t.Fatalf("hash of the checked file: %s %d %v", sum, size, err)
	}
	// the spelled path now names different content
	swapped := filepath.Join(staging, "swapped.md")
	if err := os.WriteFile(swapped, []byte("second contents"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(swapped, spelled); err != nil {
		// Windows refuses to replace a file another handle holds open (Go opens without
		// FILE_SHARE_DELETE): the path cannot be swapped at all, and the copy below still checks the handle
		if runtime.GOOS != "windows" || !errors.Is(err, fs.ErrPermission) {
			t.Fatal(err)
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(root, "copy.md")
	if err := copyFile(f, dest); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dest)
	if err != nil || string(got) != "first contents" {
		t.Fatalf("the copy must be the checked file, not the path's new contents: %q %v", got, err)
	}
}
