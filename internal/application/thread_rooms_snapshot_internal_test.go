package application

import (
	"fmt"
	"sync/atomic"
	"testing"

	"masamong/internal/memory/engine"
)

func TestThreadRoomsRechecksDeletedSnapshot(t *testing.T) {
	s, _ := deliveryService(t)
	s.NoteThread("g", "target", "parent", false)
	var reads atomic.Int32
	hook := func(string) {
		if reads.Add(1) == 1 {
			s.ThreadDeleted("target")
		}
	}
	threadTestHooks.afterRoomsRead.Store(&hook)
	t.Cleanup(func() { threadTestHooks.afterRoomsRead.Store(nil) })
	rooms := s.threadRooms("g", "parent")
	if len(rooms) != 0 || reads.Load() != 2 {
		t.Error("parent scope published a snapshot from before deletion")
	}
}

// Another thread's lifecycle changes on every read must not empty the parent's scope: the in-memory
// table still answers, and still without a thread deleted meanwhile.
func TestThreadRoomsUnderConstantOtherChanges(t *testing.T) {
	s, _ := deliveryService(t)
	s.NoteThread("g", "kept", "parent", false)
	s.NoteThread("g", "gone", "parent", false)
	var reads atomic.Int32
	hook := func(string) {
		n := reads.Add(1)
		s.NoteThread("g", fmt.Sprintf("other-%d", n), "elsewhere", false)
		if n == 1 {
			s.ThreadDeleted("gone")
		}
	}
	threadTestHooks.afterRoomsRead.Store(&hook)
	t.Cleanup(func() { threadTestHooks.afterRoomsRead.Store(nil) })
	rooms := s.threadRooms("g", "parent")
	if len(rooms) != 1 || rooms[0] != engine.RoomKey("g", "kept") {
		t.Fatalf("rooms = %v, want only the kept thread", rooms)
	}
}
