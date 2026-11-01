package storage

import (
	"bufio"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"sort"
	"strings"
)

// ExportLogical copies every product table as JSON. It is the TiDB backup stand-in for SQLite
// VACUUM INTO, so it covers the same tables the SQLite file does (memory, chat turns and tool
// usage included); the dump therefore holds conversation text and is written 0600 like the DB.
// BLOB values (embedding vectors) are written as {"b64": "..."} so they survive JSON unchanged.
func (s *Store) ExportLogical() ([]byte, error) {
	var buf bytes.Buffer
	if err := s.ExportLogicalTo(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ExportLogicalTo streams the dump row by row: memory stays flat however large the database is (the
// vectors alone are hundreds of MB for a long history, several times that as JSON maps).
func (s *Store) ExportLogicalTo(out io.Writer) error {
	w := bufio.NewWriterSize(out, 1<<20)
	names, err := s.Tables()
	if err != nil {
		return err
	}
	sort.Strings(names)
	backend, _ := json.Marshal(s.Backend)
	fmt.Fprintf(w, "{\n  \"backend\": %s,\n  \"format\": 2,\n  \"tables\": {", backend)
	for ti, name := range names {
		cols, err := s.columns(name)
		if err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		key, _ := json.Marshal(name)
		if ti > 0 {
			w.WriteString(",")
		}
		fmt.Fprintf(w, "\n    %s: [", key)
		if err := s.exportTable(w, name, cols); err != nil {
			return err
		}
		w.WriteString("]")
	}
	w.WriteString("\n  }\n}\n")
	return w.Flush()
}

func (s *Store) exportTable(w *bufio.Writer, name string, cols []column) error {
	rows, err := s.Reader().Query("SELECT " + colList(cols) + " FROM " + quoteIdent(name))
	if err != nil {
		return fmt.Errorf("%s: %w", name, err)
	}
	defer rows.Close()
	raw := make([]any, len(cols))
	ptrs := make([]any, len(cols))
	for i := range raw {
		ptrs[i] = &raw[i]
	}
	first := true
	for rows.Next() {
		if err := rows.Scan(ptrs...); err != nil {
			return err
		}
		m := make(map[string]any, len(cols))
		for i, c := range cols {
			v, err := convert(raw[i], c.Kind)
			if err != nil {
				return fmt.Errorf("%s.%s: %w", name, c.Name, err)
			}
			if b, ok := v.([]byte); ok {
				v = map[string]string{"b64": base64.StdEncoding.EncodeToString(b)}
			}
			m[c.Name] = v
		}
		line, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if !first {
			w.WriteString(",")
		}
		first = false
		w.WriteString("\n      ")
		w.Write(line)
	}
	return rows.Err()
}

// Clear deletes every row of the product tables. Restore uses it before loading a backup into an
// existing database; the schema stays.
func (s *Store) Clear(ctx context.Context) error {
	names, err := s.Tables()
	if err != nil {
		return err
	}
	return s.Hold(func() error {
		for _, t := range names {
			if _, err := s.DB.ExecContext(ctx, "DELETE FROM "+quoteIdent(t)); err != nil {
				return fmt.Errorf("%s: %w", t, err)
			}
		}
		return nil
	})
}

// ImportLogical loads an ExportLogical dump into this (cleared) store. Tables or columns the store
// does not define are refused, like Transfer. It returns the number of rows written.
func (s *Store) ImportLogical(ctx context.Context, raw []byte) (int64, error) {
	return s.ImportLogicalFrom(ctx, bytes.NewReader(raw))
}

// ImportLogicalFrom reads the dump as a stream, inserting rows in batches as they are decoded, so a
// restore needs no more memory than one batch.
func (s *Store) ImportLogicalFrom(ctx context.Context, r io.Reader) (int64, error) {
	dec := json.NewDecoder(bufio.NewReaderSize(r, 1<<20))
	dec.UseNumber()
	have, err := s.Tables()
	if err != nil {
		return 0, err
	}
	known := map[string]bool{}
	for _, t := range have {
		known[t] = true
	}
	var total int64
	if err := expectDelim(dec, '{'); err != nil {
		return 0, err
	}
	for dec.More() {
		key, err := dec.Token()
		if err != nil {
			return total, fmt.Errorf("dump: %w", err)
		}
		if key != "tables" {
			var skip json.RawMessage
			if err := dec.Decode(&skip); err != nil {
				return total, fmt.Errorf("dump: %w", err)
			}
			continue
		}
		if err := expectDelim(dec, '{'); err != nil {
			return total, err
		}
		for dec.More() {
			t, err := dec.Token()
			if err != nil {
				return total, fmt.Errorf("dump: %w", err)
			}
			table, _ := t.(string)
			if !known[table] {
				return total, fmt.Errorf("dump table %s is not in this database", table)
			}
			n, err := s.importTable(ctx, dec, table)
			total += n
			if err != nil {
				return total, err
			}
		}
		if err := expectDelim(dec, '}'); err != nil {
			return total, err
		}
	}
	return total, expectDelim(dec, '}')
}

func expectDelim(dec *json.Decoder, want json.Delim) error {
	t, err := dec.Token()
	if err != nil {
		return fmt.Errorf("dump: %w", err)
	}
	if d, ok := t.(json.Delim); !ok || d != want {
		return fmt.Errorf("dump: expected %q, found %v", want, t)
	}
	return nil
}

// importTable inserts one table's array of rows; the first row names the columns.
func (s *Store) importTable(ctx context.Context, dec *json.Decoder, table string) (int64, error) {
	if err := expectDelim(dec, '['); err != nil {
		return 0, err
	}
	cols, err := s.columns(table)
	if err != nil {
		return 0, err
	}
	kinds := map[string]colKind{}
	for _, c := range cols {
		kinds[c.Name] = c.Kind
	}
	var use []column
	var prefix, one string
	var batch []map[string]any
	size := 0
	var total int64
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		per := len(use)
		args := make([]any, 0, len(batch)*per)
		for _, row := range batch {
			for _, c := range use {
				v, err := fromDump(row[c.Name], c.Kind)
				if err != nil {
					return fmt.Errorf("%s.%s: %w", table, c.Name, err)
				}
				args = append(args, v)
			}
		}
		q := prefix + strings.TrimSuffix(strings.Repeat(one+",", len(batch)), ",")
		if _, err := s.DB.ExecContext(ctx, q, args...); err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
		total += int64(len(batch))
		batch = batch[:0]
		return nil
	}
	for dec.More() {
		var row map[string]any
		if err := dec.Decode(&row); err != nil {
			return total, fmt.Errorf("dump %s: %w", table, err)
		}
		if use == nil {
			for name := range row {
				k, ok := kinds[name]
				if !ok {
					return total, fmt.Errorf("dump column %s.%s is not in this database", table, name)
				}
				use = append(use, column{Name: name, Kind: k})
			}
			sort.Slice(use, func(a, b int) bool { return use[a].Name < use[b].Name })
			size = max(1, min(500, maxParams/max(1, len(use))))
			one = "(" + strings.TrimSuffix(strings.Repeat("?,", len(use)), ",") + ")"
			prefix = "INSERT INTO " + quoteIdent(table) + " (" + colList(use) + ") VALUES "
		}
		batch = append(batch, row)
		if len(batch) >= size {
			if err := flush(); err != nil {
				return total, err
			}
		}
	}
	if err := flush(); err != nil {
		return total, err
	}
	return total, expectDelim(dec, ']')
}

// fromDump converts one JSON value back to the column kind.
func fromDump(v any, k colKind) (any, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		b64, _ := x["b64"].(string)
		return base64.StdEncoding.DecodeString(b64)
	case json.Number:
		switch k {
		case kindInt:
			if i, err := x.Int64(); err == nil {
				return i, nil
			}
			f, err := x.Float64()
			return int64(f), err
		case kindFloat:
			return x.Float64()
		default:
			return x.String(), nil
		}
	case bool:
		if x {
			return int64(1), nil
		}
		return int64(0), nil
	case string:
		return convert(x, k)
	}
	return nil, fmt.Errorf("unsupported dump value %T", v)
}
