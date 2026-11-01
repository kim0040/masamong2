package storage

import (
	"database/sql"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestNormalizeBackend(t *testing.T) {
	for _, v := range []string{"tidb", "mysql"} {
		if got, err := NormalizeBackend(v); TiDBEnabled && (err != nil || got != BackendTiDB) {
			t.Fatalf("%s: %q %v", v, got, err)
		} else if !TiDBEnabled && err != ErrTiDBDisabled {
			t.Fatalf("%s must be refused while TiDB is disabled: %q %v", v, got, err)
		}
	}
	got, err := NormalizeBackend("local-lite")
	if err != nil || got != BackendSQLite {
		t.Fatalf("local-lite: %q %v", got, err)
	}
}

func TestRedactDSN(t *testing.T) {
	got := RedactDSN("user:secret@tcp(127.0.0.1:4000)/masamong")
	if got != "***@tcp(127.0.0.1:4000)/masamong" {
		t.Fatalf("got %q", got)
	}
}

func TestSQLiteUpsertPoll(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if !s.hasColumn("polls", "created_at") {
		t.Fatal("polls.created_at missing")
	}
	if err := s.WithWriter(func(tx *sql.Tx) error {
		return s.UpsertPoll(tx, "p1", "ch", 1, `{"id":"p1"}`, "2026-01-01T00:00:00Z")
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WithWriter(func(tx *sql.Tx) error {
		return s.UpsertPoll(tx, "p1", "ch", 2, `{"id":"p1","r":2}`, "2026-01-01T00:00:00Z")
	}); err != nil {
		t.Fatal(err)
	}
	var n, rev int
	if err := s.DB.QueryRow(`SELECT COUNT(*) FROM polls`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("count %d %v", n, err)
	}
	if err := s.DB.QueryRow(`SELECT revision FROM polls WHERE id=?`, "p1").Scan(&rev); err != nil || rev != 2 {
		t.Fatalf("rev %d %v", rev, err)
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := SaveSettings(dir, Settings{Backend: BackendTiDB, DSNRef: "env:MASAMONG_TIDB_DSN"}); !TiDBEnabled && err != ErrTiDBDisabled {
		t.Fatalf("saving a TiDB backend must be refused: %v", err)
	}
	if err := SaveSettings(dir, Settings{Backend: BackendSQLite, SQLiteFile: "other.db"}); err != nil {
		t.Fatal(err)
	}
	s, err := LoadSettings(dir)
	if err != nil {
		t.Fatal(err)
	}
	if s.Backend != BackendSQLite || s.SQLiteFile != "other.db" {
		t.Fatalf("%+v", s)
	}
}

func TestTiDBSkippedWithoutDSN(t *testing.T) {
	if !TiDBEnabled {
		t.Skip("the TiDB backend is disabled (TiDBEnabled)")
	}
	if os.Getenv("MASAMONG_TIDB_TEST_DSN") == "" {
		t.Skip("set MASAMONG_TIDB_TEST_DSN to run live TiDB/MySQL adapter test")
	}
	s, err := OpenOptions(Options{Backend: BackendTiDB, DSN: os.Getenv("MASAMONG_TIDB_TEST_DSN")})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if s.Backend != BackendTiDB {
		t.Fatalf("backend %s", s.Backend)
	}
}

func TestDatabaseIsTheOwnersAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps its ACLs")
	}
	dir := filepath.Join(t.TempDir(), "data")
	path := filepath.Join(dir, "masamong.db")
	// a database left by an older version, readable by every account
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.Chmod(dir, 0o755)
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.Close()
	_ = os.Chmod(path, 0o644)
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	for _, p := range []string{dir, path, path + "-wal"} {
		fi, err := os.Stat(p)
		if err != nil {
			continue // -wal is gone after a clean close
		}
		if fi.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is open to others: %v", filepath.Base(p), fi.Mode().Perm())
		}
	}
}
