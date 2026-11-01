package application

import (
	"context"
	"testing"
)

func TestThreadClosePersistsAfterTickEviction(t *testing.T) {
	s, _ := deliveryService(t)
	s.ThreadOpened(context.Background(), "g", "parent", "target", "member", "")
	// activity while open lives only in memory; the row still says it was last active long ago
	if _, err := s.Store.DB.Exec(`UPDATE channel_threads SET last_at='2020-01-01T00:00:00Z' WHERE thread_id='target'`); err != nil {
		t.Fatal(err)
	}
	hook := func(string) { evictThreadByCap(t, s, "target") }
	threadTestHooks.beforeCloseWrite.Store(&hook)
	t.Cleanup(func() { threadTestHooks.beforeCloseWrite.Store(nil) })
	if err := s.CloseThread(context.Background(), "target", "stop"); err != nil {
		t.Fatal(err)
	}
	var state, owner, closed, reason, last string
	if err := s.Store.DB.QueryRow(`SELECT state,owner_id,closed_at,close_reason,last_at FROM channel_threads WHERE thread_id='target'`).Scan(&state, &owner, &closed, &reason, &last); err != nil {
		t.Fatal(err)
	}
	if state != "closed" || owner != "" || closed == "" || reason != "stop" {
		t.Error("tick eviction lost the durable close")
	}
	if last <= "2020-01-01T00:00:00Z" {
		t.Error("the close did not save the thread's last activity")
	}
}
