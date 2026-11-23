//go:build !noweb

package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"masamong/internal/action"
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

// A rollback that cannot undo the store reports the failure instead of dropping it: the second call,
// with the stored key already gone, returns the removal's failure.
func TestSecretRollbackReportsAFailureItCannotUndo(t *testing.T) {
	t.Setenv("MASAMONG_SECRET_STORE", "file")
	s, _ := testServer(t)
	st, err := s.storeSecret("rbkey", "synthetic-key", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.rollback(); err != nil {
		t.Fatalf("putting back what was stored failed: %v", err)
	}
	if err := st.rollback(); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("a rollback that could not remove the stored key reported %v", err)
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
	defer func() { _ = st.rollback() }()
	if st.where != "file" || !strings.HasPrefix(st.ref, "file:") || !strings.Contains(st.note, "longer than") {
		t.Fatalf("%+v", st)
	}
	if err := fsutil.VerifyPrivateFile(strings.TrimPrefix(st.ref, "file:")); err != nil {
		t.Fatal(err)
	}
}

// Two key saves for one provider run one after the other (secretMu). The second must read the
// reference the first one left, not the one before it: otherwise the key file the first save wrote is
// never cleaned up when the second replaces it. Here the first save is stood in for by a change of the
// provider's reference while the second save waits for the lock.
func TestSecretSaveReadsThePreviousReferenceUnderTheLock(t *testing.T) {
	s, ts := testServer(t)
	c := client()
	csrf := signIn(t, s, ts, c)
	dir := filepath.Join(s.svc.DataRoot, "secrets")
	if err := fsutil.PrivateDir(dir); err != nil {
		t.Fatal(err)
	}
	stray := filepath.Join(dir, "stray.key")
	if err := os.WriteFile(stray, []byte("synthetic-first-value\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	s.secretMu.Lock()
	done := make(chan int, 1)
	go func() {
		body := strings.NewReader(`{"target":"provider","id":"finnhub","value":"synthetic-second-value"}`)
		req, _ := http.NewRequest("POST", ts.URL+"/api/secret", body)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-CSRF-Token", csrf)
		res, err := c.Do(req)
		if err != nil {
			done <- 0
			return
		}
		res.Body.Close()
		done <- res.StatusCode
	}()
	// wait until the save is blocked on the lock
	buf := make([]byte, 1<<20)
	for blocked := false; !blocked; {
		runtime.Gosched()
		for _, g := range strings.Split(string(buf[:runtime.Stack(buf, true)]), "\n\n") {
			if strings.Contains(g, "(*Server).apiSecret") && strings.Contains(g, "sync.(*Mutex).Lock") {
				blocked = true
			}
		}
	}
	raw, _ := json.Marshal(map[string]any{"ops": []any{map[string]any{"path": []any{"providers", "finnhub", "credential_ref"}, "value": "file:" + stray}}})
	if res := s.svc.Execute(context.Background(), action.Request{Action: "config.patch", Input: raw}); !res.OK {
		s.secretMu.Unlock()
		t.Fatalf("first save stand-in: %+v", res.Error)
	}
	s.secretMu.Unlock()
	if code := <-done; code != 200 {
		t.Fatalf("second save %d", code)
	}
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("the key file the first save left is still there: %v", err)
	}
}
