package application

import (
	"context"
	"testing"
)

func TestThreadParticipantWriteCannotFollowWithdrawal(t *testing.T) {
	for _, change := range []string{"consent", "forget", "both"} {
		t.Run(change, func(t *testing.T) {
			s, _ := deliveryService(t)
			s.NoteThread("g", "target", "parent", false)
			hook := func(string) {
				if change != "forget" {
					if _, err := s.setConsent("member", "memory", "withdrawn", "console"); err != nil {
						t.Fatal(err)
					}
				}
				if change != "consent" {
					s.forgetThreadOwner("member")
				}
			}
			threadTestHooks.beforeParticipantWrite.Store(&hook)
			t.Cleanup(func() { threadTestHooks.beforeParticipantWrite.Store(nil) })
			s.NoteThreadParticipant("target", "member")
			var n int
			if err := s.Store.DB.QueryRow(`SELECT count(*) FROM thread_participants WHERE user_id='member'`).Scan(&n); err != nil {
				t.Fatal(err)
			}
			if n != 0 {
				t.Error("late participant insertion undid completed withdrawal")
			}
		})
	}
}

func TestThreadOpenedDoesNotPersistWithdrawnOwner(t *testing.T) {
	s, _ := deliveryService(t)
	if _, err := s.setConsent("member", "memory", "withdrawn", "console"); err != nil {
		t.Fatal(err)
	}
	s.ThreadOpened(context.Background(), "g", "parent", "target", "member", "")
	var owner string
	if err := s.Store.DB.QueryRow(`SELECT owner_id FROM channel_threads WHERE thread_id='target'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "" {
		t.Error("stale offer persisted a withdrawn owner")
	}
}

func TestThreadSaveDoesNotPersistWithdrawnOwner(t *testing.T) {
	s, _ := deliveryService(t)
	s.ThreadOpened(context.Background(), "g", "parent", "target", "member", "")
	if _, err := s.setConsent("member", "memory", "withdrawn", "console"); err != nil {
		t.Fatal(err)
	}
	// Consent commits before the asynchronous cleanup. Saving that old owner must still fail closed.
	s.saveThread("target")
	var owner string
	if err := s.Store.DB.QueryRow(`SELECT owner_id FROM channel_threads WHERE thread_id='target'`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	if owner != "" {
		t.Error("saving a stale snapshot restored a withdrawn owner")
	}
}
