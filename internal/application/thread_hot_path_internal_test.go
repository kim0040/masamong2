package application

import (
	"context"
	"database/sql"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCachedThreadMessageLookupsDoNotWaitForDB(t *testing.T) {
	s, _ := deliveryService(t)
	s.NoteThread("g", "target", "parent", false)
	db, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var held []*sql.Conn
	var once sync.Once
	release := func() {
		once.Do(func() {
			for _, conn := range held {
				_ = conn.Close()
			}
		})
	}
	t.Cleanup(release)
	// Reserve every writer and reader connection. Any added SQL on the cached message path would wait
	// until cleanup releases them, so returning now is structural evidence of no extra reads/writes.
	for _, pool := range []*sql.DB{db.DB, db.Reader()} {
		for i := 0; i < pool.Stats().MaxOpenConnections; i++ {
			conn, err := pool.Conn(ctx)
			if err != nil {
				t.Fatal(err)
			}
			held = append(held, conn)
		}
	}
	done := make(chan struct{})
	var ok atomic.Bool
	t.Cleanup(func() { release(); awaitThreadSignal(t, done, "cached message cleanup") })
	go func() {
		defer close(done)
		_, known := s.ThreadFor("target", "member")
		parent := s.threadParent("target")
		s.NoteThread("g", "target", "parent", false)
		s.noteThreadActivity(ObservedMessage{ChannelID: "target", AuthorID: "member", At: s.Clock.Now()})
		_ = s.ChannelPolicyFor("g", "target")
		ok.Store(known && parent == "parent")
	}()
	awaitThreadSignal(t, done, "cached message with all DB connections held")
	if !ok.Load() {
		t.Fatal("cached thread lost its relation while DB connections were busy")
	}
}
