package storage

import (
	"database/sql"
	"path/filepath"
	"testing"

	"masamong/internal/hostprofile"
)

func TestOpenUsesHostProfilePoolAndCache(t *testing.T) {
	prev := hostprofile.Current()
	t.Cleanup(func() { hostprofile.Apply(prev) })

	// Unset profile: today's read pool and SQLite's default cache, on the writer too.
	// The single writer connection does not change with the tier.
	checkPool := func(t *testing.T, readers, cacheKiB int) {
		t.Helper()
		s, err := Open(filepath.Join(t.TempDir(), "m.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = s.Close() })
		if s.DB.Stats().MaxOpenConnections != 1 {
			t.Fatalf("writer connections %d", s.DB.Stats().MaxOpenConnections)
		}
		if s.Read.Stats().MaxOpenConnections != readers {
			t.Fatalf("reader connections %d want %d", s.Read.Stats().MaxOpenConnections, readers)
		}
		if got := pragmaCacheKiB(t, s.Read); got != cacheKiB {
			t.Fatalf("reader cache %d KiB want %d", got, cacheKiB)
		}
		if got := pragmaCacheKiB(t, s.DB); got != cacheKiB {
			t.Fatalf("writer cache %d KiB want %d", got, cacheKiB)
		}
	}

	checkPool(t, 4, 2000)

	for _, tc := range []struct{ readers, cache int }{{2, 2000}, {12, 32768}} {
		p := prev
		p.ReaderConns = tc.readers
		p.SQLiteCacheKiB = tc.cache
		hostprofile.Apply(p)
		checkPool(t, tc.readers, tc.cache)
	}
}

func pragmaCacheKiB(t *testing.T, db *sql.DB) int {
	t.Helper()
	var n int
	if err := db.QueryRow(`PRAGMA cache_size`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	// A positive result is pages, not KiB. The DSN sets a negative value on purpose.
	if n >= 0 {
		t.Fatalf("cache_size %d is pages, not KiB", n)
	}
	return -n
}
