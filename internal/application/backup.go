package application

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/errcode"
	"masamong/internal/fsutil"
	"masamong/internal/launcher"
	"masamong/internal/storage"
)

// Backups hold config.yaml, storage.yaml and the database (SQLite: a VACUUM INTO copy; TiDB: a
// logical JSON dump) with checksums in manifest.json. Secrets are never included. Kinds: manual (a
// person pressed the button), auto (the schedule, pruned to backup.keep), prerestore (the safety
// copy taken before a restore), preupdate (before the console installs a new version) and upgrade
// (the first start of a new version, whichever way it was installed); the last two keep their newest
// three. A restore is staged: the daemon applies it on its next start, before
// it opens the database, and reports the result in backup.list.

const (
	backupManual     = "manual"
	backupAuto       = "auto"
	backupPreRestore = "prerestore"
	backupPreUpdate  = "preupdate" // before the console installs a new version
	backupUpgrade    = "upgrade"   // the first start of a new version
	restorePending   = "restore-pending.json"
	restoreResult    = "restore-result.json"
)

// backupDir is the only directory backup Actions read from or write to; paths outside it are refused
// so a GUI form can never turn backup.verify into an arbitrary file read (11장 §2).
func (s *Service) backupDir() string { return filepath.Join(s.DataRoot, "backups") }

func backupPathIn(dir, name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." || strings.ContainsAny(name, `/\`) || !strings.HasPrefix(name, "backup-") {
		return "", fmt.Errorf("backup name required")
	}
	if st, err := os.Lstat(dir); err != nil {
		return "", err
	} else if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return "", fmt.Errorf("backups path must be a real directory")
	}
	target := filepath.Join(dir, name)
	if st, err := os.Lstat(target); err == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
			return "", fmt.Errorf("backup must be a real directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return target, nil
}

func (s *Service) backupPath(name string) (string, error) { return backupPathIn(s.backupDir(), name) }

func backupKindOf(name string, m map[string]any) string {
	if k, ok := m["kind"].(string); ok && k != "" {
		return k
	}
	switch {
	case strings.Contains(name, "-auto"):
		return backupAuto
	case strings.Contains(name, "-prerestore"):
		return backupPreRestore
	case strings.Contains(name, "-preupdate"):
		return backupPreUpdate
	case strings.Contains(name, "-upgrade"):
		return backupUpgrade
	case strings.Contains(name, "-prereset"):
		return backupPreReset
	}
	return backupManual
}

// lockMaintenanceContext takes the in-process and cross-process maintenance locks, polling so that a
// cancelled ctx ends the wait. It is the one implementation; lockMaintenance is the uncancellable form.
func lockMaintenanceContext(ctx context.Context, root string, recovery bool) (func(), error) {
	for !maintenanceMu.TryLock() {
		timer := time.NewTimer(50 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	l, err := launcher.AcquireNamedLock(root, "maintenance.lock")
	if err != nil {
		maintenanceMu.Unlock()
		return nil, err
	}
	unlock := func() { l.Release(); maintenanceMu.Unlock() }
	if !recovery {
		for _, name := range []string{maintenancePending, restorePending, resetPending} {
			if _, err := os.Lstat(filepath.Join(root, name)); !os.IsNotExist(err) {
				unlock()
				return nil, fmt.Errorf("maintenance recovery required: %s", name)
			}
		}
	}
	if err := ctx.Err(); err != nil {
		unlock()
		return nil, err
	}
	return unlock, nil
}

// detachedWork gives a long, operator-started operation the service's lifetime instead of the HTTP
// request's: a console reload or a proxy dropping the connection must not abort a backup that has
// already run for minutes on a slow SD card. Only shutdown cancels it (beginWork). Waiting for the
// maintenance lock still follows the request, so a request that is given up never queues forever.
func (s *Service) detachedWork(ctx context.Context) (context.Context, func(), bool) {
	if ctx == nil {
		ctx = context.Background()
	}
	return s.beginWork(context.WithoutCancel(ctx))
}

func (s *Service) backup(ctx context.Context, req action.Request) action.Result {
	unlock, err := lockMaintenanceContext(ctx, s.DataRoot, req.Action == "backup.list")
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	defer unlock()
	dir := s.backupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	in := inputMap(req)
	switch req.Action {
	case "backup.list":
		return action.OK(req, s.backupList())
	case "backup.create":
		wctx, done, ok := s.detachedWork(ctx)
		if !ok {
			return action.Fail(req, errcode.IO, "generic.error", "service is shutting down", nil)
		}
		defer done()
		name, manifest, err := s.createBackupLockedContext(wctx, backupManual)
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		return action.OK(req, map[string]any{"name": name, "path": filepath.Join(dir, name), "manifest": manifest, "message": s.msg(req, "backup.created")})
	case "backup.delete":
		name := strVal(in, "name", "")
		target, err := s.backupPath(name)
		if err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
		}
		if _, err := os.Stat(filepath.Join(target, "manifest.json")); err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "no such backup: "+name, nil)
		}
		if err := os.RemoveAll(target); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		return action.OK(req, map[string]any{"deleted": name})
	case "backup.verify", "backup.restoreDryRun":
		name := strVal(in, "name", strVal(in, "path", ""))
		target, err := s.backupPath(name)
		if err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
		}
		m, err := verifyBackupDirContext(ctx, target, false)
		if err != nil {
			return action.Fail(req, errcode.BackupVerifyFailed, "generic.error", err.Error(), map[string]any{"name": name})
		}
		out := map[string]any{"ok": true, "name": name, "manifest": m}
		if req.Action == "backup.restoreDryRun" {
			out["applied"], out["dry_run"], out["message"] = false, true, s.msg(req, "backup.dry_run.ok")
			out["would_replace"] = []string{"database", "config.yaml"}
			out["would_keep"] = []string{"storage backend", "credential references", "secrets"}
			out["restore_scope"] = "database_config"
			out["method"] = restoreMethod(fmt.Sprint(m["db_file"]), s.Store.Backend)
			out["backup_revision"], out["current_revision"] = m["config_revision"], s.Revision()
		}
		return action.OK(req, out)
	case "backup.restore":
		if s.practice {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "restore is not available in practice mode", nil)
		}
		name := strVal(in, "name", "")
		target, err := s.backupPath(name)
		if err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", err.Error(), nil)
		}
		if strVal(in, "confirm", "") != name {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "confirm must repeat the backup name", nil)
		}
		wctx, done, ok := s.detachedWork(ctx)
		if !ok {
			return action.Fail(req, errcode.IO, "generic.error", "service is shutting down", nil)
		}
		defer done()
		if _, err := verifyBackupDirContext(wctx, target, false); err != nil {
			return action.Fail(req, errcode.BackupVerifyFailed, "generic.error", err.Error(), map[string]any{"name": name})
		}
		safety, _, err := s.createBackupLockedContext(wctx, backupPreRestore)
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", "safety backup failed, nothing was changed: "+err.Error(), nil)
		}
		j, err := newMaintenance(s.DataRoot, "restore")
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		j.Name, j.Safety = name, safety
		j.DBPath, err = filepath.Abs(s.Store.Path())
		if err == nil {
			err = validateMaintenance(&j)
		}
		if err == nil {
			err = saveMaintenance(filepath.Join(s.DataRoot, maintenancePending), &j, "requested")
		}
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		out := map[string]any{"pending": true, "name": name, "safety_backup": safety, "message": s.msg(req, "backup.restore.pending")}
		if s.Daemon && strVal(in, "restart", "true") != "false" {
			s.requestLifecycle(lifecycleRestart)
			out["restarting"] = true
		}
		return action.OK(req, out)
	}
	return action.Fail(req, errcode.ActionUnknown, "action.unknown", s.msg(req, "action.unknown"), nil)
}

func (s *Service) backupList() map[string]any {
	dir := s.backupDir()
	entries, _ := os.ReadDir(dir)
	list := []map[string]any{}
	var lastOK time.Time
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "backup-") {
			continue
		}
		item := map[string]any{"name": e.Name(), "kind": backupKindOf(e.Name(), nil)}
		if mb, err := os.ReadFile(filepath.Join(dir, e.Name(), "manifest.json")); err == nil {
			var m map[string]any
			if json.Unmarshal(mb, &m) == nil {
				item["created_at"], item["backend"], item["config_revision"], item["version"] = m["created_at"], m["backend"], m["config_revision"], m["version"]
				item["kind"] = backupKindOf(e.Name(), m)
				if f, ok := m["db_file"].(string); ok {
					if st, err := os.Stat(filepath.Join(dir, e.Name(), f)); err == nil {
						item["size_bytes"] = st.Size()
					}
				}
				if at, err := time.Parse(time.RFC3339, fmt.Sprint(m["created_at"])); err == nil && at.After(lastOK) {
					lastOK = at
				}
			}
		}
		list = append(list, item)
	}
	out := map[string]any{"backups": list, "dir": dir, "policy": s.Doc().Backup}
	if !lastOK.IsZero() {
		out["last_backup_at"] = lastOK.UTC().Format(time.RFC3339)
	}
	if next, ok := nextBackup(s.Clock.Now(), s.Doc().Backup, s.instanceZone()); ok {
		out["next_backup_at"] = next.UTC().Format(time.RFC3339)
	}
	var sched scheduleState
	if s.Store != nil {
		if ok, _ := s.kvGet("schedule:backup", &sched); ok {
			out["schedule_state"] = sched
		}
		var last map[string]any
		if ok, _ := s.kvGet("restore:last", &last); ok {
			out["last_restore"] = last
		}
	}
	if _, err := os.Stat(filepath.Join(s.DataRoot, maintenancePending)); err == nil {
		out["restore_pending"] = true
	}
	return out
}

// backupSpace refuses a backup that would leave the disk (nearly) full: on the disk that also holds
// the database, a full disk makes every write fail, so a scheduled backup must never cause one. It needs
// room for a copy of the database plus a margin.
func (s *Service) backupSpace(ctx context.Context, dir string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	var need uint64
	if s.Store.IsSQLite() {
		if st, err := os.Stat(s.Store.Path()); err == nil {
			need = uint64(st.Size())
		}
	} else {
		var n sql.NullFloat64
		_ = s.Store.DB.QueryRowContext(ctx, `SELECT SUM(DATA_LENGTH+INDEX_LENGTH) FROM information_schema.TABLES WHERE TABLE_SCHEMA=DATABASE()`).Scan(&n)
		need = uint64(n.Float64 * 1.5) // JSON with base64 vectors is larger than the stored rows
	}
	need += need/10 + 256<<20
	free, err := fsutil.FreeBytes(dir)
	if err != nil || free >= need {
		return nil // unknown free space: try anyway (the copy is removed if it fails)
	}
	return fmt.Errorf("not enough disk space for a backup: %d MB free, about %d MB needed", free>>20, need>>20)
}

// createBackup writes one backup and returns its name.
func (s *Service) createBackup(kind string) (string, map[string]any, error) {
	return s.createBackupContext(context.Background(), kind)
}

func (s *Service) createBackupContext(ctx context.Context, kind string) (string, map[string]any, error) {
	unlock, err := lockMaintenanceContext(ctx, s.DataRoot, false)
	if err != nil {
		return "", nil, err
	}
	defer unlock()
	return s.createBackupLockedContext(ctx, kind)
}

func (s *Service) createBackupLocked(kind string) (string, map[string]any, error) {
	return s.createBackupLockedContext(context.Background(), kind)
}

func (s *Service) createBackupLockedContext(ctx context.Context, kind string) (string, map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return "", nil, err
	}
	dir := s.backupDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", nil, err
	}
	name := "backup-" + s.Clock.Now().UTC().Format("20060102T150405Z")
	if err := s.backupSpace(ctx, dir); err != nil {
		return "", nil, err
	}
	target, err := os.MkdirTemp(dir, ".backup-staging-*")
	if err != nil {
		return "", nil, err
	}
	name += fmt.Sprintf("-%020d-", time.Now().UnixNano()) + strings.TrimPrefix(filepath.Base(target), ".backup-staging-")
	if kind != backupManual {
		name += "-" + kind
	}
	fail := func(err error) (string, map[string]any, error) {
		_ = os.RemoveAll(target)
		return "", nil, err
	}
	dbFile := "masamong.db"
	dbCopy := filepath.Join(target, dbFile)
	if s.Store.IsSQLite() {
		if err := s.Store.SnapshotTo(ctx, dbCopy); err != nil {
			return fail(fmt.Errorf("vacuum into: %w", err))
		}
	} else {
		dbFile = "dump.json"
		dbCopy = filepath.Join(target, dbFile)
		f, err := os.OpenFile(dbCopy, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			return fail(err)
		}
		err = s.Store.ExportLogicalTo(f)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return fail(err)
		}
	}
	// The snapshot's committed row is authoritative, even if a config writer commits
	// again while this backup is being assembled.
	if err := maintenancePoint("backup:snapshot"); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	cfg, revision, err := snapshotConfigContext(ctx, dbCopy, dbFile, true)
	if err != nil {
		return fail(err)
	}
	if err := os.WriteFile(filepath.Join(target, "config.yaml"), cfg, 0o600); err != nil {
		return fail(err)
	}
	if b, err := os.ReadFile(storage.SettingsPath(s.DataRoot)); err == nil {
		if err := fsutil.WriteDurable(filepath.Join(target, storage.SettingsName), b, 0o600); err != nil {
			return fail(err)
		}
	} else if !os.IsNotExist(err) {
		return fail(err)
	}
	dbSum, err := fileSHA256Context(ctx, dbCopy)
	if err != nil {
		return fail(err)
	}
	cfgSum := sha256.Sum256(cfg)
	manifest := map[string]any{
		"name": name, "kind": kind, "instance": s.Doc().Instance.ID, "schema": 1, "config_revision": revision,
		"config_sha256": hex.EncodeToString(cfgSum[:]), "db_sha256": dbSum, "db_file": dbFile,
		"backend": s.Store.Backend, "secrets_included": false, "restore_scope": "database_config",
		"created_at": s.Clock.Now().UTC().Format(time.RFC3339), "version": Version,
	}
	b, _ := json.MarshalIndent(manifest, "", "  ")
	if err := fsutil.WriteDurable(filepath.Join(target, "manifest.json"), b, 0o600); err != nil {
		return fail(err)
	}
	for _, p := range []string{dbCopy, filepath.Join(target, "config.yaml")} {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		f, err := os.OpenFile(p, os.O_RDWR, 0)
		if err != nil {
			return fail(err)
		}
		err = errors.Join(f.Sync(), f.Close())
		if err != nil {
			return fail(err)
		}
	}
	if _, err := verifyBackupDirContext(ctx, target, true); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if err := fsutil.SyncDir(target); err != nil {
		return fail(err)
	}
	if err := fsutil.RenameDurable(target, filepath.Join(dir, name)); err != nil {
		return fail(err)
	}
	return name, manifest, nil
}

// updateBackupsKept is how many preupdate and upgrade backups stay (each kind).
const updateBackupsKept = 3

func (s *Service) pruneKind(kind string, keep int) []string {
	unlock, err := lockMaintenance(s.DataRoot, false)
	if err != nil {
		return nil
	}
	defer unlock()
	return s.pruneKindLocked(kind, keep)
}

func (s *Service) pruneKindLocked(kind string, keep int) []string {
	entries, _ := os.ReadDir(s.backupDir())
	var auto []string
	for _, e := range entries {
		if e.IsDir() && strings.HasPrefix(e.Name(), "backup-") && backupKindOf(e.Name(), nil) == kind {
			auto = append(auto, e.Name())
		}
	}
	sort.Strings(auto) // names start with a UTC timestamp
	var removed []string
	for len(auto) > keep {
		if p, err := s.backupPath(auto[0]); err == nil && os.RemoveAll(p) == nil {
			removed = append(removed, auto[0])
		}
		auto = auto[1:]
	}
	return removed
}

// verifyBackupDir checks the manifest checksums of a backup folder.
func verifyBackupDir(target string) (map[string]any, error) {
	return verifyBackupDirContext(context.Background(), target, false)
}

// verifyBackupDirContext checks a backup folder. justWritten skips the two full-file passes (the
// database checksum and the integrity check) that createBackupLockedContext has just computed itself;
// the manifest, file kinds, config and revision checks always run.
func verifyBackupDirContext(ctx context.Context, target string, justWritten bool) (map[string]any, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if st, err := os.Lstat(target); err != nil || st.Mode()&os.ModeSymlink != 0 || !st.IsDir() {
		return nil, fmt.Errorf("backup must be a real directory")
	}
	regular := func(name string) (string, error) {
		path := filepath.Join(target, name)
		st, err := os.Lstat(path)
		if err != nil || st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return "", fmt.Errorf("%s must be a regular file", name)
		}
		return path, nil
	}
	manifestPath, err := regular("manifest.json")
	if err != nil {
		return nil, err
	}
	mb, err := os.ReadFile(manifestPath)
	if err != nil {
		return nil, fmt.Errorf("manifest not found: %s", filepath.Base(target))
	}
	var m map[string]any
	if err := json.Unmarshal(mb, &m); err != nil {
		return nil, fmt.Errorf("invalid manifest")
	}
	dbFile, _ := m["db_file"].(string)
	if dbFile == "" {
		dbFile = "masamong.db"
		m["db_file"] = dbFile
	}
	if dbFile != filepath.Base(dbFile) {
		return nil, fmt.Errorf("invalid manifest")
	}
	dbPath, err := regular(dbFile)
	if err != nil {
		return nil, err
	}
	if !justWritten {
		dbSum, err := fileSHA256Context(ctx, dbPath)
		if err != nil {
			return nil, err
		}
		if dbSum != fmt.Sprint(m["db_sha256"]) {
			return nil, fmt.Errorf("database checksum mismatch")
		}
	}
	configPath, err := regular("config.yaml")
	if err != nil {
		return nil, err
	}
	cfg, err := os.ReadFile(configPath)
	if err != nil {
		return nil, fmt.Errorf("config missing")
	}
	cfgSum := sha256.Sum256(cfg)
	if hex.EncodeToString(cfgSum[:]) != fmt.Sprint(m["config_sha256"]) {
		return nil, fmt.Errorf("config checksum mismatch")
	}
	if _, _, err := config.ParseCommitted(cfg); err != nil { // an old backup may hold an address today's policy refuses
		return nil, fmt.Errorf("invalid backup config: %w", err)
	}
	committed, rev, err := snapshotConfigContext(ctx, dbPath, dbFile, !justWritten)
	if err != nil {
		return nil, err
	}
	if string(cfg) != string(committed) || fmt.Sprint(m["config_revision"]) != fmt.Sprint(rev) {
		return nil, fmt.Errorf("backup config, database and manifest revision disagree")
	}
	return m, nil
}

// Validation opens snapshots read-only and without migrations. immutable prevents a
// VACUUM snapshot in WAL mode from creating sidecars or interpreting unrelated WALs.
func snapshotConfig(path, dbFile string) ([]byte, int, error) {
	return snapshotConfigContext(context.Background(), path, dbFile, true)
}

func snapshotConfigContext(ctx context.Context, path, dbFile string, checkIntegrity bool) ([]byte, int, error) {
	if err := ctx.Err(); err != nil {
		return nil, 0, err
	}
	if dbFile == "dump.json" {
		return validateLogicalSnapshot(path)
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, 0, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs), RawQuery: "mode=ro&immutable=1"}
	if !strings.HasPrefix(u.Path, "/") {
		u.Path = "/" + u.Path
	}
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, 0, err
	}
	defer db.Close()
	if checkIntegrity {
		rows, err := db.QueryContext(ctx, "PRAGMA integrity_check")
		if err != nil {
			return nil, 0, fmt.Errorf("SQLite integrity check: %w", err)
		}
		good := false
		for rows.Next() {
			if err := ctx.Err(); err != nil {
				rows.Close()
				return nil, 0, err
			}
			var result string
			if err := rows.Scan(&result); err != nil {
				rows.Close()
				return nil, 0, err
			}
			if result != "ok" {
				rows.Close()
				return nil, 0, fmt.Errorf("SQLite integrity check: %s", result)
			}
			good = true
		}
		err = errors.Join(rows.Err(), rows.Close())
		if err != nil || !good {
			return nil, 0, fmt.Errorf("SQLite integrity check did not pass: %v", err)
		}
	}
	// These tables/columns identify a product database, without requiring the latest
	// schema (older supported backups are migrated only after installation).
	for _, query := range []string{"SELECT version,applied_at FROM schema_migrations LIMIT 0", "SELECT id,type,status,payload FROM jobs LIMIT 0", "SELECT revision,status,content_hash,content,base_revision FROM config_revisions LIMIT 0"} {
		r, err := db.QueryContext(ctx, query)
		if err != nil {
			return nil, 0, fmt.Errorf("backup schema sanity: %w", err)
		}
		if err := r.Close(); err != nil {
			return nil, 0, err
		}
	}
	var rev int
	var content, hash string
	err = db.QueryRowContext(ctx, "SELECT revision,content,content_hash FROM config_revisions WHERE status='committed' ORDER BY revision DESC LIMIT 1").Scan(&rev, &content, &hash)
	if err != nil {
		return nil, 0, fmt.Errorf("backup committed configuration: %w", err)
	}
	if rev <= 0 || config.Hash([]byte(content)) != hash {
		return nil, 0, fmt.Errorf("invalid committed configuration hash")
	}
	if _, _, err := config.ParseCommitted([]byte(content)); err != nil {
		return nil, 0, err
	}
	return []byte(content), rev, nil
}

// Legacy TiDB dumps are imported into an isolated SQLite file and checked before
// any live file is moved. Import rejects unknown tables and invalid row shapes.
func validateLogicalSnapshot(path string) ([]byte, int, error) {
	dir, err := os.MkdirTemp("", "masamong-validate-*")
	if err != nil {
		return nil, 0, err
	}
	defer os.RemoveAll(dir)
	dst := filepath.Join(dir, "snapshot.db")
	if _, err := importSnapshot(path, dst); err != nil {
		return nil, 0, err
	}
	return snapshotConfig(dst, "masamong.db")
}

func importSnapshot(src, dst string) (int64, error) {
	f, err := os.Open(src)
	if err != nil {
		return 0, err
	}
	defer f.Close()
	s, err := openMigrationTarget(storage.Options{Backend: storage.BackendSQLite, SQLitePath: dst})
	if err != nil {
		return 0, err
	}
	if err := migrateMemory(s); err != nil {
		s.Close()
		return 0, err
	}
	if err := s.Clear(context.Background()); err != nil {
		s.Close()
		return 0, err
	}
	n, err := s.ImportLogicalFrom(context.Background(), f)
	if err == nil {
		_, err = s.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	}
	err = errors.Join(err, s.Close())
	if err == nil {
		f, openErr := os.OpenFile(dst, os.O_RDWR, 0)
		if openErr != nil {
			return n, openErr
		}
		err = errors.Join(f.Sync(), f.Close())
	}
	return n, err
}

// restoreMethod says how a backup would be loaded into the current backend.
func restoreMethod(dbFile, backend string) string {
	switch {
	case dbFile == "dump.json":
		return "import_dump"
	case backend == storage.BackendSQLite:
		return "replace_file"
	}
	return "copy_rows"
}

func fileSHA256(path string) (string, error) {
	return fileSHA256Context(context.Background(), path)
}

func fileSHA256Context(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	buf := make([]byte, 64*1024)
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, err := f.Read(buf)
		if n > 0 {
			if _, werr := h.Write(buf[:n]); werr != nil {
				return "", werr
			}
		}
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// atomicCopy creates only its own temporary file and flushes before publication.
func atomicCopy(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.CreateTemp(filepath.Dir(dst), ".maintenance-copy-*")
	if err != nil {
		return err
	}
	defer os.Remove(out.Name())
	_, err = io.Copy(out, in)
	if err == nil {
		err = out.Sync()
	}
	err = errors.Join(err, out.Close())
	if err != nil {
		return err
	}
	return fsutil.RenameDurable(out.Name(), dst)
}

// RestoreResult is what the last staged restore did (kept in restore-result.json until the service
// records it).
type RestoreResult struct {
	Name          string `json:"name"`
	Safety        string `json:"safety_backup"`
	Method        string `json:"method"`
	OK            bool   `json:"ok"`
	Error         string `json:"error,omitempty"`
	Rows          int64  `json:"rows,omitempty"`
	RollbackOK    bool   `json:"rollback_ok,omitempty"`
	RollbackError string `json:"rollback_error,omitempty"`
	// Warning is set when the restore succeeded but old files could not be deleted yet (another program
	// holds them); they are removed later and doctor lists them until then.
	Warning string `json:"warning,omitempty"`
	At      string `json:"at"`
}

// prepareRestore validates the source before preparing an owned replacement pair.
// Nothing live is moved until all copies, SQLite checks and the file plan are durable.
func prepareRestore(path string, j *maintenanceJournal) error {
	dir, err := backupPathIn(filepath.Join(j.Root, "backups"), j.Name)
	if err != nil {
		return err
	}
	m, err := verifyBackupDir(dir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(j.DBPath), 0o700); err != nil {
		return err
	}
	dbStage := recoveryName(j.DBPath, "new", j.ID)
	cfgTarget := filepath.Join(j.Root, "config.yaml")
	cfgStage := recoveryName(cfgTarget, "new", j.ID)
	// requested means no live move has happened. A previous interrupted preparation
	// can only have left these operation-owned copies.
	for _, p := range []string{dbStage, dbStage + "-wal", dbStage + "-shm", cfgStage} {
		if err := fsutil.RemoveDurable(p); err != nil {
			return err
		}
	}
	dbFile := fmt.Sprint(m["db_file"])
	j.Method = restoreMethod(dbFile, storage.BackendSQLite)
	if err := maintenancePoint("restore:before-copy"); err != nil {
		return err
	}
	if dbFile == "dump.json" {
		_, err = importSnapshot(filepath.Join(dir, dbFile), dbStage)
	} else {
		err = atomicCopy(filepath.Join(dir, dbFile), dbStage)
	}
	if err != nil {
		return err
	}
	if err := maintenancePoint("restore:copy"); err != nil {
		return err
	}
	if err := atomicCopy(filepath.Join(dir, "config.yaml"), cfgStage); err != nil {
		return err
	}
	if err := verifyRestorePair(dbStage, cfgStage); err != nil {
		return err
	}
	j.Entries = nil
	for _, target := range []string{j.DBPath, j.DBPath + "-wal", j.DBPath + "-shm", j.DBPath + "-journal", cfgTarget} {
		stage := ""
		if target == j.DBPath {
			stage = dbStage
		}
		if target == cfgTarget {
			stage = cfgStage
		}
		f, err := planFile(target, stage, j.ID)
		if err != nil {
			return err
		}
		j.Entries = append(j.Entries, f)
	}
	return saveMaintenance(path, j, "prepared")
}

// refuseRestore ends a restore that failed before any live file moved: the copies it staged go, and the
// journal is set aside (kept for inspection) so the next start is an ordinary one.
func refuseRestore(dataRoot, path string, j *maintenanceJournal) error {
	dbStage := recoveryName(j.DBPath, "new", j.ID)
	for _, p := range []string{dbStage, dbStage + "-wal", dbStage + "-shm", recoveryName(filepath.Join(j.Root, "config.yaml"), "new", j.ID)} {
		_ = os.Remove(p)
	}
	return moveJournalAside(dataRoot, path)
}

func verifyRestorePair(dbPath, cfgPath string) error {
	committed, _, err := snapshotConfig(dbPath, "masamong.db")
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	if _, _, err := config.ParseCommitted(raw); err != nil {
		return err
	}
	if string(raw) != string(committed) {
		return fmt.Errorf("restored database and config disagree")
	}
	return nil
}

// ApplyPendingRestore reconciles durable intent before the live database is opened.
// A failed rollback retains the journal and every remaining recovery file.
func ApplyPendingRestore(dataRoot string, log *slog.Logger) *RestoreResult {
	unlock, err := lockMaintenance(dataRoot, true)
	if err != nil {
		return &RestoreResult{Error: err.Error()}
	}
	defer unlock()
	return applyPendingRestoreLocked(dataRoot, log)
}

func applyPendingRestoreLocked(dataRoot string, log *slog.Logger) *RestoreResult {
	path := filepath.Join(dataRoot, maintenancePending)
	j, err := readMaintenance(path, dataRoot)
	if os.IsNotExist(err) {
		if _, legacy := os.Lstat(filepath.Join(dataRoot, restorePending)); os.IsNotExist(legacy) {
			return nil
		}
		err = fmt.Errorf("legacy restore marker requires inspection or cancellation with masamongd --maintenance cancel")
	}
	if err == nil && j.Kind != "restore" {
		return nil
	}
	res := &RestoreResult{At: time.Now().UTC().Format(time.RFC3339)}
	wasRollback := j != nil && (j.Phase == "rollback" || j.Phase == "rolled_back")
	if j != nil {
		res.Name, res.Safety, res.Method = j.Name, j.Safety, j.Method
	}
	if err == nil {
		if j.Phase == "requested" {
			err = prepareRestore(path, j)
			// Preparation touches no live file, so a failure (the backup folder was removed, the disk
			// is full, a copy failed) needs no recovery: the intent is cleared, the failure is recorded
			// for the console and doctor, and the bot keeps running on the current data. Only a process
			// that died mid-preparation (errMaintenanceInterrupted) leaves the journal to be resumed.
			if err != nil && !errors.Is(err, errMaintenanceInterrupted) {
				if refuseErr := refuseRestore(dataRoot, path, j); refuseErr != nil {
					err = errors.Join(err, refuseErr)
				} else {
					res.RollbackOK = true
					res.Error = err.Error()
					log.Error("restore could not start; nothing was changed", "backup", j.Name, "err", err)
					if b, merr := json.Marshal(res); merr == nil {
						_ = fsutil.WriteDurable(filepath.Join(dataRoot, restoreResult), b, 0o600)
					}
					return res
				}
			}
		}
		if err == nil {
			err = installPair(path, j, func() error { return verifyRestorePair(j.DBPath, filepath.Join(j.Root, "config.yaml")) })
			if err != nil && !errors.Is(err, errMaintenanceInterrupted) && j.Phase != "committed" && j.Phase != "rolled_back" {
				rb := rollbackPair(path, j)
				res.RollbackOK = rb == nil
				if rb != nil {
					res.RollbackError = rb.Error()
				}
			}
		}
		res.Method = j.Method
	}
	if err != nil {
		res.Error = err.Error()
		log.Error("restore recovery incomplete", "err", err, "rollback_error", res.RollbackError)
	} else if wasRollback {
		res.RollbackOK = true
		res.Error = "restore rolled back to the original database and config"
	} else {
		res.OK = true
		if left := recoveryLeftovers(dataRoot); len(left) > 0 {
			res.Warning = fmt.Sprintf("%d old file(s) could not be deleted yet and are removed later", len(left))
		}
	}
	b, _ := json.Marshal(res)
	if writeErr := fsutil.WriteDurable(filepath.Join(dataRoot, restoreResult), b, 0o600); writeErr != nil {
		res.Error = errors.Join(err, writeErr).Error()
		res.OK = false
	}
	return res
}

// noteRestoreResult moves restore-result.json into the change log and admin state.
func (s *Service) noteRestoreResult() {
	path := filepath.Join(s.DataRoot, restoreResult)
	raw, err := os.ReadFile(path)
	if err != nil || s.Store == nil {
		return
	}
	var r RestoreResult
	if json.Unmarshal(raw, &r) != nil {
		_ = os.Remove(path)
		return
	}
	if err := s.kvSet("restore:last", r); err != nil {
		return
	}
	_ = os.Remove(path)
	db, _ := s.adminDB()
	ok := 0
	if r.OK {
		ok = 1
	}
	_, _ = db.DB.Exec(`INSERT INTO audit_log(at, action, via, ok, code, rev_before, rev_after, summary, actor) VALUES(?,?,?,?,?,?,?,?,?)`,
		r.At, "backup.restore.applied", "service", ok, ternary(r.OK, "", "IO"), 0, s.Revision(),
		clip(fmt.Sprintf("name=%s method=%s safety=%s", r.Name, r.Method, r.Safety), 900), "")
}
