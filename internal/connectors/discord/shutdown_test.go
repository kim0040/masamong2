package discord

import (
	"context"
	"testing"
	"time"

	"github.com/bwmarrin/discordgo"
)

// LIVE-20: at shutdown a reply in progress keeps its context for the grace period and is cancelled after
// it; new work is refused at once.
func TestShutdownLetsRepliesFinish(t *testing.T) {
	run, stop := context.WithCancel(context.Background())
	g := &Gateway{ctx: run, grace: shutdownGrace{turn: 150 * time.Millisecond, after: 50 * time.Millisecond}}
	g.workCtx, g.cancelWork = context.WithCancel(context.WithoutCancel(run))
	quick := make(chan error, 1)
	slow := make(chan time.Duration, 1)
	g.work(func(ctx context.Context) {
		time.Sleep(50 * time.Millisecond)
		quick <- ctx.Err()
	})
	start := time.Now()
	g.work(func(ctx context.Context) {
		<-ctx.Done()
		slow <- time.Since(start)
	})
	stop()
	sess, _ := discordgo.New("Bot x")
	g.shutdown(sess)
	if err := <-quick; err != nil {
		t.Fatalf("a reply finishing within the grace was cancelled: %v", err)
	}
	if d := <-slow; d < g.grace.turn {
		t.Fatalf("cancelled before the grace: %v", d)
	}
	ran := false
	g.work(func(context.Context) { ran = true })
	if ran || !g.stopping.Load() {
		t.Fatal("new work after shutdown")
	}
}
