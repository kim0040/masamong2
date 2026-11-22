package application_test

import (
	"context"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/errclass"
)

// serverLogWorkers counts the goroutines running the server log's sender.
func serverLogWorkers() int {
	buf := make([]byte, 1<<22)
	return strings.Count(string(buf[:runtime.Stack(buf, true)]), "application.(*Service).serverLogWorker(")
}

// panicLogGateway's channel posts panic.
type panicLogGateway struct {
	logGateway
	tried atomic.Int32
}

func (g *panicLogGateway) PostChannel(context.Context, string, string, string, *action.Card) (string, error) {
	g.tried.Add(1)
	panic("synthetic post failure")
}

// The server log's sender belongs to the service: it ends when the service closes, and a post that panics
// is recovered instead of taking the daemon down.
func TestServerLogWorkerIsOwnedByTheService(t *testing.T) {
	closeWithin := func(t *testing.T, close func() error) {
		t.Helper()
		done := make(chan error, 1)
		go func() { done <- close() }()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("Close waits for the server log")
		}
	}
	t.Run("ends at close", func(t *testing.T) {
		svc, gw := logSvc(t)
		svc.LogProblem("g", "9000001", "reply_failed", "", errclass.Timeout)
		gw.posted(t, "9000002", 1)
		running := serverLogWorkers()
		if running == 0 {
			t.Fatal("precondition: the sender runs after a post")
		}
		closeWithin(t, svc.Close)
		waitFor(t, "the sender to end", func() bool { return serverLogWorkers() < running })
	})
	t.Run("a panicking post is recovered", func(t *testing.T) {
		svc, base := logSvc(t)
		gw := &panicLogGateway{logGateway: logGateway{fakeGateway: base.fakeGateway}}
		svc.Discord = gw
		svc.LogProblem("g", "9000001", "reply_failed", "", errclass.Timeout)
		waitFor(t, "the post attempt", func() bool { return gw.tried.Load() == 1 })
		closeWithin(t, svc.Close) // reaching here means the process survived the panic
	})
}
