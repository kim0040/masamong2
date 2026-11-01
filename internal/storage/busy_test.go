package storage

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestWriterRetriesPastBusyTimeoutWithoutRepeatingCallback(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "busy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	external, err := sql.Open("sqlite", st.Path()+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer external.Close()
	lock, err := external.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	done := make(chan error, 1)
	calls := 0
	go func() {
		done <- st.WithWriter(func(tx *sql.Tx) error {
			calls++
			_, err := tx.Exec(`INSERT INTO consents(subject,granted,updated_at) VALUES('busy',1,'now')`)
			return err
		})
	}()
	// The audited busy timeout was 5 s. A lock held longer must not lose a writer's operation.
	timer := time.NewTimer(6 * time.Second)
	defer timer.Stop()
	select {
	case err := <-done:
		t.Fatalf("write gave up before lock release: %v", err)
	case <-timer.C:
	}
	if err := lock.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil || calls != 1 {
		t.Fatalf("writer err=%v callback calls=%d", err, calls)
	}
	var count int
	if err := st.DB.QueryRow(`SELECT COUNT(*) FROM consents WHERE subject='busy'`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("write missing: %d %v", count, err)
	}
}

func TestBusyDirectWriteAndWriterRespectContext(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "busy.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	external, err := sql.Open("sqlite", st.Path()+"?_txlock=immediate")
	if err != nil {
		t.Fatal(err)
	}
	defer external.Close()
	lock, err := external.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Rollback()
	for _, write := range []func(context.Context) error{
		func(ctx context.Context) error {
			return st.WithWriterContext(ctx, func(*sql.Tx) error { t.Error("callback ran without writer lock"); return nil })
		},
		func(ctx context.Context) error {
			_, err := st.DB.ExecContext(ctx, `INSERT INTO consents(subject,granted,updated_at) VALUES('direct',1,'now')`)
			return err
		},
	} {
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		started := time.Now()
		err := write(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) || time.Since(started) > time.Second {
			t.Fatalf("unbounded context wait: %v %s", err, time.Since(started))
		}
	}
}

// TestSlowWriterHoldsAreLogged: a statement or transaction that holds the single writer connection past
// writerSlow is counted and logged with the start of its SQL only (no quoted literal), and the timing
// wrapper keeps the driver's column types reachable.
func TestSlowWriterHoldsAreLogged(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "slow.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var buf bytes.Buffer
	prevLog, prevSlow := slog.Default(), writerSlow
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	defer func() { slog.SetDefault(prevLog); writerSlow = prevSlow }()

	writerSlow = time.Hour // nothing is slow: no line, no count
	before := SlowWriterHolds()
	if _, err := st.DB.Exec(`INSERT INTO consents(subject,granted,updated_at) VALUES('fast',1,'now')`); err != nil {
		t.Fatal(err)
	}
	if SlowWriterHolds() != before || buf.Len() != 0 {
		t.Fatalf("a fast statement was reported: %q", buf.String())
	}

	writerSlow = 0 // everything is slow
	slowWriterLog.Store(0)
	rows, err := st.DB.Query(`SELECT subject, granted FROM consents WHERE subject<>'private words'`)
	if err != nil {
		t.Fatal(err)
	}
	types, err := rows.ColumnTypes()
	if err != nil || len(types) != 2 || types[0].DatabaseTypeName() == "" {
		t.Fatalf("column types through the timing wrapper: %v %v", types, err)
	}
	for rows.Next() {
	}
	rows.Close()
	out := buf.String()
	if !strings.Contains(out, "kind=query") || !strings.Contains(out, "SELECT subject, granted FROM consents WHERE subject<>") {
		t.Fatalf("slow query line: %q", out)
	}
	if strings.Contains(out, "private") {
		t.Fatalf("a quoted literal reached the log: %q", out)
	}
	if SlowWriterHolds() <= before {
		t.Fatal("slow hold not counted")
	}

	buf.Reset()
	slowWriterLog.Store(0)
	if err := st.WithWriter(func(tx *sql.Tx) error {
		_, err := tx.Exec(`UPDATE consents SET granted=0 WHERE subject='fast'`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "kind=") {
		t.Fatalf("slow transaction line: %q", buf.String())
	}

	// a transaction's line names it by its first statement, quoted values cut off
	buf.Reset()
	writerSlow = 50 * time.Millisecond // the statements are fast, the transaction is not
	slowWriterLog.Store(0)
	if err := st.WithWriter(func(tx *sql.Tx) error {
		if _, err := tx.Exec(`DELETE FROM consents WHERE subject='private words'`); err != nil {
			return err
		}
		time.Sleep(80 * time.Millisecond)
		_, err := tx.Exec(`UPDATE consents SET granted=1`)
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if out := buf.String(); !strings.Contains(out, `kind=transaction`) || !strings.Contains(out, `sql="DELETE FROM consents WHERE subject="`) || strings.Contains(out, "private") {
		t.Fatalf("transaction named by its first statement: %q", out)
	}
}
