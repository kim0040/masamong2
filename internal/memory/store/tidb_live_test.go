package store

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"strings"
	"testing"

	"masamong/internal/storage"
)

// openTiDBStore opens the memory tables on a fresh database of the MASAMONG_TIDB_TEST_DSN server.
func openTiDBStore(t *testing.T) *Store {
	t.Helper()
	if !storage.TiDBEnabled {
		t.Skip("the TiDB backend is disabled (storage.TiDBEnabled)")
	}
	base := os.Getenv("MASAMONG_TIDB_TEST_DSN")
	if base == "" {
		t.Skip("set MASAMONG_TIDB_TEST_DSN to run against a live TiDB")
	}
	name := fmt.Sprintf("masamong_store_%d", rand.Int64N(1e9))
	admin, err := sql.Open("mysql", base)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = admin.Close() })
	if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP DATABASE " + name) })
	slash := strings.LastIndex(base, "/")
	dsn := base[:slash+1] + name
	if i := strings.Index(base[slash:], "?"); i >= 0 {
		dsn += base[slash+i:]
	}
	db, err := storage.OpenOptions(storage.Options{Backend: storage.BackendTiDB, DSN: dsn})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	st, err := Open(db)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

// The statements written separately for each backend behave the same on TiDB: trimming the chat
// history (a DELETE reading its own table), the one-row cache insert, and a leaf vector replacement.
func TestTiDBStatements(t *testing.T) {
	st := openTiDBStore(t)
	for i := 0; i < 5; i++ {
		if err := st.AppendChatTurns("c", []ChatTurn{{Role: "user", Content: "q"}, {Role: "assistant", Content: "a"}}, 4); err != nil {
			t.Fatal(err)
		}
	}
	turns, _ := st.RecentChatTurns("c", 100)
	if len(turns) != 4 || turns[3].Role != "assistant" {
		t.Fatalf("%+v", turns)
	}
	if err := st.PutQueryVectors(map[string][]byte{"k": {1, 2}}); err != nil {
		t.Fatal(err)
	}
	if err := st.PutQueryVectors(map[string][]byte{"k": {3, 4}}); err != nil { // existing key kept
		t.Fatal(err)
	}
	if v, ok := st.CachedVector("k"); !ok || v[0] != 1 {
		t.Fatalf("%v %v", v, ok)
	}
	if n, err := st.ShrinkSpace("a", "b", func(b []byte) []byte { return b }); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if _, err := st.DropCachedDuplicates(func(string) string { return "m" }, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if _, err := st.Spaces(); err != nil {
		t.Fatal(err)
	}
	if _, err := st.LeafVectors([]string{"x"}); err != nil {
		t.Fatal(err)
	}
}
