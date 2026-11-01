package storage

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"go.yaml.in/yaml/v3"

	"masamong/internal/fsutil"
	"masamong/internal/secrets"
)

const (
	BackendSQLite = "sqlite"
	BackendTiDB   = "tidb"
	SettingsName  = "storage.yaml"
)

// TiDBEnabled gates the TiDB (MySQL protocol) backend, and it is off.
//
// Why (2026-09-28, operator decision): supporting two databases doubled what had to be tested and
// looked after — per-backend DDL and upserts, a migration wizard, TiDB-only locking and backup paths,
// a live-server CI job — for a personal bot that runs fine on one SQLite file. The product is SQLite
// only for now. The TiDB code (tidb.go, the BackendTiDB branches in the packages that write SQL, the
// live tests) is kept so it can be switched back on by flipping this constant, but while it is false
// no path — storage.yaml, MASAMONG_DB_BACKEND, `masamongd -db-backend`, setup — can open TiDB.
//
// An instance that used TiDB moves over through a backup: a TiDB backup is a logical dump
// (dump.json), and restoring it into a SQLite instance still works (backup.go, import_dump).
const TiDBEnabled = false

// ErrTiDBDisabled is returned wherever a TiDB backend is requested while TiDBEnabled is false.
var ErrTiDBDisabled = errors.New("the TiDB backend is disabled: this version runs on SQLite only. " +
	"To move a TiDB instance, create a backup with the previous version, remove backend: tidb from storage.yaml, " +
	"start this version and restore that backup from Maintenance")

// Settings lives next to the instance data-root, not in the SQL config_revisions
// table, so the process can open the right engine before the DB exists.
type Settings struct {
	Backend string `yaml:"backend"`
	// DSNRef is env:NAME, file:PATH or keychain:NAME. Never a password. TiDB only.
	DSNRef string `yaml:"dsn_ref,omitempty"`
	// SQLiteFile is relative to data-root. Empty means masamong.db.
	SQLiteFile string `yaml:"sqlite_file,omitempty"`
}

type Options struct {
	Backend    string
	SQLitePath string
	DSN        string
	DSNRef     string
}

func NormalizeBackend(v string) (string, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", BackendSQLite, "local", "local-lite":
		return BackendSQLite, nil
	case BackendTiDB, "mysql", "tidb-mysql":
		if !TiDBEnabled {
			return "", ErrTiDBDisabled
		}
		return BackendTiDB, nil
	default:
		return "", fmt.Errorf("unknown storage backend %q (sqlite)", v)
	}
}

func SettingsPath(dataRoot string) string {
	return filepath.Join(dataRoot, SettingsName)
}

func LoadSettings(dataRoot string) (Settings, error) {
	raw, err := os.ReadFile(SettingsPath(dataRoot))
	if err != nil {
		if os.IsNotExist(err) {
			return Settings{Backend: BackendSQLite}, nil
		}
		return Settings{}, err
	}
	var s Settings
	if err := yaml.Unmarshal(raw, &s); err != nil {
		return Settings{}, fmt.Errorf("storage.yaml: %w", err)
	}
	backend, err := NormalizeBackend(s.Backend)
	if err != nil {
		return Settings{}, err
	}
	s.Backend = backend
	if s.DSNRef != "" && !secrets.ValidRef(s.DSNRef) {
		return Settings{}, fmt.Errorf("storage.dsn_ref must be %s", secrets.Hint)
	}
	return s, nil
}

func SaveSettings(dataRoot string, s Settings) error {
	backend, err := NormalizeBackend(s.Backend)
	if err != nil {
		return err
	}
	s.Backend = backend
	if err := fsutil.PrivateDir(dataRoot); err != nil {
		return err
	}
	raw, err := yaml.Marshal(s)
	if err != nil {
		return err
	}
	return fsutil.WriteAtomic(SettingsPath(dataRoot), raw, 0o600)
}

func ResolveDSN(ref string) (string, error) {
	if ref == "" {
		ref = "env:MASAMONG_TIDB_DSN"
	}
	if !secrets.ValidRef(ref) {
		return "", fmt.Errorf("dsn_ref must be %s", secrets.Hint)
	}
	v, err := secrets.Lookup(ref)
	if errors.Is(err, secrets.ErrMissing) {
		return "", fmt.Errorf("%s is empty", ref)
	}
	return v, err
}

func OptionsFromEnv(dataRoot string, override Settings) (Options, error) {
	file, err := LoadSettings(dataRoot)
	if err != nil {
		return Options{}, err
	}
	if override.Backend != "" {
		file.Backend = override.Backend
	}
	if override.DSNRef != "" {
		file.DSNRef = override.DSNRef
	}
	if v := os.Getenv("MASAMONG_DB_BACKEND"); v != "" && override.Backend == "" {
		b, err := NormalizeBackend(v)
		if err != nil {
			return Options{}, err
		}
		file.Backend = b
	}
	backend, err := NormalizeBackend(file.Backend)
	if err != nil {
		return Options{}, err
	}
	opt := Options{Backend: backend, DSNRef: file.DSNRef}
	if backend == BackendSQLite {
		name := file.SQLiteFile
		if name == "" {
			name = "masamong.db"
		}
		opt.SQLitePath = filepath.Join(dataRoot, name)
		return opt, nil
	}
	dsn, err := ResolveDSN(file.DSNRef)
	if err != nil {
		return opt, err
	}
	opt.DSN = dsn
	return opt, nil
}

// RedactDSN hides userinfo so logs and doctor never print a password.
func RedactDSN(dsn string) string {
	dsn = strings.TrimSpace(dsn)
	if dsn == "" {
		return ""
	}
	if strings.Contains(dsn, "://") {
		u, err := url.Parse(dsn)
		if err == nil {
			if u.User != nil {
				u.User = url.User("***")
			}
			return u.Redacted()
		}
	}
	if at := strings.LastIndex(dsn, "@"); at > 0 {
		return "***@" + dsn[at+1:]
	}
	return "(set)"
}
