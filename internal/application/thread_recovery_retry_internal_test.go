package application

import (
	"sync/atomic"
	"testing"
)

func TestThreadRecoveryRetriesInvalidatedSnapshot(t *testing.T) {
	s, _ := deliveryService(t)
	s.NoteThread("g", "target", "parent", false)
	s.threads.mu.Lock()
	delete(s.threads.threads, "target")
	s.threads.mu.Unlock()
	var reads atomic.Int32
	hook := func(string) {
		if reads.Add(1) == 1 {
			s.forgetThreadOwner("unrelated")
		}
	}
	threadTestHooks.afterRead.Store(&hook)
	t.Cleanup(func() { threadTestHooks.afterRead.Store(nil) })
	if !s.recoverThread("target", false) {
		t.Error("unrelated withdrawal made this lookup miss")
	}
	if reads.Load() != 2 {
		t.Error("recovery did not retry exactly once")
	}
}

func TestThreadRecoveryRetryIsBounded(t *testing.T) {
	s, _ := deliveryService(t)
	s.NoteThread("g", "target", "parent", false)
	s.threads.mu.Lock()
	delete(s.threads.threads, "target")
	s.threads.mu.Unlock()
	var reads atomic.Int32
	hook := func(string) { reads.Add(1); s.forgetThreadOwner("unrelated") }
	threadTestHooks.afterRead.Store(&hook)
	t.Cleanup(func() { threadTestHooks.afterRead.Store(nil) })
	if s.recoverThread("target", false) {
		t.Error("repeatedly invalidated snapshots were published")
	}
	s.threads.mu.Lock()
	retained := len(s.threads.reads)
	s.threads.mu.Unlock()
	if reads.Load() != 2 || retained != 0 {
		t.Error("recovery did not stop and release generations after two reads")
	}
}
