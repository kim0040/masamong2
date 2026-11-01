//go:build !noweb

package web

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"masamong/internal/fsutil"
	"masamong/internal/secrets"
)

type failingSecretTemp struct {
	fail string
}

func (f failingSecretTemp) Chmod(os.FileMode) error {
	if f.fail == "chmod" {
		return errors.New("injected chmod failure")
	}
	return nil
}

func (f failingSecretTemp) Write(b []byte) (int, error) {
	if f.fail == "write" {
		return 0, errors.New("injected write failure")
	}
	return len(b), nil
}

func (f failingSecretTemp) Sync() error {
	if f.fail == "sync" {
		return errors.New("injected sync failure")
	}
	return nil
}

func (f failingSecretTemp) Close() error { return nil }

func TestSecretTempWritePropagatesProtectionAndDurabilityErrors(t *testing.T) {
	for _, phase := range []string{"chmod", "write", "sync"} {
		t.Run(phase, func(t *testing.T) {
			if err := writeSecretTemp(failingSecretTemp{fail: phase}, "replacement"); err == nil {
				t.Fatalf("%s failure was lost", phase)
			}
		})
	}
}

func TestStoreSecretWriteFailureKeepsPreviousCredential(t *testing.T) {
	t.Setenv("MASAMONG_SECRET_STORE", "file")
	s, _ := testServer(t)
	dir := filepath.Join(s.svc.DataRoot, "secrets")
	if err := fsutil.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "finnhub.key")
	if err := os.WriteFile(path, []byte("previous-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	oldWrite := secretTempWrite
	secretTempWrite = func(secretTempFile, string) error { return errors.New("injected disk-full write") }
	t.Cleanup(func() { secretTempWrite = oldWrite })
	if _, err := s.storeSecret("finnhub", "new-value", "file:"+path); err == nil {
		t.Fatal("failed replacement reported success")
	}
	got, err := os.ReadFile(path)
	if err != nil || string(got) != "previous-value\n" {
		t.Fatalf("previous credential changed after failed replacement: %q %v", got, err)
	}
}

func TestFileFallbackNoteNamesTheRealProtectionAndTheCause(t *testing.T) {
	long := fileFallbackNote(fmt.Errorf("write: %w", secrets.ErrKeychainValueTooLong))
	if !strings.Contains(long, "longer than") || !strings.Contains(long, "file") {
		t.Fatalf("an oversized key must say so: %s", long)
	}
	plain := fileFallbackNote(errors.New("locked"))
	if !strings.Contains(plain, "unavailable") {
		t.Fatalf("unavailable store: %s", plain)
	}
	for _, note := range []string{long, plain} {
		if runtime.GOOS == "windows" && strings.Contains(note, "0600") {
			t.Fatalf("Windows files have ACLs, not mode bits: %s", note)
		}
		if runtime.GOOS != "windows" && !strings.Contains(note, "0600") {
			t.Fatalf("Unix files are mode 0600: %s", note)
		}
	}
}

// A key too long for the Windows Credential Manager (2,560 bytes) is kept in a private file, with a
// warning the caller can show.
func TestOversizedKeyFallsBackToAFileWithAWarningOnWindows(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("the size limit is Credential Manager's")
	}
	s, _ := testServer(t)
	t.Setenv("MASAMONG_SECRET_STORE", "") // the harness keeps keys in files; this test wants the credential store tried
	st, err := s.storeSecret("longkey", strings.Repeat("k", 3000), "")
	if err != nil {
		t.Fatal(err)
	}
	defer st.rollback()
	if st.where != "file" || !strings.HasPrefix(st.ref, "file:") || !strings.Contains(st.note, "longer than") {
		t.Fatalf("%+v", st)
	}
	if err := fsutil.VerifyPrivateFile(strings.TrimPrefix(st.ref, "file:")); err != nil {
		t.Fatal(err)
	}
}
