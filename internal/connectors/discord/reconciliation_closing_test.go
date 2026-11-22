package discord

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"masamong/internal/application"
	"masamong/internal/memory/engine"
)

// A delete that reaches the gateway once the service has begun closing is not reported as applied: the
// service says it is closing, the gateway queues the change without retrying, and the next start applies it.
func TestDeleteAfterServiceCloseIsQueuedAndAppliedOnRestart(t *testing.T) {
	g, dir := reconciliationTestGateway(t)
	waits := 0
	g.recon.wait = func(context.Context, time.Duration) bool { waits++; return true }
	root := g.svc.DataRoot
	eng, err := g.svc.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m1", AuthorID: "u1", AuthorName: "u",
		Content: "a synthetic message the author will delete", At: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	_ = g.svc.Close() // shutdown has begun while a Discord handler still runs

	if err := g.svc.MessagesDeleted("g", "c", []string{"m1"}); !errors.Is(err, application.ErrServiceClosing) {
		t.Fatalf("MessagesDeleted after Close = %v, want ErrServiceClosing", err)
	}
	if err := g.svc.MessageEdited("g", "c", "m1", "new words", time.Now()); !errors.Is(err, application.ErrServiceClosing) {
		t.Fatalf("MessageEdited after Close = %v, want ErrServiceClosing", err)
	}
	if _, err := g.svc.MessageKeptItsWords("g", "c", "m1", "same", time.Now()); !errors.Is(err, application.ErrServiceClosing) {
		t.Fatalf("MessageKeptItsWords after Close = %v, want ErrServiceClosing", err)
	}

	g.applyMessageChange(messageReconciliation{Operation: "delete", GuildID: "g", ChannelID: "c", MessageIDs: []string{"m1"}})
	if waits != 0 {
		t.Fatalf("a closing service was retried %d times; it should be queued at once", waits)
	}
	if queued := reconciliationNames(t, dir); len(queued) != 1 {
		t.Fatalf("reconciliation files after the delete = %v, want the delete queued", queued)
	}

	svc2, err := application.Open(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer svc2.Close()
	var live string
	if err := svc2.Store.Reader().QueryRow(`SELECT COALESCE(deleted_at,'') FROM memory_observations WHERE native_message_id='m1'`).Scan(&live); err != nil {
		t.Fatal(err)
	}
	if live != "" {
		t.Fatalf("the message is already marked deleted before the queue ran (deleted_at=%q); the check below proves nothing", live)
	}
	g2 := &Gateway{svc: svc2, ctx: context.Background(), log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	g2.recon.wait = func(context.Context, time.Duration) bool { return true }
	if err := g2.reconciler().drainMessageReconciliation(); err != nil {
		t.Fatal(err)
	}
	var deleted string
	if err := svc2.Store.Reader().QueryRow(`SELECT COALESCE(deleted_at,'') FROM memory_observations WHERE native_message_id='m1'`).Scan(&deleted); err != nil {
		t.Fatal(err)
	}
	if deleted == "" {
		t.Fatal("after restart and drain the deleted message is still live in memory")
	}
	if left := reconciliationNames(t, dir); len(left) != 0 {
		t.Fatalf("the applied delete is still queued: %v", left)
	}
}
