package application

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/storage"
)

// The schema golden files (E7 review F2, step 2a) pin what the database looks like, so that moving
// the table lists out of this package, or replacing their init() registration with explicit
// lists, can be shown to change nothing:
//
//   - the order of adminSQLite, adminTiDB, featureDDL and pruneRules (statement head and a hash of
//     its whitespace-normalised text), since an index created in one file on a table from another
//     needs the table first;
//   - the sqlite_master rows of the database the application itself opens (Open, setup, then the
//     admin migration): every table, index and trigger with its SQL;
//   - what an older database turns into: testdata/schema_upgrade_old.sql, the first shape of the
//     tables the central add-column list and migrateEvidence changed, migrated, and the objects whose
//     SQL differs from a fresh store's (the columns land at the end of the CREATE text).
//
// A schema package that tests will use must produce the same sqlite_master as the store here. Rewrite
// the golden files, after reading the diff, with: go test ./internal/application -run 'TestSchema' -update

const (
	schemaGolden        = "testdata/schema.golden"
	schemaUpgradeGolden = "testdata/schema_upgrade.golden"
	schemaOldFixture    = "testdata/schema_upgrade_old.sql"
)

// stmtLine is one statement of a list: its index, its first words and a short hash of all of it.
func stmtLine(i int, q string) string {
	norm := strings.Join(strings.Fields(q), " ")
	head := norm
	if len(head) > 72 {
		head = head[:72]
	}
	sum := sha256.Sum256([]byte(norm))
	return fmt.Sprintf("%3d  %s  %s", i, hex.EncodeToString(sum[:6]), head)
}

func listSection(name string, stmts []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "== %s (%d)\n", name, len(stmts))
	for i, q := range stmts {
		b.WriteString(stmtLine(i, q) + "\n")
	}
	return b.String()
}

func pruneSection() string {
	var b strings.Builder
	fmt.Fprintf(&b, "== pruneRules (%d)\n", len(pruneRules))
	for i, r := range pruneRules {
		fmt.Fprintf(&b, "%3d  %s table=%s column=%s days=%s dayOnly=%v extra=%q\n", i, r.name, r.table, r.column, r.days, r.dayOnly, r.extra)
	}
	return b.String()
}

// schemaObject is one row of sqlite_master.
type schemaObject struct{ kind, name, table, sql string }

func (o schemaObject) key() string { return o.kind + " " + o.name }

func readMaster(t *testing.T, db *sql.DB) []schemaObject {
	t.Helper()
	rows, err := db.Query(`SELECT type, name, tbl_name, COALESCE(sql, '') FROM sqlite_master WHERE name NOT LIKE 'sqlite_%' ORDER BY type, name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []schemaObject
	for rows.Next() {
		var o schemaObject
		if err := rows.Scan(&o.kind, &o.name, &o.table, &o.sql); err != nil {
			t.Fatal(err)
		}
		out = append(out, o)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func objectsSection(title string, objs []schemaObject) string {
	var b strings.Builder
	fmt.Fprintf(&b, "== %s (%d objects)\n", title, len(objs))
	for _, o := range objs {
		fmt.Fprintf(&b, "-- %s %s on %s\n%s\n", o.kind, o.name, o.table, o.sql)
	}
	return b.String()
}

// applicationStore opens the database the application opens: Open, the setup that creates the store,
// then the admin migration. The caller reads it before Close.
func applicationStore(t *testing.T) *Service {
	t.Helper()
	s, err := Open(t.TempDir(), nil, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	if r := s.Execute(context.Background(), action.Request{Action: "setup.resume", Input: []byte(`{"instance":"schema","timezone":"Asia/Seoul"}`)}); !r.OK {
		t.Fatal(r.Error)
	}
	if _, err := s.adminDB(); err != nil {
		t.Fatal(err)
	}
	return s
}

// bareStore is a fresh SQLite store with only the admin migration run on it.
func bareStore(t *testing.T) *storage.Store {
	t.Helper()
	st, err := storage.Open(filepath.Join(t.TempDir(), "bare.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func checkGolden(t *testing.T, path, got string) {
	t.Helper()
	if *updateGoldens {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (create it with: go test ./internal/application -run TestSchema -update)", err)
	}
	if got != string(want) {
		t.Errorf("%s differs from what the code produces:\n%s\nIf this is intended, read the diff above and run: go test ./internal/application -run TestSchema -update", path, goldenDiff(string(want), got))
	}
}

// goldenDiff shows the first lines where the two texts differ, with a little context.
func goldenDiff(want, got string) string {
	w, g := strings.Split(want, "\n"), strings.Split(got, "\n")
	for i := 0; i < len(w) || i < len(g); i++ {
		var a, b string
		if i < len(w) {
			a = w[i]
		}
		if i < len(g) {
			b = g[i]
		}
		if a != b {
			lo := max(0, i-2)
			var out []string
			for j := lo; j < i; j++ {
				out = append(out, "  "+w[j])
			}
			out = append(out, "- "+a, "+ "+b)
			return fmt.Sprintf("first difference at line %d:\n%s", i+1, strings.Join(out, "\n"))
		}
	}
	return "(identical)"
}

func TestSchemaGoldenFreshStore(t *testing.T) {
	s := applicationStore(t)
	var b strings.Builder
	b.WriteString("# The database layout of internal/application (E7 step 2a). Rewrite with: go test ./internal/application -run TestSchema -update\n")
	b.WriteString(listSection("adminSQLite", adminSQLite))
	b.WriteString(listSection("adminTiDB", adminTiDB))
	b.WriteString(listSection("featureDDL", featureDDL))
	b.WriteString(pruneSection())
	b.WriteString(objectsSection("sqlite_master of the store the application opens", readMaster(t, s.Store.DB)))
	checkGolden(t, schemaGolden, b.String())
}

// TestAdminMigrationAloneMakesTheSameObjects: the admin migration is the one function a test store
// has to call, and what it makes is what the application's own store holds for the same names.
func TestAdminMigrationAloneMakesTheSameObjects(t *testing.T) {
	app := map[string]schemaObject{}
	for _, o := range readMaster(t, applicationStore(t).Store.DB) {
		app[o.key()] = o
	}
	st := bareStore(t)
	if err := migrateAdmin(st); err != nil {
		t.Fatal(err)
	}
	bare := readMaster(t, st.DB)
	if len(bare) < 90 {
		t.Fatalf("the admin migration made only %d objects", len(bare))
	}
	for _, o := range bare {
		want, ok := app[o.key()]
		if !ok {
			t.Errorf("%s exists after migrateAdmin alone but not in the application's store", o.key())
			continue
		}
		if o.sql != want.sql {
			t.Errorf("%s differs:\n  migrateAdmin alone: %s\n  application store:  %s", o.key(), o.sql, want.sql)
		}
	}
}

func TestSchemaGoldenUpgradeFromOlderShape(t *testing.T) {
	fixture, err := os.ReadFile(schemaOldFixture)
	if err != nil {
		t.Fatal(err)
	}
	st := bareStore(t)
	if _, err := st.DB.Exec(string(fixture)); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if err := migrateAdmin(st); err != nil {
		t.Fatalf("migrating the older database: %v", err)
	}
	upgraded := readMaster(t, st.DB)

	fresh := bareStore(t)
	if err := migrateAdmin(fresh); err != nil {
		t.Fatal(err)
	}
	freshByKey := map[string]schemaObject{}
	for _, o := range readMaster(t, fresh.DB) {
		freshByKey[o.key()] = o
	}

	// the same objects, and the same columns with the same declared types in every table
	if len(upgraded) != len(freshByKey) {
		t.Errorf("upgraded database has %d objects, a fresh one %d", len(upgraded), len(freshByKey))
	}
	var differing []schemaObject
	for _, o := range upgraded {
		f, ok := freshByKey[o.key()]
		if !ok {
			t.Errorf("%s exists only in the upgraded database", o.key())
			continue
		}
		if o.sql != f.sql {
			differing = append(differing, o)
		}
		if o.kind == "table" {
			if got, want := columnsOf(t, st.DB, o.name), columnsOf(t, fresh.DB, o.name); strings.Join(got, ",") != strings.Join(want, ",") {
				t.Errorf("table %s: upgraded columns %v, fresh columns %v", o.name, got, want)
			}
		}
	}

	// the old rows survive, and the columns they did not have hold their defaults
	var rule, task, kind, speaker, actor, actorID string
	var calls int
	if err := st.DB.QueryRow(`SELECT repeat_rule, task, kind FROM reminders WHERE id='rem-old'`).Scan(&rule, &task, &kind); err != nil {
		t.Fatalf("reminder lost in the upgrade: %v", err)
	}
	if err := st.DB.QueryRow(`SELECT speaker_ref FROM turn_log WHERE id='turn-old'`).Scan(&speaker); err != nil {
		t.Fatalf("turn lost in the upgrade: %v", err)
	}
	if err := st.DB.QueryRow(`SELECT actor, actor_id FROM audit_log`).Scan(&actor, &actorID); err != nil {
		t.Fatalf("audit row lost in the upgrade: %v", err)
	}
	if err := st.DB.QueryRow(`SELECT call_count FROM llm_calls`).Scan(&calls); err != nil {
		t.Fatalf("ledger row lost in the upgrade: %v", err)
	}
	if rule != "" || task != "" || kind != "" || speaker != "" || actor != "" || actorID != "" {
		t.Errorf("defaults of the added columns: repeat_rule=%q task=%q kind=%q speaker_ref=%q actor=%q actor_id=%q; all want empty", rule, task, kind, speaker, actor, actorID)
	}
	if calls != 1 {
		t.Errorf("llm_calls.call_count of the old row = %d, want 1", calls)
	}

	// migrating again changes nothing
	if err := migrateAdmin(st); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	again := readMaster(t, st.DB)
	if len(again) != len(upgraded) {
		t.Fatalf("a second migration changed the object count: %d -> %d", len(upgraded), len(again))
	}
	for i := range again {
		if again[i] != upgraded[i] {
			t.Errorf("a second migration changed %s", again[i].key())
		}
	}

	sort.Slice(differing, func(i, j int) bool { return differing[i].key() < differing[j].key() })
	var b strings.Builder
	b.WriteString("# What testdata/schema_upgrade_old.sql becomes after migrateAdmin: the objects whose SQL differs from a fresh store's.\n")
	b.WriteString("# The added columns sit at the end of the CREATE text, which is also where the add-column list put them in older databases.\n")
	b.WriteString(objectsSection("upgraded objects that differ from a fresh store", differing))
	checkGolden(t, schemaUpgradeGolden, b.String())
}

// columnsOf lists "name type" of a table's columns in column order.
func columnsOf(t *testing.T, db *sql.DB, table string) []string {
	t.Helper()
	rows, err := db.Query(`SELECT name, type FROM pragma_table_info(?) ORDER BY name`, table)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n, ty string
		if err := rows.Scan(&n, &ty); err != nil {
			t.Fatal(err)
		}
		out = append(out, n+" "+ty)
	}
	return out
}
