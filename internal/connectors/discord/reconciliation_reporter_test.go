package discord

import (
	"os"
	"path/filepath"
	"testing"

	"masamong/internal/application"
	"masamong/internal/fsutil"
)

// The doctor and the console read the blocked count through this interface; a rename of the method
// would silently turn the operator-visible warning off, so the assertion is made at compile time.
var _ application.DiscordReconciliationReporter = (*Gateway)(nil)

// A quarantined delete file is the state docs/memory-operations.md §17 asks an operator to handle;
// the reporter must show it, and show zero again once the file is moved to resolved/.
func TestGatewayReportsBlockedReconciliationFiles(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if n := g.ReconciliationBlocked(); n != 0 {
		t.Fatalf("fresh gateway reports %d blocked files", n)
	}
	file := filepath.Join(dir, "d-abc.json.corrupt")
	if err := fsutil.WriteAtomic(file, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := g.refreshReconciliationBlocked(g.svc.DataRoot, dir); err != nil {
		t.Fatal(err)
	}
	if n := g.ReconciliationBlocked(); n != 1 {
		t.Fatalf("ReconciliationBlocked = %d, want 1", n)
	}
	resolved := filepath.Join(dir, "resolved")
	if err := os.MkdirAll(resolved, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(file, filepath.Join(resolved, filepath.Base(file))); err != nil {
		t.Fatal(err)
	}
	if err := g.refreshReconciliationBlocked(g.svc.DataRoot, dir); err != nil {
		t.Fatal(err)
	}
	if n := g.ReconciliationBlocked(); n != 0 {
		t.Fatalf("after moving to resolved/, ReconciliationBlocked = %d, want 0", n)
	}
}
