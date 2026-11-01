package storage

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"masamong/internal/fsutil"
	"masamong/internal/hostprofile"
)

// Store is the V2 product DB: one SQLite file. The TiDB branches in this package and the ones that
// write SQL are dormant while the TiDB backend is disabled (TiDBEnabled); DDL and upsert syntax stay
// per-backend so it can come back.
type Store struct {
	DB *sql.DB
	// Read serves long read-only scans (loading the memory index, backups). With SQLite, DB has a single
	// connection that every write goes through; Read is a separate pool of query-only connections that,
	// in WAL mode, read a consistent snapshot while writes continue. With TiDB it is DB.
	Read          *sql.DB
	Backend       string
	path          string // sqlite file or redacted DSN
	mu            sync.Mutex
	eventMu       sync.Mutex
	eventFileMu   sync.Mutex
	eventWakeOnce sync.Once
	eventWake     chan struct{}
}

func Open(path string) (*Store, error) {
	return OpenOptions(Options{Backend: BackendSQLite, SQLitePath: path})
}

func OpenOptions(opt Options) (*Store, error) {
	backend, err := NormalizeBackend(opt.Backend)
	if err != nil {
		return nil, err
	}
	switch backend {
	case BackendSQLite:
		return openSQLite(opt.SQLitePath)
	case BackendTiDB:
		return openTiDB(opt.DSN)
	default:
		return nil, fmt.Errorf("unknown backend %s", backend)
	}
}

func openSQLite(path string) (*Store, error) {
	if err := fsutil.PrivateDir(filepath.Dir(path)); err != nil {
		return nil, err
	}
	// the conversation database is the owner's alone; SQLite gives its -wal and -shm files the same mode
	if f, err := os.OpenFile(path, os.O_CREATE|os.O_RDONLY, 0o600); err == nil {
		_ = f.Close()
	}
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		_ = fsutil.PrivateFile(p) // a database made before this was 0644
	}
	// journal_size_limit trims the WAL file after a checkpoint that followed a burst of writes (it
	// otherwise keeps its largest size).
	// WHY cache_size is negative: SQLite treats a negative value as KiB. A positive one would be
	// pages, so the same number would mean four times the RAM on the default 4 KiB page.
	// The writer keeps one connection; the cache still helps the hot pages it rewrites.
	hp := hostprofile.Current()
	readers, cacheKiB := hp.ReaderConns, hp.SQLiteCacheKiB
	// WHY these floors: SetMaxOpenConns(0) means unlimited, and a zero cache pragma is not the
	// engine default. Medium (the unset profile) is 4 connections and 2000 KiB.
	if readers < 1 {
		readers = 4
	}
	if cacheKiB < 1 {
		cacheKiB = 2000
	}
	cachePragma := fmt.Sprintf("&_pragma=cache_size(-%d)", cacheKiB)
	dsn := path + "?_txlock=immediate&_pragma=foreign_keys(1)&_pragma=busy_timeout(100)&_pragma=journal_mode(WAL)&_pragma=journal_size_limit(67108864)" + cachePragma
	db, err := sql.Open("masamong-sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db, Read: db, Backend: BackendSQLite, path: path}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	// opened after the writer switched the file to WAL, which is what lets readers run beside it
	read, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=query_only(1)"+cachePragma)
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	read.SetMaxOpenConns(readers)
	// WHY idle stays 2: the open cap grows on a large host, but idle connections keep their page
	// cache. Two warm readers are what the pool kept before tiers.
	read.SetMaxIdleConns(2)
	read.SetConnMaxIdleTime(5 * time.Minute)
	s.Read = read
	return s, nil
}

func (s *Store) Close() error {
	var err error
	if s.Read != nil && s.Read != s.DB {
		err = s.Read.Close()
	}
	if e := s.DB.Close(); e != nil {
		err = e
	}
	return err
}

func (s *Store) Path() string { return s.path }

// SnapshotTo writes a consistent copy of the SQLite database to dst (VACUUM INTO) from a connection
// of its own: live writes carry on while it copies (WAL), instead of waiting behind the single writer
// connection for the whole copy.
func (s *Store) SnapshotTo(ctx context.Context, dst string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if !s.IsSQLite() {
		return fmt.Errorf("snapshot: %s is not a SQLite store", s.Backend)
	}
	db, err := sql.Open("sqlite", s.path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return err
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	_, err = db.ExecContext(ctx, `VACUUM INTO ?`, dst)
	return err
}

// Reader is the pool for long read-only scans (see Read).
func (s *Store) Reader() *sql.DB {
	if s.Read != nil {
		return s.Read
	}
	return s.DB
}

func (s *Store) IsSQLite() bool { return s.Backend == BackendSQLite }

func (s *Store) migrate() error {
	stmts := sqliteDDL
	if s.Backend == BackendTiDB {
		stmts = tidbDDL
	}
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for i, q := range stmts {
		if _, err := tx.Exec(q); err != nil {
			return fmt.Errorf("migration %d: %w", i, err)
		}
	}
	var n int
	if err := tx.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		return err
	}
	if n == 0 {
		if _, err := tx.Exec(`INSERT INTO schema_migrations(version, applied_at) VALUES (1, ?)`, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.ensurePollsCreatedAt()
}

func (s *Store) ensurePollsCreatedAt() error {
	if s.hasColumn("polls", "created_at") {
		return nil
	}
	_, err := s.DB.Exec(`ALTER TABLE polls ADD COLUMN created_at TEXT NOT NULL DEFAULT ''`)
	if err != nil && s.Backend == BackendTiDB {
		_, err = s.DB.Exec(`ALTER TABLE polls ADD COLUMN created_at VARCHAR(64) NOT NULL DEFAULT ''`)
	}
	return err
}

// HasColumn reports whether table has col (for additive migrations).
func (s *Store) HasColumn(table, col string) bool { return s.hasColumn(table, col) }

func (s *Store) hasColumn(table, col string) bool {
	if s.Backend == BackendTiDB {
		var name string
		err := s.DB.QueryRow(
			`SELECT COLUMN_NAME FROM information_schema.COLUMNS WHERE TABLE_SCHEMA = DATABASE() AND TABLE_NAME = ? AND COLUMN_NAME = ?`,
			table, col,
		).Scan(&name)
		return err == nil
	}
	rows, err := s.DB.Query(`PRAGMA table_info(` + table + `)`)
	if err != nil {
		return false
	}
	defer rows.Close()
	for rows.Next() {
		var cid int
		var name, ctype string
		var notnull, pk int
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false
		}
		if strings.EqualFold(name, col) {
			return true
		}
	}
	return false
}

// WithWriter runs fn in a write transaction. SQLite allows one writer at a time, so its writes are
// serialized here (a second writer would otherwise fail with SQLITE_BUSY). TiDB runs transactions
// concurrently and locks rows itself: serializing them in the process would cap writes at one
// transaction's round trips, which against a cluster in another region is a handful per second.
// Code that reads and then writes the same rows locks them with SELECT ... FOR UPDATE on TiDB.
func (s *Store) WithWriter(fn func(*sql.Tx) error) error {
	return s.WithWriterContext(context.Background(), fn)
}

// WithWriterContext also bounds the wait for SQLite's writer and the transaction lifetime.
func (s *Store) WithWriterContext(ctx context.Context, fn func(*sql.Tx) error) error {
	if s.Backend != BackendTiDB {
		if ctx.Done() == nil {
			s.mu.Lock()
		} else {
			ticker := time.NewTicker(5 * time.Millisecond)
			defer ticker.Stop()
			for !s.mu.TryLock() {
				select {
				case <-ctx.Done():
					return ctx.Err()
				case <-ticker.C:
				}
			}
		}
		defer s.mu.Unlock()
	}
	tx, err := s.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Ping(ctx context.Context) error {
	return s.DB.PingContext(ctx)
}

func (s *Store) UpsertRevision(tx *sql.Tx, revision int, status, hash, content string, baseRevision int, createdAt string) error {
	if s.Backend == BackendTiDB {
		_, err := tx.Exec(`INSERT INTO config_revisions(revision, status, content_hash, content, base_revision, created_at)
			VALUES(?,?,?,?,?,?)
			ON DUPLICATE KEY UPDATE status=VALUES(status), content_hash=VALUES(content_hash), content=VALUES(content)`,
			revision, status, hash, content, baseRevision, createdAt)
		return err
	}
	_, err := tx.Exec(`INSERT INTO config_revisions(revision, status, content_hash, content, base_revision, created_at)
			VALUES(?,?,?,?,?,?)
			ON CONFLICT(revision) DO UPDATE SET status=excluded.status, content_hash=excluded.content_hash, content=excluded.content`,
		revision, status, hash, content, baseRevision, createdAt)
	return err
}

func (s *Store) UpsertPoll(tx *sql.Tx, id, scopeKey string, revision int, state, createdAt string) error {
	if s.Backend == BackendTiDB {
		_, err := tx.Exec(`INSERT INTO polls(id, scope_key, revision, state, created_at) VALUES(?,?,?,?,?)
			ON DUPLICATE KEY UPDATE revision=VALUES(revision), state=VALUES(state)`,
			id, scopeKey, revision, state, createdAt)
		return err
	}
	_, err := tx.Exec(`INSERT INTO polls(id, scope_key, revision, state, created_at) VALUES(?,?,?,?,?)
			ON CONFLICT(id) DO UPDATE SET revision=excluded.revision, state=excluded.state`,
		id, scopeKey, revision, state, createdAt)
	return err
}

func (s *Store) UpsertConsent(tx *sql.Tx, subject string, granted int, updatedAt string) error {
	if s.Backend == BackendTiDB {
		_, err := tx.Exec(`INSERT INTO consents(subject, granted, updated_at) VALUES(?,?,?)
			ON DUPLICATE KEY UPDATE granted=VALUES(granted), updated_at=VALUES(updated_at)`,
			subject, granted, updatedAt)
		return err
	}
	_, err := tx.Exec(`INSERT INTO consents(subject, granted, updated_at) VALUES(?,?,?)
			ON CONFLICT(subject) DO UPDATE SET granted=excluded.granted, updated_at=excluded.updated_at`,
		subject, granted, updatedAt)
	return err
}

func (s *Store) ListPollsQuery() string {
	return `SELECT state FROM polls ORDER BY created_at DESC, id DESC LIMIT 100`
}
