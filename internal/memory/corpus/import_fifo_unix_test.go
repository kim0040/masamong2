//go:build !windows

package corpus

import (
	"errors"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// refuseFIFO checks that opening a named pipe is refused instead of blocking (no named pipes on Windows).
func refuseFIFO(t *testing.T, dir string) {
	t.Helper()
	fifo := filepath.Join(dir, "pipe.jsonl")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("no fifo here: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- readRawCorpus(t.Context(), fifo, nil, 1<<20, func(Observation) error { return nil }) }()
	select {
	case err := <-done:
		if !errors.Is(err, ErrNotRegularFile) {
			t.Fatalf("fifo: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("opening a pipe blocked")
	}
}
