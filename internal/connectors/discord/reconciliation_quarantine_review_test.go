package discord

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"masamong/internal/fsutil"
)

func TestReconciliationLegacyQuarantineScanOutsideQueueLock(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	path := filepath.Join(g.svc.DataRoot, messageReconciliationFile+".corrupt")
	if err := fsutil.WriteAtomic(path, []byte(`[{"operation":"edit"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	queueLocked := false
	scans := 0
	g.legacyReconciliationReader = func(r io.Reader) io.Reader {
		scans++
		if g.reconciliationMu.TryLock() {
			g.reconciliationMu.Unlock()
		} else {
			queueLocked = true
		}
		return r
	}
	if err := g.refreshReconciliationBlocked(g.svc.DataRoot, dir); err != nil {
		t.Fatal(err)
	}
	if scans != 1 || queueLocked {
		t.Fatal("legacy quarantine scan held the queue lock")
	}
}

func TestReconciliationLegacyQuarantineClassificationUsesSizeAndMtime(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	path := filepath.Join(g.svc.DataRoot, messageReconciliationFile+".corrupt")
	body := []byte(`[{"operation":"edit  "}]`)
	if err := fsutil.WriteAtomic(path, body, 0o600); err != nil {
		t.Fatal(err)
	}
	scans := 0
	g.legacyReconciliationReader = func(r io.Reader) io.Reader { scans++; return r }
	refresh := func(wantScans int, wantBlocked int64) {
		t.Helper()
		if err := g.refreshReconciliationBlocked(g.svc.DataRoot, dir); err != nil {
			t.Fatal(err)
		}
		if scans != wantScans || g.reconciliationBlocked.Load() != wantBlocked {
			t.Fatal("legacy classification was rescanned or stale")
		}
	}
	refresh(1, 0)
	refresh(1, 0)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	// Same size, distinct mtime: only the timestamp can invalidate this classification.
	if err := fsutil.WriteAtomic(path, []byte(`[{"operation":"delete"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	changed := info.ModTime().Add(time.Hour)
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	refresh(2, 1)
	refresh(2, 1)
	// Same mtime, different size: only the size can invalidate this classification.
	if err := fsutil.WriteAtomic(path, []byte(`[{"operation":"edit"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	refresh(3, 0)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	refresh(3, 0)
	if err := fsutil.WriteAtomic(path, []byte(`[{"operation":"delete"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(path, changed, changed); err != nil {
		t.Fatal(err)
	}
	refresh(4, 1)
}
