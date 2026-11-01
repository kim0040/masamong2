package application_test

import (
	"context"
	"testing"
)

// Restart the real service/store: an in-process negative cache is no authority for deletion.
func TestDeletedThreadStaysExcludedAfterRestart(t *testing.T) {
	svc := setupSvc(t)
	svc.ThreadOpened(context.Background(), "g", "parent", "deleted", "owner", "")
	svc.NoteThreadParticipant("deleted", "member")
	svc.NoteThread("g", "live", "parent", false)
	svc.ThreadDeleted("deleted")
	svc = restartServiceForTest(t, svc)

	check := func() {
		t.Helper()
		if _, ok := svc.ThreadFor("deleted", "member"); ok {
			t.Error("deleted thread and its participants returned after restart")
		}
		if rooms := svc.ChannelRoomsForTest("g", "parent"); containsAll(rooms, "discord:g:deleted") || !containsAll(rooms, "discord:g:live") {
			t.Error("parent search scope did not exclude only the deleted thread")
		}
		if rooms := svc.ChannelRoomsForTest("g", "deleted"); len(rooms) != 0 {
			t.Error("deleted thread still has a search scope")
		}
	}
	check()
	// Both discovery and lifecycle callbacks can arrive late, including a bot-open callback.
	svc.NoteThread("g", "deleted", "parent", true)
	svc.ThreadArchived("deleted", false)
	if seed := svc.ThreadOpened(context.Background(), "g", "parent", "deleted", "owner", ""); seed != "" {
		t.Error("a late bot-open callback reopened a deleted thread")
	}
	check()
	svc = restartServiceForTest(t, svc)
	check()
	var reason string
	if err := svc.Store.DB.QueryRow(`SELECT close_reason FROM channel_threads WHERE thread_id='deleted'`).Scan(&reason); err != nil || reason != "deleted" {
		t.Error("late callbacks changed the durable deletion marker")
	}
}

func TestThreadDeletionOutlivesRelationRetention(t *testing.T) {
	svc := setupSvc(t)
	// A delete may arrive before discovery; it still records a durable tombstone.
	svc.ThreadDeleted("unseen")
	svc.NoteThread("g", "deleted", "parent", false)
	svc.ThreadDeleted("deleted")
	if _, err := svc.Store.DB.Exec(`DELETE FROM channel_threads WHERE thread_id='deleted'`); err != nil {
		t.Fatal(err)
	}
	svc = restartServiceForTest(t, svc)
	for _, id := range []string{"unseen", "deleted"} {
		svc.NoteThread("g", id, "parent", false)
		if _, ok := svc.ThreadFor(id, "member"); ok {
			t.Error("discovery recreated a durably deleted relation")
		}
		if len(svc.ChannelRoomsForTest("g", id)) != 0 {
			t.Error("a durably deleted thread has a search scope")
		}
	}
}
