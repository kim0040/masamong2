package discord

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestReconciliationCorruptDeletesDoNotExhaustStoreFailureBudget(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	for i := 0; i < reconciliationPassFailures+2; i++ {
		if err := writeReconciliationItem(filepath.Join(dir, fmt.Sprintf("d-%02d.json", i)), messageReconciliation{Operation: "delete", MessageIDs: []string{"intent"}}); err != nil {
			t.Fatal(err)
		}
	}
	// Inject six corrupt reads followed by a healthy one, regardless of filesystem enumeration order.
	reads, applied := 0, 0
	g.reconciliationRead = func(path string) ([]byte, error) {
		reads++
		if reads <= reconciliationPassFailures+1 {
			return []byte("{"), nil
		}
		return os.ReadFile(path)
	}
	g.messageEventApply = func(messageReconciliation) error { applied++; return nil }
	err := g.drainMessageReconciliation()
	if !errors.Is(err, errReconciliationBlocked) || applied != 1 || reads != reconciliationPassFailures+2 || g.reconciliationBlocked.Load() != int64(reconciliationPassFailures+1) {
		t.Fatal("corrupt deletes delayed a healthy delete until the next backoff")
	}
	if names, err := listReconciliation(dir, "d-", 1, nil); err != nil || len(names) != 0 {
		t.Fatal("healthy delete stayed queued")
	}
}
