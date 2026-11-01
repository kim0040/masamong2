package application

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func TestThreadDeletionRetriesFailedTombstoneOnTick(t *testing.T) {
	s, _ := deliveryService(t)
	s.NoteThread("g", "target", "parent", false)
	s.NoteThreadParticipant("target", "member")
	if _, err := s.Store.DB.Exec(`CREATE TRIGGER reject_thread_deletion BEFORE INSERT ON thread_tombstones BEGIN SELECT RAISE(FAIL,'forced thread deletion failure'); END`); err != nil {
		t.Fatal(err)
	}
	s.ThreadDeleted("target")
	s.threads.mu.Lock()
	pending := s.threads.deleted["target"]
	s.threads.mu.Unlock()
	if !pending {
		t.Fatal("failed tombstone was not kept fail closed")
	}
	var n int
	if err := s.Store.DB.QueryRow(`SELECT count(*) FROM thread_tombstones WHERE thread_id='target'`).Scan(&n); err != nil || n != 0 {
		t.Fatal("failed transaction committed a tombstone")
	}
	if _, err := s.Store.DB.Exec(`DROP TRIGGER reject_thread_deletion`); err != nil {
		t.Fatal(err)
	}
	s.closeIdleThreads(context.Background())
	if err := s.Store.DB.QueryRow(`SELECT count(*) FROM thread_tombstones WHERE thread_id='target'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Error("minute tick did not retry failed deletion")
	}
	if err := s.Store.DB.QueryRow(`SELECT count(*) FROM thread_participants WHERE thread_id='target'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Error("retried deletion left participants")
	}
	s.threads.mu.Lock()
	pending = s.threads.deleted["target"]
	s.threads.mu.Unlock()
	if pending {
		t.Error("successful retry retained pending deletion")
	}
}

// While the database keeps refusing, the minute tick retries every pending deletion; the warning comes
// once per threadDeleteWarnEvery, not once per thread per minute.
func TestThreadDeletionRetryWarnsOncePerInterval(t *testing.T) {
	s, clk := deliveryService(t)
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelWarn}))
	if _, err := s.Store.DB.Exec(`CREATE TRIGGER reject_thread_deletion BEFORE INSERT ON thread_tombstones BEGIN SELECT RAISE(FAIL,'forced thread deletion failure'); END`); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c"} {
		s.NoteThread("g", id, "parent", false)
		s.ThreadDeleted(id)
	}
	for i := 0; i < 3; i++ {
		clk.t = clk.t.Add(time.Minute)
		s.closeIdleThreads(context.Background())
	}
	warned := func() int { return strings.Count(logs.String(), "thread deletion write failed; retrying") }
	if n := warned(); n != 1 {
		t.Fatalf("%d warnings in the first interval, want 1", n)
	}
	clk.t = clk.t.Add(threadDeleteWarnEvery)
	s.closeIdleThreads(context.Background())
	if n := warned(); n != 2 {
		t.Fatalf("%d warnings after the interval, want 2", n)
	}
}
