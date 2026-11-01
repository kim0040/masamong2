package application

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeletedThreadExcludedFromStoredLookups(t *testing.T) {
	s, clk := deliveryService(t)
	s.NoteThread("g", "deleted", "parent", false)
	s.NoteThreadParticipant("deleted", "member")
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	// Leave participants behind deliberately, as older services did on deletion.
	if _, err := db.DB.Exec(`UPDATE channel_threads SET state='closed',close_reason='deleted' WHERE thread_id='deleted'`); err != nil {
		t.Fatal(err)
	}
	if parent := storedThreadParent(db, "g", "deleted"); parent != "" {
		t.Error("stored parent lookup returned a deleted relation")
	}
	if _, ok := s.storedThread("deleted", clk.Now()); ok {
		t.Error("stored thread lookup returned a deleted thread")
	}
	info := &threadInfo{humans: map[string]bool{}, participants: map[string]bool{}}
	s.loadThreadParticipants(db, map[string]*threadInfo{"deleted": info}, "")
	if len(info.participants) != 0 || len(info.humans) != 0 {
		t.Error("participant lookup included a deleted thread")
	}
}

func awaitThreadSignal(t *testing.T, ch <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(20 * time.Second):
		t.Fatal("timed out waiting for " + what)
	}
}

// Pause after both reads, then finish the privacy change before allowing publication. This is a
// logical race even when every map access has its mutex and the race detector reports no data race.
func TestThreadReadCannotPublishAfterPrivacyChange(t *testing.T) {
	for _, source := range []string{"recover", "note"} {
		for _, change := range []string{"delete", "withdraw"} {
			t.Run(source+"/"+change, func(t *testing.T) {
				s, _ := deliveryService(t)
				s.ThreadOpened(context.Background(), "g", "parent", "target", "member", "")
				s.NoteThreadParticipant("target", "member")
				s.threads.mu.Lock()
				delete(s.threads.threads, "target")
				s.threads.mu.Unlock()
				parked, resume, done := make(chan struct{}), make(chan struct{}), make(chan struct{})
				var once sync.Once
				release := func() { once.Do(func() { close(resume) }) }
				var paused atomic.Bool
				hook := func(id string) {
					if id == "target" && paused.CompareAndSwap(false, true) {
						close(parked)
						<-resume
					}
				}
				threadTestHooks.afterRead.Store(&hook)
				// This cleanup was registered after deliveryService: release and join before it closes DBs.
				t.Cleanup(func() {
					release()
					awaitThreadSignal(t, done, "thread read cleanup")
					threadTestHooks.afterRead.Store(nil)
				})
				go func() {
					defer close(done)
					if source == "recover" {
						s.recoverThread("target", true)
					} else {
						s.NoteThread("g", "target", "parent", true)
					}
				}()
				awaitThreadSignal(t, parked, "read before publication")
				if change == "delete" {
					s.ThreadDeleted("target")
				} else {
					s.forgetThreadOwner("member")
				}
				release()
				awaitThreadSignal(t, done, "publication attempt")
				s.threads.mu.Lock()
				info := s.threads.threads["target"]
				pending := len(s.threads.reads)
				s.threads.mu.Unlock()
				if info != nil && (change == "delete" || info.Owner != "" || info.participants["member"] || info.humans["member"]) {
					t.Error("a snapshot predating the privacy change was published")
				}
				if pending != 0 {
					t.Error("finished reads retained generation entries")
				}
				view, known := s.ThreadFor("target", "member")
				if change == "delete" {
					if known || len(s.channelRooms("g", "target")) != 0 {
						t.Error("a deleted relation was republished")
					}
					for _, room := range s.channelRooms("g", "parent") {
						if room == "discord:g:target" {
							t.Error("parent search recovered a deleted relation")
						}
					}
				} else if !known || view.Participant {
					t.Error("a fresh recovery did not respect consent withdrawal")
				}
			})
		}
	}
}
