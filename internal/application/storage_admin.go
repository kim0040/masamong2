package application

import (
	"context"
	"os"
	"path/filepath"
	"strings"

	"masamong/internal/action"
	"masamong/internal/errcode"
	memstore "masamong/internal/memory/store"
	"masamong/internal/secrets"
	"masamong/internal/storage"
	"masamong/internal/tools"
)

// storage.status shows the instance's database. storage.migrate moved it between SQLite and TiDB (or to
// a fresh SQLite file / another TiDB database); it is disabled together with the TiDB backend
// (storage.TiDBEnabled explains why) and the catalog refuses it before this code runs. It is kept so
// both can be switched back on together.
// A migration never touches the source: it copies into an empty target, verifies every table, and only
// then points storage.yaml at the target. The switch takes effect on the next start; the previous
// storage.yaml is kept next to it for rollback.

func (s *Service) storageActions(ctx context.Context, req action.Request) action.Result {
	switch req.Action {
	case "storage.status":
		return s.storageStatus(ctx, req)
	case "storage.migrate":
		return s.storageMigrate(ctx, req)
	}
	return action.Fail(req, errcode.ActionUnknown, "action.unknown", s.msg(req, "action.unknown"), nil)
}

func (s *Service) storageStatus(ctx context.Context, req action.Request) action.Result {
	tables, err := s.Store.Tables()
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
	}
	counts := map[string]int64{}
	var total int64
	for _, t := range tables {
		n, err := s.Store.CountRead(ctx, t)
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		counts[t] = n
		total += n
	}
	out := map[string]any{"backend": s.Store.Backend, "location": s.Store.Path(), "tables": counts, "rows": total}
	if s.Store.IsSQLite() {
		if fi, err := os.Stat(s.Store.Path()); err == nil {
			out["size_bytes"] = fi.Size()
		}
	}
	if file, err := storage.LoadSettings(s.DataRoot); err == nil {
		out["configured_backend"] = file.Backend
		out["restart_pending"] = file.Backend != s.Store.Backend || (file.Backend == storage.BackendSQLite &&
			filepath.Join(s.DataRoot, orDefault(file.SQLiteFile, "masamong.db")) != s.Store.Path())
	}
	return action.OK(req, out)
}

func orDefault(v, d string) string {
	if v == "" {
		return d
	}
	return v
}

func (s *Service) storageMigrate(ctx context.Context, req action.Request) action.Result {
	in := inputMap(req)
	to, err := storage.NormalizeBackend(strVal(in, "to", ""))
	if err != nil || strVal(in, "to", "") == "" {
		return action.Fail(req, errcode.InvalidInput, "generic.error", "to must be sqlite or tidb", nil)
	}
	target := storage.Settings{Backend: to}
	opt := storage.Options{Backend: to}
	switch to {
	case storage.BackendSQLite:
		name := strVal(in, "sqlite_file", "masamong-"+s.Clock.Now().UTC().Format("20060102T150405Z")+".db")
		if name != filepath.Base(name) || !strings.HasSuffix(name, ".db") {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "sqlite_file must be a file name ending in .db inside the data root", nil)
		}
		opt.SQLitePath = filepath.Join(s.DataRoot, name)
		if opt.SQLitePath == s.Store.Path() {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "target is the current database", nil)
		}
		if _, err := os.Stat(opt.SQLitePath); err == nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "sqlite_file already exists: "+name, nil)
		}
		target.SQLiteFile = name
	case storage.BackendTiDB:
		ref := strVal(in, "dsn_ref", "")
		if !secrets.ValidRef(ref) {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "dsn_ref must be "+secrets.Hint+" (never the DSN itself)", nil)
		}
		dsn, err := storage.ResolveDSN(ref)
		if err != nil {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "dsn_ref: "+err.Error(), nil)
		}
		if s.Store.Backend == storage.BackendTiDB && storage.RedactDSN(dsn) == s.Store.Path() {
			return action.Fail(req, errcode.InvalidInput, "generic.error", "target is the current database", nil)
		}
		opt.DSN, opt.DSNRef = dsn, ref
		target.DSNRef = ref
		// Look before touching: a database that holds another program's tables (for example the
		// previous bot's) is refused before any table is created in it.
		existing, err := storage.InspectTiDB(dsn)
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", "target: "+err.Error(), map[string]any{"to": to})
		}
		ours, err := s.productTables()
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		var foreign []string
		for _, t := range existing {
			if !ours[t] {
				foreign = append(foreign, t)
			}
		}
		if len(foreign) > 0 {
			return action.Fail(req, errcode.InvalidInput, "storage.migrate.foreign", s.msg(req, "storage.migrate.foreign", "n", len(foreign)),
				map[string]any{"foreign_tables": foreign[:min(len(foreign), 5)], "foreign_count": len(foreign)})
		}
		if len(existing) == 0 && !boolVal(in, "confirm") {
			// an empty database stays empty until the move itself runs
			plan, err := s.planIntoEmpty(ctx)
			if err != nil {
				return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
			}
			return action.OK(req, map[string]any{"dry_run": true, "applied": false, "target_empty": true,
				"from": map[string]any{"backend": s.Store.Backend, "location": s.Store.Path()},
				"to":   map[string]any{"backend": to, "location": storage.RedactDSN(dsn)}, "plan": plan, "ready": true,
				"message": s.msg(req, "storage.migrate.plan")})
		}
	}
	confirm := boolVal(in, "confirm")
	// A dry run against SQLite must not leave an empty file behind.
	if !confirm && to == storage.BackendSQLite {
		defer os.Remove(opt.SQLitePath)
		defer os.Remove(opt.SQLitePath + "-wal")
		defer os.Remove(opt.SQLitePath + "-shm")
	}
	dst, err := openMigrationTarget(opt)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", "target: "+err.Error(), map[string]any{"to": to})
	}
	defer dst.Close()
	from := map[string]any{"backend": s.Store.Backend, "location": s.Store.Path()}
	dest := map[string]any{"backend": to, "location": dst.Path()}
	if !confirm {
		plan, err := storage.PlanTransfer(ctx, s.Store, dst)
		if err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), nil)
		}
		return action.OK(req, map[string]any{"dry_run": true, "applied": false, "from": from, "to": dest, "plan": plan,
			"ready": plan.Ready(), "message": s.msg(req, "storage.migrate.plan")})
	}
	plan, err := storage.Transfer(ctx, s.Store, dst, nil)
	if err != nil {
		return action.Fail(req, errcode.IO, "generic.error", err.Error(), map[string]any{"plan": plan, "switched": false})
	}
	if !plan.Verified() {
		return action.Fail(req, errcode.IO, "storage.migrate.mismatch", s.msg(req, "storage.migrate.mismatch"), map[string]any{"plan": plan, "switched": false})
	}
	out := map[string]any{"dry_run": false, "from": from, "to": dest, "plan": plan, "verified": true, "switched": false}
	if strVal(in, "switch", "true") != "false" {
		prev := storage.SettingsPath(s.DataRoot) + ".before-" + s.Clock.Now().UTC().Format("20060102T150405Z")
		if raw, err := os.ReadFile(storage.SettingsPath(s.DataRoot)); err == nil {
			if err := os.WriteFile(prev, raw, 0o600); err != nil {
				return action.Fail(req, errcode.IO, "generic.error", err.Error(), out)
			}
			out["previous_settings"] = filepath.Base(prev)
		}
		if err := storage.SaveSettings(s.DataRoot, target); err != nil {
			return action.Fail(req, errcode.IO, "generic.error", err.Error(), out)
		}
		out["switched"], out["restart_required"] = true, true
	}
	out["message"] = s.msg(req, "storage.migrate.done")
	return action.OK(req, out)
}

// openMigrationTarget opens (and migrates) every table family the product defines, so the plan can
// compare columns and the copy lands in the backend's own DDL.
func openMigrationTarget(opt storage.Options) (*storage.Store, error) {
	dst, err := storage.OpenOptions(opt)
	if err != nil {
		return nil, err
	}
	if _, err := memstore.Open(dst); err != nil {
		_ = dst.Close()
		return nil, err
	}
	if _, err := tools.OpenUsage(dst); err != nil {
		_ = dst.Close()
		return nil, err
	}
	if err := migrateAdmin(dst); err != nil {
		_ = dst.Close()
		return nil, err
	}
	return dst, nil
}

// productTables is every table this product defines (the current database has them all migrated).
func (s *Service) productTables() (map[string]bool, error) {
	// table families created on first use exist on the source from now on (idempotent DDL, our own DB)
	if _, err := memstore.Open(s.Store); err != nil {
		return nil, err
	}
	if _, err := tools.OpenUsage(s.Store); err != nil {
		return nil, err
	}
	if _, err := s.adminDB(); err != nil {
		return nil, err
	}
	tables, err := s.Store.Tables()
	if err != nil {
		return nil, err
	}
	out := map[string]bool{"schema_migrations": true}
	for _, t := range tables {
		out[t] = true
	}
	return out, nil
}

// planIntoEmpty is the plan for an empty target: every source table is copied in full.
func (s *Service) planIntoEmpty(ctx context.Context) (storage.TransferPlan, error) {
	var plan storage.TransferPlan
	tables, err := s.Store.Tables()
	if err != nil {
		return plan, err
	}
	for _, t := range tables {
		n, err := s.Store.Count(ctx, t)
		if err != nil {
			return plan, err
		}
		plan.Tables = append(plan.Tables, storage.TableCopy{Table: t, Rows: n})
		plan.Rows += n
	}
	return plan, nil
}
