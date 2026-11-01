package application

import (
	"context"
	"database/sql"
	"sync"
	"testing"
	"time"
)

func holdThreadPool(t *testing.T, pool *sql.DB) func() {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var conns []*sql.Conn
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, c := range conns {
				_ = c.Close()
			}
		})
	}
	t.Cleanup(release)
	for i := 0; i < pool.Stats().MaxOpenConnections; i++ {
		c, err := pool.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	return release
}

func awaitThreadDBWait(t *testing.T, pool *sql.DB, before int64) {
	t.Helper()
	deadline := time.After(20 * time.Second)
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for pool.Stats().WaitCount <= before {
		select {
		case <-deadline:
			t.Fatal("thread operation did not wait for held DB pool")
		case <-tick.C:
		}
	}
}

func TestThreadDBWaitDoesNotBlockCachedMessage(t *testing.T) {
	for _, operation := range []string{"archive", "delete", "note", "save", "rooms", "participant", "forget"} {
		t.Run(operation, func(t *testing.T) {
			s, _ := deliveryService(t)
			s.NoteThread("g", "cached", "parent", false)
			s.NoteThread("g", "target", "parent", false)
			_ = s.MemoryAllowed("member")
			pool := s.Store.DB
			if operation == "rooms" {
				pool = s.Store.Reader()
			}
			release := holdThreadPool(t, pool)
			before := pool.Stats().WaitCount
			done, queried := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				release()
				awaitThreadSignal(t, done, "DB waiter cleanup")
				awaitThreadSignal(t, queried, "cached reader cleanup")
			})
			go func() {
				defer close(done)
				switch operation {
				case "archive":
					s.ThreadArchived("target", true)
				case "delete":
					s.ThreadDeleted("target")
				case "note":
					s.NoteThread("g", "new", "parent", false)
				case "save":
					s.saveThread("target")
				case "rooms":
					s.threadRooms("g", "parent")
				case "participant":
					s.NoteThreadParticipant("target", "member")
				case "forget":
					s.forgetThreadOwner("member")
				}
			}()
			awaitThreadDBWait(t, pool, before)
			go func() {
				defer close(queried)
				s.ThreadFor("cached", "member")
				s.threadParent("cached")
				s.ChannelPolicyFor("g", "cached")
			}()
			select {
			case <-queried:
			case <-time.After(time.Second):
				t.Error("DB connection wait held the thread cache mutex")
			}
		})
	}
}

func TestThreadInitialLoadDoesNotHoldCacheLock(t *testing.T) {
	for _, kind := range []string{"reader", "writer"} {
		t.Run(kind, func(t *testing.T) {
			s, _ := deliveryService(t)
			pool := s.Store.Reader()
			if kind == "writer" {
				pool = s.Store.DB
			}
			release := holdThreadPool(t, pool)
			before := pool.Stats().WaitCount
			done, locked := make(chan struct{}), make(chan struct{})
			t.Cleanup(func() {
				release()
				awaitThreadSignal(t, done, "initial load cleanup")
				awaitThreadSignal(t, locked, "initial cache lock cleanup")
			})
			go func() { defer close(done); s.threadsLoaded() }()
			awaitThreadDBWait(t, pool, before)
			go func() { defer close(locked); s.threads.mu.Lock(); s.threads.mu.Unlock() }()
			select {
			case <-locked:
			case <-time.After(time.Second):
				t.Error("initial DB load held thread cache mutex")
			}
		})
	}
}

func TestThreadRecoveryDuringForgetWriterWaitIsCleared(t *testing.T) {
	s, _ := deliveryService(t)
	s.ThreadOpened(context.Background(), "g", "parent", "target", "member", "")
	s.threads.mu.Lock()
	delete(s.threads.threads, "target")
	s.threads.mu.Unlock()
	release := holdThreadPool(t, s.Store.DB)
	before := s.Store.DB.Stats().WaitCount
	done := make(chan struct{})
	t.Cleanup(func() { release(); awaitThreadSignal(t, done, "forget writer cleanup") })
	go func() { defer close(done); s.forgetThreadOwner("member") }()
	awaitThreadDBWait(t, s.Store.DB, before)
	// This read starts after the initial invalidation, while the DB still contains the old owner.
	if !s.recoverThread("target", false) {
		t.Fatal("could not establish the intermediate DB snapshot")
	}
	release()
	awaitThreadSignal(t, done, "completed owner cleanup")
	s.threads.mu.Lock()
	info := s.threads.threads["target"]
	dirty := info != nil && (info.Owner != "" || info.participants["member"] || info.humans["member"])
	retained := len(s.threads.reads)
	s.threads.mu.Unlock()
	if dirty || retained != 0 {
		t.Error("a read during the writer wait survived completed owner cleanup")
	}
}
