package application

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// Feature tables register themselves from init() functions into featureDDL (R8 in the code standard).
// An init() that is deleted, moved to a file that is not built, or written with a typo leaves a table
// missing, and the first query on it fails on a live instance. These tests make the registration visible.

var (
	ddlName = regexp.MustCompile(`(?i)CREATE\s+(?:UNIQUE\s+)?(TABLE|INDEX)\s+IF\s+NOT\s+EXISTS\s+([a-z_][a-z0-9_]*)`)
	// the table in a data statement; SQL keywords are written in capitals in this package, prose is not
	// (a name followed by "=" is a column: MySQL's ON DUPLICATE KEY UPDATE rating=VALUES(rating))
	sqlTable = regexp.MustCompile(`\b(?:FROM|INTO|UPDATE|JOIN|TABLE(?:\s+IF\s+NOT\s+EXISTS)?)\s+([a-z_][a-z0-9_]*)\b\s*(=?)`)
)

func practiceNames(t *testing.T, s *Service) map[string]bool {
	t.Helper()
	rows, err := s.Store.DB.Query(`SELECT name FROM sqlite_master`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	have := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		have[name] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return have
}

// Every table and index a registered statement names exists once the service has opened, and running
// the migration again changes nothing (it runs on every start).
func TestRegisteredFeatureTablesExist(t *testing.T) {
	t.Parallel()
	s, err := OpenPractice(context.Background(), t.TempDir(), "en", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if len(featureDDL) < 20 {
		t.Fatalf("only %d feature DDL statements are registered; an init() that registers tables is missing", len(featureDDL))
	}
	have := practiceNames(t, s)
	named := 0
	for _, q := range featureDDL {
		m := ddlName.FindStringSubmatch(q)
		if m == nil {
			if !strings.HasPrefix(strings.TrimSpace(q), "INSERT ") { // seed rows are allowed; anything else should create something
				t.Errorf("a registered statement creates nothing the test can name: %.60q", q)
			}
			continue
		}
		named++
		if !have[m[2]] {
			t.Errorf("%s %s is registered in featureDDL but does not exist after open", strings.ToLower(m[1]), m[2])
		}
	}
	if named == 0 {
		t.Fatal("no registered statement names a table")
	}
	if err := migrateFeatures(s.Store); err != nil {
		t.Fatalf("running the feature migration a second time: %v", err)
	}
	if again := practiceNames(t, s); len(again) != len(have) {
		t.Fatalf("a second migration changed the schema: %d names, then %d", len(have), len(again))
	}
}

// Not a table of this database, so the source scan below leaves it alone.
var notFeatureTables = map[string]string{
	"sqlite_master":      "SQLite's own catalog",
	"json_each":          "SQLite's table-valued function",
	"information_schema": "TiDB-only size query (backup.go), behind the disabled TiDB backend",
}

// Every table the code of this package reads or writes exists on a freshly opened instance. This is
// the check that fails when a registration is taken out: the SQL stays in the code and the table is
// gone. migration.go is skipped on purpose: it reads the old Masamong V1 database, not this one.
func TestEveryTableTheCodeNamesExists(t *testing.T) {
	t.Parallel()
	s, err := OpenPractice(context.Background(), t.TempDir(), "en", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	have := practiceNames(t, s)
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	where := map[string]string{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") || f == "migration.go" {
			continue
		}
		af, err := parser.ParseFile(token.NewFileSet(), f, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(af, func(n ast.Node) bool {
			lit, ok := n.(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return true
			}
			text, err := strconv.Unquote(lit.Value)
			if err != nil || !isSQL(text) {
				return true
			}
			for _, m := range sqlTable.FindAllStringSubmatch(text, -1) {
				if m[2] == "=" {
					continue
				}
				if _, ok := where[m[1]]; !ok {
					where[m[1]] = f
				}
			}
			return true
		})
	}
	if len(where) < 40 {
		t.Fatalf("found only %d table names in the SQL of the package; the scan or the code changed shape", len(where))
	}
	names := make([]string, 0, len(where))
	for name := range where {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if have[name] || notFeatureTables[name] != "" || cteAlias(name) {
			continue
		}
		t.Errorf("%s names table %q, which does not exist on a freshly opened instance (is its DDL registered?)", where[name], name)
	}
}

func isSQL(text string) bool {
	text = strings.TrimSpace(text)
	for _, kw := range []string{"SELECT ", "INSERT ", "UPDATE ", "DELETE ", "CREATE ", "ALTER ", "WITH ", "REPLACE "} {
		if strings.HasPrefix(text, kw) {
			return true
		}
	}
	return false
}

// cteAlias: a one-letter name after FROM or JOIN is a WITH alias, not a table.
func cteAlias(name string) bool { return len(name) == 1 }
