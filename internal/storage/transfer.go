package storage

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Moving an instance between SQLite and TiDB copies rows, never SQL text: each backend keeps its own DDL
// (schema.go and the per-package DDL next to it), the target is migrated by the normal Open paths first,
// and values are converted by column kind. A copy is only reported as verified when every table's row
// count and order-independent checksum match on both sides.

// Tables lists the product tables in the store (backend internals such as sqlite_sequence excluded).
func (s *Store) Tables() ([]string, error) {
	q := `SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`
	if s.Backend == BackendTiDB {
		q = `SELECT TABLE_NAME FROM information_schema.TABLES WHERE TABLE_SCHEMA = DATABASE() AND TABLE_TYPE = 'BASE TABLE' ORDER BY TABLE_NAME`
	}
	rows, err := s.DB.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// Hold blocks writers that go through WithWriter while fn runs (fn must not call WithWriter).
func (s *Store) Hold(fn func() error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn()
}

type colKind int

const (
	kindText colKind = iota
	kindInt
	kindFloat
	kindBlob
)

type column struct {
	Name string
	Kind colKind
}

func kindOf(dbType string) colKind {
	t := strings.ToUpper(dbType)
	switch {
	case strings.Contains(t, "BLOB") || strings.Contains(t, "BINARY"):
		return kindBlob
	case strings.Contains(t, "INT"):
		return kindInt
	case strings.Contains(t, "REAL") || strings.Contains(t, "DOUBLE") || strings.Contains(t, "FLOAT") || strings.Contains(t, "DECIMAL"):
		return kindFloat
	}
	return kindText
}

// columns returns the declared columns of table in name order.
func (s *Store) columns(table string) ([]column, error) {
	rows, err := s.DB.Query("SELECT * FROM " + quoteIdent(table) + " LIMIT 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	types, err := rows.ColumnTypes()
	if err != nil {
		return nil, err
	}
	out := make([]column, 0, len(types))
	for _, t := range types {
		out = append(out, column{Name: t.Name(), Kind: kindOf(t.DatabaseTypeName())})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func quoteIdent(name string) string { return "`" + strings.ReplaceAll(name, "`", "``") + "`" }

func colList(cols []column) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = quoteIdent(c.Name)
	}
	return strings.Join(names, ", ")
}

// convert turns a driver value into the Go type the column kind stores on either backend.
// The MySQL text protocol returns every scalar as []byte; SQLite returns typed values.
func convert(v any, k colKind) (any, error) {
	if v == nil {
		return nil, nil
	}
	switch k {
	case kindBlob:
		switch x := v.(type) {
		case []byte:
			return append([]byte(nil), x...), nil
		case string:
			return []byte(x), nil
		}
	case kindInt:
		switch x := v.(type) {
		case int64:
			return x, nil
		case float64:
			return int64(x), nil
		case bool:
			if x {
				return int64(1), nil
			}
			return int64(0), nil
		case []byte:
			return strconv.ParseInt(string(x), 10, 64)
		case string:
			return strconv.ParseInt(x, 10, 64)
		}
	case kindFloat:
		switch x := v.(type) {
		case float64:
			return x, nil
		case int64:
			return float64(x), nil
		case []byte:
			return strconv.ParseFloat(string(x), 64)
		case string:
			return strconv.ParseFloat(x, 64)
		}
	default:
		switch x := v.(type) {
		case string:
			return x, nil
		case []byte:
			return string(x), nil
		case int64:
			return strconv.FormatInt(x, 10), nil
		case float64:
			return strconv.FormatFloat(x, 'g', -1, 64), nil
		}
	}
	return nil, fmt.Errorf("unsupported value %T for column kind %d", v, k)
}

func canonical(v any) string {
	switch x := v.(type) {
	case nil:
		return "\x00"
	case []byte:
		return "b:" + hex.EncodeToString(x)
	case int64:
		return "i:" + strconv.FormatInt(x, 10)
	case float64:
		return "f:" + strconv.FormatFloat(x, 'g', -1, 64)
	case string:
		return "s:" + x
	}
	return fmt.Sprintf("?:%v", v)
}

// TableSum is a row count and an order-independent checksum over the given columns.
type TableSum struct {
	Rows int64  `json:"rows"`
	Sum  string `json:"checksum"`
}

// Checksum hashes every row of table over cols (converted by kind), so SQLite and TiDB copies of
// the same data give the same value regardless of scan order or collation.
func (s *Store) Checksum(ctx context.Context, table string, cols []column) (TableSum, error) {
	rows, err := s.DB.QueryContext(ctx, "SELECT "+colList(cols)+" FROM "+quoteIdent(table))
	if err != nil {
		return TableSum{}, err
	}
	defer rows.Close()
	raw := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	var n int64
	var acc uint64
	h := sha256.New()
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return TableSum{}, err
		}
		h.Reset()
		for i, c := range cols {
			v, err := convert(raw[i], c.Kind)
			if err != nil {
				return TableSum{}, fmt.Errorf("%s.%s: %w", table, c.Name, err)
			}
			h.Write([]byte(canonical(v)))
			h.Write([]byte{0x1f})
		}
		acc += binary.BigEndian.Uint64(h.Sum(nil)[:8])
		n++
	}
	if err := rows.Err(); err != nil {
		return TableSum{}, err
	}
	return TableSum{Rows: n, Sum: fmt.Sprintf("%016x", acc)}, nil
}

// Count returns the row count of table.
func (s *Store) Count(ctx context.Context, table string) (int64, error) {
	var n int64
	err := s.DB.QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdent(table)).Scan(&n)
	return n, err
}

// CountRead is Count on the reader connections: a status page counting a million-row table must not
// hold the writer (and so every message being stored) for seconds.
func (s *Store) CountRead(ctx context.Context, table string) (int64, error) {
	var n int64
	err := s.Reader().QueryRowContext(ctx, "SELECT COUNT(*) FROM "+quoteIdent(table)).Scan(&n)
	return n, err
}

// TableCopy reports one table of a transfer.
type TableCopy struct {
	Table    string   `json:"table"`
	Rows     int64    `json:"rows"`
	Copied   int64    `json:"copied"`
	Source   TableSum `json:"source"`
	Target   TableSum `json:"target"`
	Verified bool     `json:"verified"`
}

// TransferPlan is what a copy would do, computed without writing.
type TransferPlan struct {
	Tables []TableCopy `json:"tables"`
	// Missing lists source tables the target schema does not define (the copy refuses them).
	Missing []string `json:"missing_tables,omitempty"`
	// MissingColumns lists source columns absent from the target table (table.column).
	MissingColumns []string `json:"missing_columns,omitempty"`
	// NotEmpty lists target tables that already hold rows (the copy refuses a non-empty target).
	NotEmpty []string `json:"target_not_empty,omitempty"`
	Rows     int64    `json:"rows"`
}

// Ready reports whether Transfer may run.
func (p TransferPlan) Ready() bool {
	return len(p.Missing) == 0 && len(p.MissingColumns) == 0 && len(p.NotEmpty) == 0
}

// bootstrapTables are written by Open on an empty database; a copy replaces them.
var bootstrapTables = map[string]bool{"schema_migrations": true}

// PlanTransfer compares src and dst without writing.
func PlanTransfer(ctx context.Context, src, dst *Store) (TransferPlan, error) {
	var plan TransferPlan
	srcTables, err := src.Tables()
	if err != nil {
		return plan, err
	}
	dstTables, err := dst.Tables()
	if err != nil {
		return plan, err
	}
	have := map[string]bool{}
	for _, t := range dstTables {
		have[t] = true
	}
	for _, t := range srcTables {
		if !have[t] {
			plan.Missing = append(plan.Missing, t)
			continue
		}
		scols, err := src.columns(t)
		if err != nil {
			return plan, fmt.Errorf("%s: %w", t, err)
		}
		dcols, err := dst.columns(t)
		if err != nil {
			return plan, fmt.Errorf("%s: %w", t, err)
		}
		dset := map[string]bool{}
		for _, c := range dcols {
			dset[c.Name] = true
		}
		for _, c := range scols {
			if !dset[c.Name] {
				plan.MissingColumns = append(plan.MissingColumns, t+"."+c.Name)
			}
		}
		n, err := src.Count(ctx, t)
		if err != nil {
			return plan, fmt.Errorf("%s: %w", t, err)
		}
		if !bootstrapTables[t] {
			m, err := dst.Count(ctx, t)
			if err != nil {
				return plan, fmt.Errorf("%s: %w", t, err)
			}
			if m > 0 {
				plan.NotEmpty = append(plan.NotEmpty, t)
			}
		}
		plan.Tables = append(plan.Tables, TableCopy{Table: t, Rows: n})
		plan.Rows += n
	}
	return plan, nil
}

// maxParams keeps multi-row INSERTs under both engines' placeholder limits (SQLite 32766, MySQL 65535).
const maxParams = 8000

// Transfer copies every planned table from src into dst and verifies it. It holds src's writer lock
// for the whole copy so in-process writes wait; verification catches anything written around it.
// progress (optional) is called after each batch.
func Transfer(ctx context.Context, src, dst *Store, progress func(table string, done, total int64)) (TransferPlan, error) {
	var plan TransferPlan
	err := src.Hold(func() error {
		var err error
		plan, err = PlanTransfer(ctx, src, dst)
		if err != nil {
			return err
		}
		if !plan.Ready() {
			return fmt.Errorf("target not ready: missing tables %v, missing columns %v, non-empty %v", plan.Missing, plan.MissingColumns, plan.NotEmpty)
		}
		for i := range plan.Tables {
			tc := &plan.Tables[i]
			cols, err := src.columns(tc.Table)
			if err != nil {
				return err
			}
			if bootstrapTables[tc.Table] {
				if _, err := dst.DB.ExecContext(ctx, "DELETE FROM "+quoteIdent(tc.Table)); err != nil {
					return fmt.Errorf("%s: %w", tc.Table, err)
				}
			}
			n, err := copyTable(ctx, src, dst, tc.Table, cols, tc.Rows, progress)
			tc.Copied = n
			if err != nil {
				return fmt.Errorf("%s: %w", tc.Table, err)
			}
			if tc.Source, err = src.Checksum(ctx, tc.Table, cols); err != nil {
				return err
			}
			if tc.Target, err = dst.Checksum(ctx, tc.Table, cols); err != nil {
				return err
			}
			tc.Verified = tc.Source == tc.Target
		}
		return nil
	})
	return plan, err
}

// Verified reports whether every table matched after Transfer.
func (p TransferPlan) Verified() bool {
	if !p.Ready() {
		return false
	}
	for _, t := range p.Tables {
		if !t.Verified {
			return false
		}
	}
	return true
}

func copyTable(ctx context.Context, src, dst *Store, table string, cols []column, total int64, progress func(string, int64, int64)) (int64, error) {
	rows, err := src.DB.QueryContext(ctx, "SELECT "+colList(cols)+" FROM "+quoteIdent(table))
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	perRow := len(cols)
	batch := max(1, min(500, maxParams/max(1, perRow)))
	one := "(" + strings.TrimSuffix(strings.Repeat("?,", perRow), ",") + ")"
	prefix := "INSERT INTO " + quoteIdent(table) + " (" + colList(cols) + ") VALUES "
	raw := make([]any, perRow)
	ptrs := make([]any, perRow)
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	args := make([]any, 0, batch*perRow)
	pending := 0
	var done int64
	flush := func() error {
		if pending == 0 {
			return nil
		}
		q := prefix + strings.TrimSuffix(strings.Repeat(one+",", pending), ",")
		tx, err := dst.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, q, args...); err != nil {
			_ = tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		done += int64(pending)
		args = args[:0]
		pending = 0
		if progress != nil {
			progress(table, done, total)
		}
		return nil
	}
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return done, err
		}
		for i, c := range cols {
			v, err := convert(raw[i], c.Kind)
			if err != nil {
				return done, fmt.Errorf("%s: %w", c.Name, err)
			}
			args = append(args, v)
		}
		pending++
		if pending == batch {
			if err := flush(); err != nil {
				return done, err
			}
		}
	}
	if err := rows.Err(); err != nil {
		return done, err
	}
	return done, flush()
}
