package discord

import (
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"

	"masamong/internal/memory/engine"
)

// DiscordGo calls what registerHandlers added on its event loop and goes on with the next event; the
// handler runs later in a goroutine of its own. A delete it handed over is counted from that moment: while
// the handler has not started yet, Wait does not report that everything finished. Once the handler ends the
// message is gone from memory (or queued), and Wait finishes.
func TestGatewayWaitCountsADeleteNotYetStarted(t *testing.T) {
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
			held := make(chan struct{})
			if bulk {
				deliver := counted(&g.changes, func(s *discordgo.Session, m *discordgo.MessageDeleteBulk) { <-held; g.onMessageDeleteBulk(s, m) })
				deliver(nil, &discordgo.MessageDeleteBulk{Messages: []string{"m1"}, GuildID: "g", ChannelID: "c"})
			} else {
				deliver := counted(&g.changes, func(s *discordgo.Session, m *discordgo.MessageDelete) { <-held; g.onMessageDelete(s, m) })
				deliver(nil, &discordgo.MessageDelete{Message: &discordgo.Message{ID: "m1", GuildID: "g", ChannelID: "c"}})
			}
			if g.Wait(200 * time.Millisecond) {
				close(held)
				t.Fatal("Wait reported everything finished while a delivered delete had not started")
			}
			close(held)
			if !g.Wait(10 * time.Second) {
				t.Fatal("Wait did not finish after the delete handler ended")
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

// Every handler the gateway registers is of a type DiscordGo dispatches, DiscordGo calls them on its event
// loop, and the handler itself still runs. Wait and shutdown both close the session, and it is closed once:
// one disconnect, not two.
func TestRegisteredHandlersRunAndTheSessionClosesOnce(t *testing.T) {
	var invalid atomic.Int32
	prev := discordgo.Logger
	discordgo.Logger = func(_, _ int, format string, _ ...any) {
		if strings.Contains(format, "Invalid handler type") {
			invalid.Add(1)
		}
	}
	t.Cleanup(func() { discordgo.Logger = prev })
	sess, err := discordgo.New("Bot test")
	if err != nil {
		t.Fatal(err)
	}
	sess.LogLevel = discordgo.LogError
	g := &Gateway{sess: sess, log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	g.registerHandlers(sess)
	if !sess.SyncEvents {
		t.Fatal("DiscordGo still starts the handlers itself: a delivered change is not counted until its goroutine runs")
	}
	if n := invalid.Load(); n != 0 {
		t.Fatalf("%d registered handlers are of a type DiscordGo never calls", n)
	}
	g.connected.Store(true)
	if !g.Wait(10 * time.Second) {
		t.Fatal("Wait with nothing in progress did not finish")
	}
	g.shutdown(sess)
	deadline := time.Now().Add(10 * time.Second)
	for g.health.disconnects.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond) // a second disconnect would have arrived by now
	if n := g.health.disconnects.Load(); n != 1 {
		t.Fatalf("closing the session handed out %d disconnects, want 1 (the handler ran once)", n)
	}
	if g.connected.Load() {
		t.Fatal("the gateway still reports itself connected after shutdown")
	}
}
