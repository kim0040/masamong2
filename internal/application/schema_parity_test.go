package application

import (
	"fmt"
	"reflect"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The admin tables (admin.go and the files that add to adminSQLite and adminTiDB from init()) keep one
// definition per dialect, like internal/storage/schema.go. TiDB is off at build time (storage.TiDBEnabled),
// so nothing runs adminTiDB. The decision of 2026-09-28 is to keep that code; this test keeps the two
// lists defining the same tables, columns, primary keys, unique columns and indexes (types may differ).
// The reader below is the same as in internal/storage/schema_parity_test.go: a test helper cannot be
// imported across packages, and the shared test helper package is a later item (T-A).

type ddlTable struct {
	columns []string
	primary []string
	unique  []string // single-column UNIQUE
}

type ddlIndex struct {
	table   string
	columns []string
}

type ddlSchema struct {
	tables  map[string]*ddlTable
	indexes map[string]ddlIndex
}

var (
	createTable = regexp.MustCompile(`(?is)^CREATE\s+TABLE\s+IF\s+NOT\s+EXISTS\s+([a-z_][a-z0-9_]*)\s*\((.*)\)\s*(?:WITHOUT\s+ROWID)?\s*$`)
	createIndex = regexp.MustCompile(`(?is)^CREATE\s+(?:UNIQUE\s+)?INDEX\s+IF\s+NOT\s+EXISTS\s+([a-z_][a-z0-9_]*)\s+ON\s+([a-z_][a-z0-9_]*)\s*\((.*)\)\s*$`)
	inlineKey   = regexp.MustCompile(`(?is)^(?:UNIQUE\s+)?(?:KEY|INDEX)\s+([a-z_][a-z0-9_]*)\s*\((.*)\)$`)
	primaryKey  = regexp.MustCompile(`(?is)^PRIMARY\s+KEY\s*\((.*)\)$`)
	prefixLen   = regexp.MustCompile(`\(\d+\)`)
)

// splitTop splits on commas outside parentheses.
func splitTop(s string) []string {
	var parts []string
	depth, start := 0, 0
	for i, r := range s {
		switch r {
		case '(':
			depth++
		case ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, strings.TrimSpace(s[start:i]))
				start = i + 1
			}
		}
	}
	return append(parts, strings.TrimSpace(s[start:]))
}

// columnList reads "a, b DESC, c(64)" as [a b c].
func columnList(s string) []string {
	var out []string
	for _, p := range splitTop(s) {
		f := strings.Fields(prefixLen.ReplaceAllString(p, ""))
		if len(f) > 0 {
			out = append(out, f[0])
		}
	}
	return out
}

func parseDDL(t *testing.T, stmts []string) ddlSchema {
	t.Helper()
	sc := ddlSchema{tables: map[string]*ddlTable{}, indexes: map[string]ddlIndex{}}
	for _, q := range stmts {
		q = strings.TrimSpace(q)
		if m := createTable.FindStringSubmatch(q); m != nil {
			tb := &ddlTable{}
			for _, def := range splitTop(m[2]) {
				switch up := strings.ToUpper(def); {
				case primaryKey.MatchString(def):
					tb.primary = columnList(primaryKey.FindStringSubmatch(def)[1])
				case inlineKey.MatchString(def):
					k := inlineKey.FindStringSubmatch(def)
					sc.indexes[k[1]] = ddlIndex{table: m[1], columns: columnList(k[2])}
				case strings.HasPrefix(up, "UNIQUE") || strings.HasPrefix(up, "CONSTRAINT") || strings.HasPrefix(up, "FOREIGN"):
					t.Fatalf("table %s: constraint the parity test does not read: %.60q", m[1], def)
				default:
					name := strings.Fields(def)[0]
					tb.columns = append(tb.columns, name)
					if strings.Contains(up, "PRIMARY KEY") {
						tb.primary = []string{name}
					}
					if strings.Contains(up, " UNIQUE") {
						tb.unique = append(tb.unique, name)
					}
				}
			}
			if _, dup := sc.tables[m[1]]; dup {
				t.Fatalf("table %s is defined twice in one list", m[1])
			}
			sc.tables[m[1]] = tb
		} else if m := createIndex.FindStringSubmatch(q); m != nil {
			sc.indexes[m[1]] = ddlIndex{table: m[2], columns: columnList(m[3])}
		} else {
			t.Fatalf("statement the parity test does not read: %.60q", q)
		}
	}
	return sc
}

// compareDDL returns what differs between the SQLite and the TiDB definition, as one line each.
func compareDDL(sqlite, tidb ddlSchema) []string {
	var diffs []string
	names := func(m map[string]*ddlTable) []string {
		var out []string
		for k := range m {
			out = append(out, k)
		}
		sort.Strings(out)
		return out
	}
	for _, n := range names(sqlite.tables) {
		a, b := sqlite.tables[n], tidb.tables[n]
		if b == nil {
			diffs = append(diffs, fmt.Sprintf("table %s: in the SQLite list, not in the TiDB list", n))
			continue
		}
		if !reflect.DeepEqual(a.columns, b.columns) {
			diffs = append(diffs, fmt.Sprintf("table %s: columns differ: sqlite %v, tidb %v", n, a.columns, b.columns))
		}
		if !reflect.DeepEqual(a.primary, b.primary) {
			diffs = append(diffs, fmt.Sprintf("table %s: primary key differs: sqlite %v, tidb %v", n, a.primary, b.primary))
		}
		if !reflect.DeepEqual(a.unique, b.unique) {
			diffs = append(diffs, fmt.Sprintf("table %s: unique columns differ: sqlite %v, tidb %v", n, a.unique, b.unique))
		}
	}
	for _, n := range names(tidb.tables) {
		if sqlite.tables[n] == nil {
			diffs = append(diffs, fmt.Sprintf("table %s: in the TiDB list, not in the SQLite list", n))
		}
	}
	var idx []string
	for k := range sqlite.indexes {
		idx = append(idx, k)
	}
	for k := range tidb.indexes {
		if _, ok := sqlite.indexes[k]; !ok {
			idx = append(idx, k)
		}
	}
	sort.Strings(idx)
	for _, n := range idx {
		a, aok := sqlite.indexes[n]
		b, bok := tidb.indexes[n]
		switch {
		case !aok:
			diffs = append(diffs, fmt.Sprintf("index %s: in the TiDB list, not in the SQLite list", n))
		case !bok:
			diffs = append(diffs, fmt.Sprintf("index %s: in the SQLite list, not in the TiDB list", n))
		case a.table != b.table || !reflect.DeepEqual(a.columns, b.columns):
			diffs = append(diffs, fmt.Sprintf("index %s: sqlite %s%v, tidb %s%v", n, a.table, a.columns, b.table, b.columns))
		}
	}
	return diffs
}

func TestAdminSQLiteAndTiDBSchemasDefineTheSameTables(t *testing.T) {
	sqlite, tidb := parseDDL(t, adminSQLite), parseDDL(t, adminTiDB)
	if len(sqlite.tables) < 12 {
		t.Fatalf("read only %d tables from adminSQLite; an init() that appends to it is missing or the parser changed", len(sqlite.tables))
	}
	for _, d := range compareDDL(sqlite, tidb) {
		t.Error(d)
	}
}
