package discord

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

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

// A delete Discord delivered is part of the gateway's work in progress: while its handler still cleans up
// (before it stores the deletion) Wait does not report that everything finished, so the daemon does not go
// on to close the database and exit under it. Once the handler ends, the message is gone from memory.
func TestGatewayWaitJoinsDeliveredDelete(t *testing.T) {
	for _, bulk := range []bool{false, true} {
		name := "delete"
		if bulk {
			name = "bulk delete"
		}
		t.Run(name, func(t *testing.T) {
			g, dir := reconciliationTestGateway(t)
			eng, err := g.svc.MemoryEngine()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := eng.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "m1", AuthorID: "u1", AuthorName: "u",
				Content: "a synthetic message the author will delete", At: time.Now().Add(-time.Minute)}); err != nil {
				t.Fatal(err)
			}
			entered, release := make(chan struct{}), make(chan struct{})
			g.cancelPictures = func(string, []string) { close(entered); <-release }
			handled := make(chan struct{})
			// delivered as DiscordGo does with what registerHandlers added: a call on its event loop
			if bulk {
				deliver := counted(&g.changes, func(s *discordgo.Session, m *discordgo.MessageDeleteBulk) {
					defer close(handled)
					g.onMessageDeleteBulk(s, m)
				})
				deliver(nil, &discordgo.MessageDeleteBulk{Messages: []string{"m1"}, GuildID: "g", ChannelID: "c"})
			} else {
				deliver := counted(&g.changes, func(s *discordgo.Session, m *discordgo.MessageDelete) {
					defer close(handled)
					g.onMessageDelete(s, m)
				})
				deliver(nil, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "m1", GuildID: "g", ChannelID: "c"}})
			}
			select {
			case <-entered:
			case <-time.After(10 * time.Second):
				t.Fatal("the delete handler never reached its cleanup")
			}
			if g.Wait(200 * time.Millisecond) {
				close(release)
				t.Fatal("Wait reported everything finished while a delivered delete was still being handled")
			}
			close(release)
			if !g.Wait(10 * time.Second) {
				t.Fatal("Wait did not finish after the delete handler ended")
			}
			select {
			case <-handled:
			default:
				t.Fatal("Wait finished before the delete handler returned")
			}
			var deleted string
			if err := g.svc.Store.Reader().QueryRow(`SELECT COALESCE(deleted_at,'') FROM memory_observations WHERE native_message_id='m1'`).Scan(&deleted); err != nil {
				t.Fatal(err)
			}
			if deleted == "" && len(reconciliationNames(t, dir)) == 0 {
				t.Fatal("the delivered delete was neither applied nor queued")
			}
		})
	}
}
