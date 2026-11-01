package application

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"masamong/internal/action"
	"masamong/internal/fsutil"
	"masamong/internal/memory/engine"
)

func externalWriteLock(t *testing.T, s *Service) func() {
	t.Helper()
	db, err := sql.Open("sqlite", s.Store.Path()+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Begin()
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(); db.Close() })
	return func() { tx.Rollback() }
}

func TestLiveDeleteSurvivesExternalLockLongerThanBusyTimeout(t *testing.T) {
	s := privacyService(t)
	turn := captureAndAnswer(t, s, "locked-delete", "busy deletion private canary")
	unlock := externalWriteLock(t, s)
	result := make(chan error, 1)
	go func() { result <- s.MessagesDeleted("g", "c", []string{"locked-delete"}) }()
	timer := time.NewTimer(6 * time.Second)
	defer timer.Stop()
	select {
	case err := <-result:
		t.Fatalf("delete dropped before external lock released: %v", err)
	case <-timer.C:
	}
	unlock()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	s.DrainRedactions(t.Context())
	var content string
	if err := s.Store.DB.QueryRow(`SELECT content FROM memory_observations WHERE native_message_id='locked-delete'`).Scan(&content); err != nil || content != "" {
		t.Fatalf("delete missing: %q %v", content, err)
	}
	if body := turnBody(t, s, turn); strings.Contains(body, "busy deletion private canary") {
		t.Fatalf("dependent turn not redacted: %s", body)
	}
}

func TestLiveCaptureAndEditReplayFromDurableFilesAfterRestart(t *testing.T) {
	s := privacyService(t)
	root := s.DataRoot
	e, err := s.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "to-edit", AuthorID: "alice", Content: "old private canary", At: time.Now().Add(-time.Hour)}); err != nil {
		t.Fatal(err)
	}
	unlock := externalWriteLock(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err = e.CaptureContext(ctx, engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "to-capture", AuthorID: "bob", Content: "durable capture canary", At: time.Now()})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected capture timeout, got %v", err)
	}
	ctxEdit, cancelEdit := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancelEdit()
	if err := s.queueSourceChangeContext(ctxEdit, "g", "c", []string{"to-edit"}, "edit", "revised private canary", time.Now()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected edit timeout, got %v", err)
	}
	for _, kind := range []string{"capture", "source-change"} {
		if events, err := s.Store.PendingEvents(kind); err != nil || len(events) != 1 {
			t.Fatalf("%s was not durably queued: %d %v", kind, len(events), err)
		}
	}
	// Cancel the worker while the database is still locked, as during daemon shutdown.
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	unlock()
	restarted, err := Open(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var captures, edits int
		_ = restarted.Store.Reader().QueryRow(`SELECT COUNT(*) FROM memory_observations WHERE native_message_id='to-capture' AND content='durable capture canary'`).Scan(&captures)
		_ = restarted.Store.Reader().QueryRow(`SELECT COUNT(*) FROM memory_observations WHERE native_message_id='to-edit' AND content='revised private canary'`).Scan(&edits)
		if captures == 1 && edits == 1 {
			for _, kind := range []string{"capture", "source-change"} {
				events, err := restarted.Store.PendingEvents(kind)
				if err != nil || len(events) != 0 {
					t.Fatalf("committed %s not acknowledged: %d %v", kind, len(events), err)
				}
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("durable events did not replay after restart")
}

func TestLiveFailedDeleteBlocksReadsAndReplaysAfterRestart(t *testing.T) {
	s := privacyService(t)
	root := s.DataRoot
	turn := captureAndAnswer(t, s, "queued-delete", "durable deletion private canary")
	e, _ := s.MemoryEngine()
	unlock := externalWriteLock(t, s)
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	if err := s.queueSourceChangeContext(ctx, "g", "c", []string{"queued-delete"}, "delete", "", time.Now()); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected timeout: %v", err)
	}
	events, err := s.Store.PendingEvents("source-change")
	if err != nil || len(events) != 1 {
		t.Fatalf("deletion lost: %+v %v", events, err)
	}
	if err := fsutil.VerifyPrivateFile(filepath.Join(filepath.Dir(s.Store.Path()), "pending-events", "source-change", events[0].ID)); err != nil {
		t.Fatal(err)
	}
	if res := s.Execute(context.Background(), action.Request{Action: "turns.get"}); res.OK || res.Error == nil || !res.Error.Retryable {
		t.Fatalf("read exposed pending privacy data: %+v", res)
	}
	if _, err := e.Search(context.Background(), engine.SearchRequest{}); !errors.Is(err, engine.ErrNotReady) {
		t.Fatalf("search accepted pending privacy data: %v", err)
	}
	if _, err := s.ChatTurn(context.Background(), ChatInput{Message: "a question", ChannelID: "c"}); !errors.Is(err, ErrChatBusy) {
		t.Fatalf("chat accepted pending privacy data: %v", err)
	}
	if turns, err := e.Store.RecentChatTurns("g:c", 10); err != nil || len(turns) != 0 {
		t.Fatalf("conversation exposed pending privacy data: %+v %v", turns, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	unlock()
	again, err := Open(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		var content string
		_ = again.Store.Reader().QueryRow(`SELECT content FROM memory_observations WHERE native_message_id='queued-delete'`).Scan(&content)
		if content == "" && !again.Store.HasPendingEvents("source-change") {
			again.DrainRedactions(t.Context())
			if strings.Contains(turnBody(t, again, turn), "durable deletion private canary") {
				t.Fatal("derivative text survived durable deletion")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("delete did not replay")
}

func TestLiveResyncRetriesExternalWriteLock(t *testing.T) {
	s := privacyService(t)
	_, _ = s.MemoryEngine()
	captureAndAnswer(t, s, "resync", "resync canary")
	e, _ := s.MemoryEngine()
	room := engine.RoomKey("g", "c")
	unlock := externalWriteLock(t, s)
	done := make(chan error, 1)
	go func() { done <- e.Resync([]string{room}) }()
	timer := time.NewTimer(6 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		t.Fatalf("resync abandoned its room: %v", err)
	case <-timer.C:
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	msgs, err := e.Store.Messages([]string{room})
	if err != nil || len(msgs) != 1 {
		t.Fatalf("resync lost source: %d %v", len(msgs), err)
	}
	// Confirm the leaf contains the source, rather than accepting a no-op resync.
	var n int
	if err := s.Store.DB.QueryRow(`SELECT COUNT(*) FROM memory_leaves WHERE room=? AND body LIKE '%resync canary%'`, room).Scan(&n); err != nil || n == 0 {
		t.Fatalf("resync did not publish its leaf: %d %v", n, err)
	}
}

func TestLiveResyncQueueSurvivesFailedCallbackAndRestart(t *testing.T) {
	s := privacyService(t)
	root := s.DataRoot
	e, _ := s.MemoryEngine()
	e.OnDirty = func(string) {} // the secondary index:queue write failed or never ran before a crash
	if _, err := e.Capture(engine.LiveMessage{GuildID: "g", ChannelID: "c", MessageID: "pending-sync", Content: "pending resync canary", At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	_ = e.DirtyRooms() // a failed pass took the in-process queue
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	again, err := Open(root, nil, false)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	fresh, err := again.MemoryEngine()
	if err != nil {
		t.Fatal(err)
	}
	room := engine.RoomKey("g", "c")
	if rooms := fresh.DirtyRooms(); !slices.Contains(rooms, room) {
		t.Fatalf("source transaction did not preserve the resync: %v", rooms)
	}
	if err := fresh.Resync([]string{room}); err != nil {
		t.Fatal(err)
	}
	if rooms, err := fresh.Store.PendingSyncRooms(); err != nil || slices.Contains(rooms, room) {
		t.Fatalf("completed revision was not acknowledged: %v %v", rooms, err)
	}
}

func TestNewSummaryWaitsForPendingPrivacyCleanup(t *testing.T) {
	s := privacyService(t)
	if _, err := s.Store.DB.Exec(`INSERT INTO redaction_jobs(payload,created_at) VALUES('{}','now')`); err != nil {
		t.Fatal(err)
	}
	s.redaction.running.Store(true) // a worker is still completing a previous source change
	defer s.redaction.running.Store(false)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	res := s.Execute(ctx, action.Request{Action: "summaries.channel", Input: json.RawMessage(`{"guild_id":"g","channel_id":"c"}`)})
	if res.OK || res.Error == nil || !res.Error.Retryable {
		t.Fatalf("new summary did not wait for older redaction: %+v", res)
	}
	if countRows(t, s, "redaction_jobs") != 1 {
		t.Fatal("waiting summary altered the pending job")
	}
}
