package application

import (
	"context"
	"database/sql"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/testutil"
)

// A decision lock holds memory only: a slow store must not stop the decisions of other channels behind it
// (fix plan R03, code standard R20). Each test makes the store wait — the single writer connection held
// by a transaction, or every reader connection taken — starts one call that needs it, and checks that a
// call needing only the same lock still returns while the first one waits.

const lockScopeWait = 5 * time.Second

// takeWriter holds the store's single writer connection (holdWriter).
func takeWriter(t *testing.T, s *Service) (release func()) {
	t.Helper()
	st, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	return holdWriter(t, st)
}

// holdReaders takes every connection of the store's read pool until release is called.
func holdReaders(t *testing.T, s *Service) (release func()) {
	t.Helper()
	st, err := s.adminDB()
	if err != nil {
		t.Fatal(err)
	}
	r := st.Reader()
	n := r.Stats().MaxOpenConnections
	if n <= 0 || r == st.DB {
		t.Fatalf("the read pool is not bounded and separate (max %d)", n)
	}
	var conns []*sql.Conn
	for range n {
		c, err := r.Conn(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		conns = append(conns, c)
	}
	var once sync.Once
	release = func() {
		once.Do(func() {
			for _, c := range conns {
				_ = c.Close()
			}
		})
	}
	t.Cleanup(release)
	return release
}

// waitsInStore returns once a goroutine running fn (a function name as a stack trace prints it, such
// as "(*Service).noteBanter(") is waiting for a connection of the store. The pools' wait counters are no
// signal: the service's own workers wait for the held store too.
func waitsInStore(t *testing.T, fn string) {
	t.Helper()
	buf := make([]byte, 1<<22)
	testutil.WaitFor(t, lockScopeWait, fn+" waiting for the store", func() bool {
		n := runtime.Stack(buf, true)
		for _, g := range strings.Split(string(buf[:n]), "\n\n") {
			if strings.Contains(g, fn) && strings.Contains(g, "database/sql.(*DB).conn(") {
				return true
			}
		}
		return false
	})
}

// returnsWhileWaiting runs fn and reports an error when it has not returned within lockScopeWait. It
// returns a channel closed when fn returns; the caller waits on it only after freeing the store, since a
// call that does wait cannot return before that.
func returnsWhileWaiting(t *testing.T, what string, fn func()) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(lockScopeWait):
		t.Errorf("%s waited for another call's store access", what)
	}
	return done
}
