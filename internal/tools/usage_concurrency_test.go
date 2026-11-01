package tools

import (
	"database/sql"
	"fmt"
	"math/rand/v2"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/go-sql-driver/mysql"

	"masamong/internal/storage"
)

// Parallel calls never take more than the limit: on SQLite the single writer serializes them, on TiDB
// they lock the counters they check (including today's not-yet-written ones). Set
// MASAMONG_TIDB_TEST_DSN to run it against TiDB as well.
func TestReserveHoldsTheLimitUnderConcurrency(t *testing.T) {
	stores := map[string]*storage.Store{}
	s, err := storage.Open(filepath.Join(t.TempDir(), "u.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	stores["sqlite"] = s
	if base := os.Getenv("MASAMONG_TIDB_TEST_DSN"); base != "" && storage.TiDBEnabled {
		name := fmt.Sprintf("masamong_usage_%d", rand.Int64N(1e9))
		admin, err := sql.Open("mysql", base)
		if err != nil {
			t.Fatal(err)
		}
		defer admin.Close()
		if _, err := admin.Exec("CREATE DATABASE " + name); err != nil {
			t.Fatal(err)
		}
		defer admin.Exec("DROP DATABASE " + name)
		slash := strings.LastIndex(base, "/")
		dsn := base[:slash+1] + name
		if i := strings.Index(base[slash:], "?"); i >= 0 {
			dsn += base[slash+i:]
		}
		ts, err := storage.OpenOptions(storage.Options{Backend: storage.BackendTiDB, DSN: dsn})
		if err != nil {
			t.Fatal(err)
		}
		defer ts.Close()
		stores["tidb"] = ts
	}
	for name, st := range stores {
		t.Run(name, func(t *testing.T) {
			u, err := OpenUsage(st)
			if err != nil {
				t.Fatal(err)
			}
			const limit, callers = 5, 24
			var wg sync.WaitGroup
			var mu sync.Mutex
			ok, errs := 0, 0
			now := time.Now()
			for i := 0; i < callers; i++ {
				wg.Add(1)
				go func(i int) {
					defer wg.Done()
					d, err := u.Reserve("2026-09-25", now, "weather", Limits{PerDay: limit, PerUserPerDay: 100},
						Caller{UserID: fmt.Sprintf("u%d", i%3), Channel: "discord:g:c"})
					mu.Lock()
					defer mu.Unlock()
					if err != nil {
						errs++
					} else if d.OK {
						ok++
					}
				}(i)
			}
			wg.Wait()
			if ok != limit || errs != 0 {
				t.Fatalf("%d calls admitted (limit %d), %d errors", ok, limit, errs)
			}
			counts, err := u.Today("2026-09-25")
			if err != nil || counts["weather"] != limit {
				t.Fatalf("stored count %d: %v", counts["weather"], err)
			}
		})
	}
}
