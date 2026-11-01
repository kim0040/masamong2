package storage

// The TiDB adapter is disabled (see TiDBEnabled in settings.go for why); NormalizeBackend refuses
// "tidb" before anything here runs. The code stays so the backend can be switched back on.

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func openTiDB(dsn string) (*Store, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, fmt.Errorf("tidb DSN required (env:MASAMONG_TIDB_DSN)")
	}
	dsn = ensureMySQLParams(dsn)
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	// Chat turns, the console and background jobs query in parallel. Bound the pool, and recycle
	// connections before TiDB Cloud / proxies drop idle ones (typically after 5-10 minutes).
	db.SetMaxOpenConns(16)
	db.SetMaxIdleConns(8)
	db.SetConnMaxLifetime(5 * time.Minute)
	db.SetConnMaxIdleTime(2 * time.Minute)
	if err := db.Ping(); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("tidb ping: %w", err)
	}
	s := &Store{DB: db, Read: db, Backend: BackendTiDB, path: RedactDSN(dsn)}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

func ensureMySQLParams(dsn string) string {
	if strings.Contains(dsn, "://") {
		return dsn
	}
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	if !strings.Contains(dsn, "parseTime=") {
		dsn += sep + "parseTime=false"
		sep = "&"
	}
	if !strings.Contains(dsn, "charset=") {
		dsn += sep + "charset=utf8mb4"
		sep = "&"
	}
	if !strings.Contains(dsn, "loc=") {
		dsn += sep + "loc=UTC"
		sep = "&"
	}
	// Placeholders are filled in by the driver (safe with utf8mb4) instead of a server-side prepare,
	// execute and close for every statement: one round trip per query instead of two or three, which
	// is most of a query's time against a cluster in another region.
	if !strings.Contains(dsn, "interpolateParams=") {
		dsn += sep + "interpolateParams=true"
	}
	return dsn
}

// InspectTiDB lists the tables of the DSN's database without creating anything, so the migration
// wizard can refuse a database that belongs to another program before any DDL runs.
func InspectTiDB(dsn string) ([]string, error) {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return nil, fmt.Errorf("tidb DSN required")
	}
	db, err := sql.Open("mysql", ensureMySQLParams(dsn))
	if err != nil {
		return nil, err
	}
	defer db.Close()
	if err := db.Ping(); err != nil {
		return nil, fmt.Errorf("tidb ping: %w", err)
	}
	s := &Store{DB: db, Backend: BackendTiDB}
	return s.Tables()
}
