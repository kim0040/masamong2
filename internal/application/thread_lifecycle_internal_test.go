package application

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestThreadLifecyclePersistsAcrossTickEviction(t *testing.T) {
	for _, phase := range []string{"before-db", "before-write"} {
		for _, archived := range []bool{false, true} {
			name := "unarchive"
			if archived {
				name = "archive"
			}
			t.Run(phase+"/"+name, func(t *testing.T) {
				s, clk := deliveryService(t)
				s.NoteThread("g", "target", "parent", false)
				s.threads.mu.Lock()
				info := s.threads.threads["target"]
				info.Open = archived
				info.LastAt = clk.Now().Add(-threadKeepClosed - time.Hour)
				s.threads.mu.Unlock()
				s.saveThread("target")
				if !archived {
					s.threads.mu.Lock()
					delete(s.threads.threads, "target")
					s.threads.mu.Unlock()
					s.ensureThread("target", true)
				}
				h := &threadTestHooks.beforeArchiveDB
				if phase == "before-write" {
					h = &threadTestHooks.beforeArchiveWrite
				}
				var ticked bool
				hook := func(string) {
					// A legacy unarchive refreshed LastAt before saving. Keep the thread evictable at
					// either checkpoint so the test checks the durable write after actual eviction.
					s.threads.mu.Lock()
					if info := s.threads.threads["target"]; info != nil {
						info.LastAt = clk.Now().Add(-threadKeepClosed - time.Hour)
					}
					s.threads.mu.Unlock()
					s.closeIdleThreads(context.Background())
					s.threads.mu.Lock()
					absent := s.threads.threads["target"] == nil
					s.threads.mu.Unlock()
					if !absent {
						t.Fatal("minute tick did not evict the lifecycle event's thread")
					}
					ticked = true
				}
				h.Store(&hook)
				t.Cleanup(func() { h.Store(nil) })
				s.ThreadArchived("target", archived)
				if !ticked {
					t.Fatal("lifecycle event did not reach the tick checkpoint")
				}
				var state, reason, closed string
				if err := s.Store.DB.QueryRow(`SELECT state,close_reason,closed_at FROM channel_threads WHERE thread_id='target'`).Scan(&state, &reason, &closed); err != nil {
					t.Fatal(err)
				}
				if archived && (state != "closed" || reason != "archived" || closed == "") || !archived && (state != "open" || reason != "" || closed != "") {
					t.Error("tick eviction suppressed the durable lifecycle update")
				}
				if !s.recoverThread("target", true) {
					t.Fatal("updated row could not be recovered")
				}
				if view, ok := s.ThreadFor("target", "member"); !ok || view.Open == archived {
					t.Error("cache did not follow the durable lifecycle result")
				}
			})
		}
	}
}

func TestThreadLifecycleInvalidatesAnOlderRead(t *testing.T) {
	s, _ := deliveryService(t)
	s.NoteThread("g", "target", "parent", false)
	s.threads.mu.Lock()
	delete(s.threads.threads, "target")
	s.threads.mu.Unlock()
	parked, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(resume) }) }
	var paused atomic.Bool
	hook := func(string) {
		if paused.CompareAndSwap(false, true) {
			close(parked)
			<-resume
		}
	}
	threadTestHooks.afterRead.Store(&hook)
	t.Cleanup(func() {
		release()
		awaitThreadSignal(t, done, "archive read cleanup")
		threadTestHooks.afterRead.Store(nil)
	})
	go func() {
		defer close(done)
		s.recoverThread("target", true)
	}()
	awaitThreadSignal(t, parked, "open snapshot before archive")
	s.ThreadArchived("target", true)
	release()
	awaitThreadSignal(t, done, "old snapshot after archive")
	threadTestHooks.afterRead.Store(nil)
	s.threads.mu.Lock()
	info := s.threads.threads["target"]
	s.threads.mu.Unlock()
	if info != nil && info.Open {
		t.Error("an older recovery snapshot undid the archive")
	}
	if view, ok := s.ThreadFor("target", "member"); !ok || view.Open {
		t.Error("a fresh lookup did not follow the archived DB row")
	}
}
