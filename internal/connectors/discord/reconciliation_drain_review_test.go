package discord

import (
	"bytes"
	"context"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"masamong/internal/fsutil"
)

func TestReconciliationDrainSurvivesMigrationPanic(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	path := filepath.Join(g.svc.DataRoot, messageReconciliationFile)
	if err := fsutil.WriteAtomic(path, []byte(`[{"operation":"delete","message_ids":["intent"]}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	var logs bytes.Buffer
	g.log = slog.New(slog.NewTextHandler(&logs, nil))
	steps, applied := 0, 0
	g.legacyReconciliationStep = func(int) {
		steps++
		if steps == 1 {
			panic("private-migration-marker")
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	g.messageEventApply = func(messageReconciliation) error { applied++; cancel(); return nil }
	done := make(chan struct{})
	go func() { g.runReconciliationDrain(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("panic retry did not finish")
	}
	if steps != 2 || applied != 1 || len(reconciliationNames(t, dir)) != 0 {
		t.Fatal("non-apply panic permanently stopped the drain")
	}
	if bytes.Contains(logs.Bytes(), []byte("private-migration-marker")) {
		t.Fatal("migration panic value reached logs")
	}
}
