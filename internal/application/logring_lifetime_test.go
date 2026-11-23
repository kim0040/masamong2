package application

import (
	"log/slog"
	"runtime"
	"testing"
	"time"
)

// The ring a captured logger writes to is its own allocation owned by the service: a handler kept
// past the service's Close retains the ring, not the service that owned it.
func TestCapturedLoggerKeepsOnlyTheRing(t *testing.T) {
	s := &Service{}
	keep := s.CaptureLogs(slog.New(slog.DiscardHandler))
	done := make(chan struct{})
	runtime.SetFinalizer(s, func(*Service) { close(done) })
	s.ring().close() // what Close does
	s = nil
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		runtime.GC()
		runtime.Gosched()
		select {
		case <-done:
			runtime.KeepAlive(keep)
			return
		default:
		}
	}
	runtime.KeepAlive(keep)
	t.Fatal("a captured logger keeps the whole service alive")
}
