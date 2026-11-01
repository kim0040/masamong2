package application

import (
	"context"
	"os"
	"time"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/fsutil"
	"masamong/internal/lang"
	"masamong/internal/storage"
)

func (s *Service) setup(_ context.Context, req action.Request) action.Result {
	// one setup at a time, and never interleaved with a config change (a double-clicked button)
	s.cfgMu.Lock()
	defer s.cfgMu.Unlock()
	in := inputMap(req)
	instance := strVal(in, "instance", "masamo")
	backend := strVal(in, "backend", strVal(in, "db_backend", ""))
	dsnRef := strVal(in, "dsn_ref", "")
	if err := fsutil.PrivateDir(s.DataRoot); err != nil {
		return action.Fail(req, "IO", "generic.error", err.Error(), nil)
	}
	file, err := storage.LoadSettings(s.DataRoot)
	if err != nil {
		return action.Fail(req, "IO", "generic.error", err.Error(), nil)
	}
	if backend != "" {
		norm, err := storage.NormalizeBackend(backend)
		if err != nil {
			return action.Fail(req, "IO", "generic.error", err.Error(), nil)
		}
		file.Backend = norm
	}
	if dsnRef != "" {
		file.DSNRef = dsnRef
	}
	if file.Backend == storage.BackendTiDB && file.DSNRef == "" {
		file.DSNRef = "env:MASAMONG_TIDB_DSN"
	}
	if err := storage.SaveSettings(s.DataRoot, file); err != nil {
		return action.Fail(req, "IO", "generic.error", err.Error(), nil)
	}
	already := false
	if s.Store == nil {
		opt, err := storage.OptionsFromEnv(s.DataRoot, storage.Settings{})
		if err != nil {
			return action.Fail(req, "IO", "generic.error", err.Error(), nil)
		}
		if opt.Backend == storage.BackendSQLite {
			if _, err := os.Stat(opt.SQLitePath); err == nil {
				already = true
			}
		}
		store, err := storage.OpenOptions(opt)
		if err != nil {
			return action.Fail(req, "IO", "generic.error", err.Error(), nil)
		}
		if err := migrateMemory(store); err != nil {
			_ = store.Close()
			return action.Fail(req, "IO", "generic.error", err.Error(), nil)
		}
		s.Store = store
	} else {
		already = s.Revision() > 0
	}
	if s.Revision() == 0 {
		doc := config.Default(instance, s.DataRoot)
		// the bot's language: the one the wizard asked for, else the language the console is in. New
		// rooms answer in it, their default persona is the built-in one of that language, and memory
		// reads conversations with that language's pack.
		for _, tag := range []string{strVal(in, "locale", ""), req.Locale} {
			if tag = lang.Normalize(tag); tag != "" {
				if _, ok := lang.Get(tag); ok {
					doc.I18n.DefaultLocale = tag
					doc.Memory.Languages = []string{tag}
					break
				}
			}
		}
		// the time zone of the browser the wizard runs in, when it is a real one (reminders, "today",
		// imported chat times without a zone)
		if tz := strVal(in, "timezone", ""); tz != "" && tz != "Local" {
			if _, err := time.LoadLocation(tz); err == nil {
				doc.I18n.Timezone, doc.Memory.NaiveTimezone = tz, tz
			}
		}
		raw, err := config.Marshal(doc)
		if err != nil {
			return action.Fail(req, "IO", "generic.error", err.Error(), nil)
		}
		rev := config.Revision{Revision: 1, Status: config.StatusCommitted, ContentHash: config.Hash(raw), Content: raw, BaseRevision: 0}
		if err := persistRevision(s.Store, rev); err != nil {
			return action.Fail(req, "IO", "generic.error", err.Error(), nil)
		}
		if err := atomicWriteFile(s.CfgPath, raw); err != nil {
			return action.Fail(req, "IO", "generic.error", err.Error(), nil)
		}
		s.setDoc(doc, 1)
		_ = s.kvSet("runtime:version", Version) // a new instance: its first start is not an upgrade (BackupOnUpgrade)
	} else if s.Registry().Quota.Store == nil {
		s.setDoc(s.Doc(), s.Revision()) // opened before setup: quota counting can use the database only now
	}
	key := s.msg(req, "setup.complete")
	if already && s.Revision() >= 1 {
		key = s.msg(req, "setup.already")
	}
	out := map[string]any{
		"instance":   s.Doc().Instance.ID,
		"profile":    s.Doc().Limits.Profile,
		"revision":   s.Revision(),
		"data_root":  s.DataRoot,
		"storage":    s.Store.Backend,
		"message":    key,
		"idempotent": true,
	}
	for k, v := range redactedEnvNote() {
		out[k] = v
	}
	res := action.OK(req, out)
	res.Revision = s.Revision()
	return res
}
