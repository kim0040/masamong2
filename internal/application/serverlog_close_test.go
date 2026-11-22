package application_test

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/errclass"
)

// heldLogGateway's channel posts wait until their context ends (or a safety bound), and say when the
// first one has started.
type heldLogGateway struct {
	logGateway
	tried   atomic.Int32
	started chan struct{}
}

func (g *heldLogGateway) PostChannel(ctx context.Context, _, _, _ string, _ *action.Card) (string, error) {
	if g.tried.Add(1) == 1 {
		close(g.started)
	}
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-time.After(time.Minute):
		return "posted", nil
	}
}

// onceFailingLogGateway's first channel post panics; later ones are posted.
type onceFailingLogGateway struct {
	logGateway
	tried atomic.Int32
}

func (g *onceFailingLogGateway) PostChannel(ctx context.Context, channelID, reply, text string, card *action.Card) (string, error) {
	if g.tried.Add(1) == 1 {
		panic("synthetic post failure")
	}
	return g.logGateway.PostChannel(ctx, channelID, reply, text, card)
}

// Close ends the server log at once: the post under way is cancelled (it does not run out its own
// timeout), and an entry still queued is not started.
func TestServerLogCloseCancelsThePostUnderWay(t *testing.T) {
	svc, base := logSvc(t)
	gw := &heldLogGateway{logGateway: logGateway{fakeGateway: base.fakeGateway}, started: make(chan struct{})}
	svc.SetDiscord(gw)
	svc.LogProblem("g", "9000001", "reply_failed", "", errclass.Timeout)
	select {
	case <-gw.started:
	case <-time.After(10 * time.Second):
		t.Fatal("the first post never started")
	}
	svc.LogProblem("g", "9000003", "reply_failed", "", errclass.Timeout) // queued behind the post under way
	done := make(chan error, 1)
	go func() { done <- svc.Close() }()
	select {
	case <-done:
	case <-time.After(5 * time.Second): // a post's own timeout is 20 seconds
		t.Fatal("Close waited for the post under way")
	}
	if n := gw.tried.Load(); n != 1 {
		t.Fatalf("%d posts started; the entry queued at Close must not start", n)
	}
}

// A post that panics is recovered without ending the server log: a later entry is still posted.
func TestServerLogGoesOnAfterAPanickingPost(t *testing.T) {
	svc, base := logSvc(t)
	gw := &onceFailingLogGateway{logGateway: logGateway{fakeGateway: base.fakeGateway}}
	svc.SetDiscord(gw)
	svc.LogProblem("g", "9000001", "reply_failed", "", errclass.Timeout)
	waitFor(t, "the first post attempt", func() bool { return gw.tried.Load() == 1 })
	svc.LogProblem("g", "9000003", "reply_failed", "", errclass.Timeout)
	gw.posted(t, "9000002", 1)
}
