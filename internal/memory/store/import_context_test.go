package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"masamong/internal/memory/corpus"
)

func TestImportCancellationDoesNotWaitForBusyWriter(t *testing.T) {
	st := openStore(t)
	locked := make(chan struct{})
	release := make(chan struct{})
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		_ = st.DB.WithWriter(func(*sql.Tx) error { close(locked); <-release; return nil })
	}()
	<-locked
	defer func() { close(release); <-stopped }()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		_, err := st.InsertObservationsContext(ctx, []corpus.Observation{mk(0, time.Now())}, nil)
		done <- err
	}()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled import waited for an unrelated writer")
	}
}
