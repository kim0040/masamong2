package storage

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

// Long reads (loading the memory index, a backup) use their own connections: writes go on while they
// run, and the reader connections refuse writes.
func TestSQLiteReadersRunBesideTheWriter(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.DB.Exec(`CREATE TABLE t(id INTEGER PRIMARY KEY, v TEXT)`); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		if _, err := s.DB.Exec(`INSERT INTO t(v) VALUES('x')`); err != nil {
			t.Fatal(err)
		}
	}
	// hold a read open on the reader pool, as a long scan does
	rows, err := s.Reader().Query(`SELECT id FROM t`)
	if err != nil {
		t.Fatal(err)
	}
	if !rows.Next() {
		t.Fatal("no rows")
	}
	done := make(chan error, 1)
	go func() {
		_, err := s.DB.Exec(`INSERT INTO t(v) VALUES('during read')`)
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("a write waited for a reader")
	}
	rows.Close()
	var n int
	if err := s.Reader().QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil || n != 101 {
		t.Fatalf("reader sees %d rows after the write: %v", n, err)
	}
	if _, err := s.Reader().Exec(`INSERT INTO t(v) VALUES('no')`); err == nil {
		t.Fatal("reader connection accepted a write")
	}
	// a backup copy is made on a connection of its own, while the writer holds a transaction open
	tx, err := s.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(`INSERT INTO t(v) VALUES('uncommitted')`); err != nil {
		t.Fatal(err)
	}
	if err := s.SnapshotTo(context.Background(), filepath.Join(dir, "copy.db")); err != nil {
		t.Fatalf("snapshot: %v", err)
	}
	_ = tx.Rollback()
	c, err := Open(filepath.Join(dir, "copy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.DB.QueryRow(`SELECT COUNT(*) FROM t`).Scan(&n); err != nil || n != 101 {
		t.Fatalf("copy has %d rows: %v", n, err)
	}
}

func TestSnapshotToHonorsCancellation(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "m.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = s.SnapshotTo(ctx, filepath.Join(t.TempDir(), "cancelled.db"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled snapshot returned %v", err)
	}
}

func TestEnqueueEventWakesIdleWorker(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "events.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	wake := s.EventWake()
	done := make(chan error, 1)
	go func() {
		_, err := s.EnqueueEvent("capture", map[string]any{"id": "synthetic"})
		done <- err
	}()
	select {
	case <-wake:
	case <-time.After(2 * time.Second):
		t.Fatal("durable event enqueue did not wake the worker")
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	events, err := s.PendingEvents("capture")
	if err != nil || len(events) != 1 {
		t.Fatalf("event was not durably queued: %d, %v", len(events), err)
	}
}
