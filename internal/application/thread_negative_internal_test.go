package application

import (
	"context"
	"fmt"
	"testing"
	"time"
)

func evictThreadByCap(t *testing.T, s *Service, id string) {
	t.Helper()
	now := s.Clock.Now()
	s.threads.mu.Lock()
	defer s.threads.mu.Unlock()
	if info := s.threads.threads[id]; info != nil {
		info.LastAt = now.Add(-time.Hour)
	}
	for i := 0; i < threadMaxKnown; i++ {
		s.threads.threads[fmt.Sprintf("filler-%d", i)] = &threadInfo{LastAt: now, Open: true}
	}
	s.threads.trimKnown(now)
	if s.threads.threads[id] != nil {
		t.Fatal("cap did not evict the target thread")
	}
}

func TestThreadSuccessClearsNegativeCache(t *testing.T) {
	for _, source := range []string{"recovery", "registration", "opened", "reactivation"} {
		t.Run(source, func(t *testing.T) {
			s, clk := deliveryService(t)
			if source == "recovery" || source == "reactivation" {
				s.NoteThread("g", "target", "parent", false)
				s.threads.mu.Lock()
				info := s.threads.threads["target"]
				info.Open = false
				info.LastAt = clk.Now().Add(-threadKeepClosed - time.Hour)
				s.threads.mu.Unlock()
				s.saveThread("target")
				s.threads.mu.Lock()
				delete(s.threads.threads, "target")
				s.threads.mu.Unlock()
			}
			if s.recoverThread("target", false) {
				t.Fatal("expected a miss before recovery or registration")
			}
			if _, _, ok := s.threads.notThreadCache().get("target", clk.Now()); !ok {
				t.Fatal("test did not establish a negative cache entry")
			}
			switch source {
			case "recovery":
				if !s.recoverThread("target", true) {
					t.Fatal("any-age recovery failed")
				}
			case "registration":
				s.NoteThread("g", "target", "parent", false)
			case "opened":
				s.ThreadOpened(context.Background(), "g", "parent", "target", "member", "")
			case "reactivation":
				s.ThreadArchived("target", false)
			}
			if _, _, ok := s.threads.notThreadCache().get("target", clk.Now()); ok {
				t.Error("successful recovery/registration/reactivation retained the previous miss")
			}
			// The row is open when it leaves memory again. Recovery alone also clears its previous miss.
			if source == "recovery" {
				s.threads.mu.Lock()
				info := s.threads.threads["target"]
				info.Open, info.LastAt = true, clk.Now()
				s.threads.mu.Unlock()
				s.saveThread("target")
			}
			if source == "opened" {
				// Open bot threads are deliberately exempt from the cap; simulate a gateway human thread.
				s.threads.mu.Lock()
				s.threads.threads["target"].BotOwned = false
				s.threads.mu.Unlock()
			}
			evictThreadByCap(t, s, "target")
			if parent := s.threadParent("target"); parent != "parent" {
				t.Error("a stale negative cache hid the active thread after cap eviction")
			}
		})
	}
}

func TestThreadDeletionIsNotANegativeCacheEntry(t *testing.T) {
	s, clk := deliveryService(t)
	if s.recoverThread("target", false) {
		t.Fatal("expected initial miss")
	}
	s.ThreadDeleted("target")
	if _, ok := s.ThreadFor("target", "member"); ok {
		t.Fatal("deleted thread returned")
	}
	if _, _, ok := s.threads.notThreadCache().get("target", clk.Now()); ok {
		t.Fatal("deletion authority was mixed into the no-row cache")
	}
}
