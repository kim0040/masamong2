package storage

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"log/slog"
	"reflect"
	"strings"
	"sync/atomic"
	"time"

	"modernc.org/sqlite"
)

// All writes, including older DB.Exec callers, share the writer's single connection and retry policy.
// BEGIN IMMEDIATE obtains the lock before callers read or accumulate transaction results, so retries
// never repeat a transaction callback or its non-SQL side effects.
func init() { sql.Register("masamong-sqlite", busyDriver{}) }

type busyDriver struct{}

func (busyDriver) Open(name string) (driver.Conn, error) {
	c, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return &busyConn{Conn: c}, nil
}

type busyConn struct {
	driver.Conn
	tx *timedTx // the open transaction (database/sql never uses a connection from two goroutines)
}

// noteStatement remembers the first statement of the open transaction, which names it in the log.
func (c *busyConn) noteStatement(query string) {
	if c.tx != nil && c.tx.first == "" {
		c.tx.first = query
	}
}

func IsBusy(err error) bool {
	var coded interface{ Code() int }
	return errors.As(err, &coded) && (coded.Code()&255 == 5 || coded.Code()&255 == 6)
}

func retryBusy(ctx context.Context, fn func() error) error {
	limit := time.Now().Add(30 * time.Second)
	delay := 10 * time.Millisecond
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := fn()
		if !IsBusy(err) || !time.Now().Before(limit) {
			return err
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		delay = min(2*delay, 250*time.Millisecond)
	}
}

func (c *busyConn) BeginTx(ctx context.Context, opts driver.TxOptions) (driver.Tx, error) {
	var tx driver.Tx
	err := retryBusy(ctx, func() (err error) {
		tx, err = c.Conn.(driver.ConnBeginTx).BeginTx(ctx, opts)
		return err
	})
	if err != nil {
		return nil, err
	}
	c.tx = &timedTx{Tx: tx, start: time.Now(), conn: c}
	return c.tx, nil
}

func (c *busyConn) Begin() (driver.Tx, error) {
	return c.BeginTx(context.Background(), driver.TxOptions{})
}

func (c *busyConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.noteStatement(query)
	defer noteHold("statement", query, time.Now())
	var result driver.Result
	err := retryBusy(ctx, func() (err error) {
		result, err = c.Conn.(driver.ExecerContext).ExecContext(ctx, query, args)
		return err
	})
	return result, err
}

func (c *busyConn) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	c.noteStatement(query)
	start := time.Now()
	rows, err := c.Conn.(driver.QueryerContext).QueryContext(ctx, query, args)
	if err != nil {
		noteHold("query", query, start)
		return nil, err
	}
	return &timedRows{Rows: rows, query: query, start: start}, nil
}

func (c *busyConn) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	c.noteStatement(query)
	return c.Conn.(driver.ConnPrepareContext).PrepareContext(ctx, query)
}

// The writer is one connection for the whole process (Open: SetMaxOpenConns(1)), so a statement or a
// transaction that holds it for long makes every capture, answer and command wait. On a 1-core machine
// a single slow console count once stopped the bot for minutes (docs/postmortems/0004). Such holds are
// logged, with the first words of the statement only (values are bound separately and a quoted literal
// is cut off), so a stall shows up in the log instead of only as slow answers. Long reads belong on
// Store.Reader.
var (
	writerSlow     = 2 * time.Second // a hold at least this long is logged
	slowWriterSeen atomic.Int64      // how many holds were that long (SlowWriterHolds)
	slowWriterLog  atomic.Int64      // unix nanos of the last log line: at most one a second
)

// SlowWriterHolds is how many statements or transactions have held the writer connection for at least
// writerSlow since the process started.
func SlowWriterHolds() int64 { return slowWriterSeen.Load() }

func noteHold(kind, query string, start time.Time) {
	d := time.Since(start)
	if d < writerSlow {
		return
	}
	slowWriterSeen.Add(1)
	now := time.Now().UnixNano()
	last := slowWriterLog.Load()
	if now-last < int64(time.Second) || !slowWriterLog.CompareAndSwap(last, now) {
		return
	}
	slog.Warn("storage: a slow statement held the writer connection; everything else waited", "kind", kind,
		"ms", d.Milliseconds(), "sql", sqlHead(query))
}

// sqlHead is the start of a statement for a log line: whitespace folded, cut before any quoted literal
// and after 80 bytes.
func sqlHead(q string) string {
	if i := strings.IndexAny(q, "'\""); i >= 0 {
		q = q[:i]
	}
	q = strings.Join(strings.Fields(q), " ")
	if len(q) > 80 {
		q = q[:80]
	}
	return q
}

// timedTx measures a transaction from BEGIN to COMMIT or ROLLBACK, the time it holds the writer. Its
// log line shows the transaction's first statement, which tells which transaction it was.
type timedTx struct {
	driver.Tx
	start time.Time
	conn  *busyConn
	first string
}

func (t *timedTx) end() {
	if t.conn != nil && t.conn.tx == t {
		t.conn.tx = nil
	}
	noteHold("transaction", t.first, t.start)
}

func (t *timedTx) Commit() error {
	defer t.end()
	return t.Tx.Commit()
}

func (t *timedTx) Rollback() error {
	defer t.end()
	return t.Tx.Rollback()
}

// timedRows measures a query until its rows are closed: SQLite does the work while the rows are read.
// It passes the column type methods of the driver's rows through (ColumnTypes in transfer.go uses them).
type timedRows struct {
	driver.Rows
	query string
	start time.Time
}

func (r *timedRows) Close() error {
	defer noteHold("query", r.query, r.start)
	return r.Rows.Close()
}

func (r *timedRows) ColumnTypeDatabaseTypeName(i int) string {
	if t, ok := r.Rows.(driver.RowsColumnTypeDatabaseTypeName); ok {
		return t.ColumnTypeDatabaseTypeName(i)
	}
	return ""
}

func (r *timedRows) ColumnTypeLength(i int) (int64, bool) {
	if t, ok := r.Rows.(driver.RowsColumnTypeLength); ok {
		return t.ColumnTypeLength(i)
	}
	return 0, false
}

func (r *timedRows) ColumnTypeNullable(i int) (bool, bool) {
	if t, ok := r.Rows.(driver.RowsColumnTypeNullable); ok {
		return t.ColumnTypeNullable(i)
	}
	return false, false
}

func (r *timedRows) ColumnTypePrecisionScale(i int) (int64, int64, bool) {
	if t, ok := r.Rows.(driver.RowsColumnTypePrecisionScale); ok {
		return t.ColumnTypePrecisionScale(i)
	}
	return 0, 0, false
}

func (r *timedRows) ColumnTypeScanType(i int) reflect.Type {
	if t, ok := r.Rows.(driver.RowsColumnTypeScanType); ok {
		return t.ColumnTypeScanType(i)
	}
	return reflect.TypeFor[any]()
}
