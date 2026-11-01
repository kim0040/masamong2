package application

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"testing"
)

// cancelAtCountContext cancels itself once the watched count falls to target. database/sql calls Done
// while it holds a pool's lock, so the count must be read through a pool of its own: a query on the
// store's pools from inside Done would wait on the lock its caller holds.
type cancelAtCountContext struct {
	context.Context
	reader  *sql.DB
	query   string
	target  int
	cancel  context.CancelFunc
	cancel1 sync.Once
}

func (c *cancelAtCountContext) Done() <-chan struct{} {
	var count int
	if err := c.reader.QueryRow(c.query).Scan(&count); err == nil && count <= c.target {
		c.cancel1.Do(c.cancel)
	}
	return c.Context.Done()
}

func TestRetentionDeleteAndUpdateStopBetweenBatches(t *testing.T) {
	s, _ := deliveryService(t)
	tx, err := s.Store.DB.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2*pruneBatch+1; i++ {
		if _, err := tx.Exec(`INSERT INTO reminder_context(reminder_id,seq,text,at) VALUES('orphan',?,'line','')`, i); err != nil {
			t.Fatal(err)
		}
		if _, err := tx.Exec(`INSERT INTO traces(id,body,created_at) VALUES(?, '{"turn_id":"missing"}', 'old')`, fmt.Sprintf("trace-%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	watch, err := sql.Open("sqlite", s.Store.Path())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = watch.Close() })
	makeCtx := func(query string) (*cancelAtCountContext, context.CancelFunc) {
		base, cancel := context.WithCancel(context.Background())
		return &cancelAtCountContext{Context: base, reader: watch, query: query, target: pruneBatch + 1, cancel: cancel}, cancel
	}
	ctx, cancel := makeCtx(`SELECT COUNT(*) FROM reminder_context`)
	_, err = s.runRetention(ctx)
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("bounded reminder cleanup returned %v", err)
	}
	var remaining int
	if err := s.Store.Reader().QueryRow(`SELECT COUNT(*) FROM reminder_context`).Scan(&remaining); err != nil || remaining != pruneBatch+1 {
		t.Fatalf("delete removed beyond one batch: %d rows remain, %v", remaining, err)
	}

	ctx, cancel = makeCtx(`SELECT COUNT(*) FROM traces WHERE body<>'{}'`)
	_, err = s.runRetention(ctx)
	cancel()
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("bounded trace cleanup returned %v", err)
	}
	if err := s.Store.Reader().QueryRow(`SELECT COUNT(*) FROM traces WHERE body<>'{}'`).Scan(&remaining); err != nil || remaining != pruneBatch+1 {
		t.Fatalf("update crossed the batch boundary: %d rows remain, %v", remaining, err)
	}
}
