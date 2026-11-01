package application

import (
	"testing"
	"time"
)

func TestThreadNegativeDeletionCheckDoesNotWaitForDB(t *testing.T) {
	s, _ := deliveryService(t)
	if _, known := s.ThreadFor("ordinary", "member"); known {
		t.Fatal("ordinary channel is a thread")
	}
	release := holdThreadPool(t, s.Store.Reader())
	done := make(chan struct{})
	t.Cleanup(func() { release(); awaitThreadSignal(t, done, "negative deletion lookup cleanup") })
	go func() { defer close(done); s.threadDeleted("ordinary") }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Error("negative cached channel performed a deletion query")
	}
}
