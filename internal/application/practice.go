package application

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"masamong/internal/action"
	"masamong/internal/clock"
	"masamong/internal/config"
	"masamong/internal/i18n"
	"masamong/internal/lang"
	"masamong/internal/providers/practice"
	"masamong/internal/storage"
	"masamong/internal/tools"
)

// Practice mode is a second, separate instance in <data-root>/practice that the console switches to.
// It has its own SQLite file, uses the offline practice model (no network, no cost), never connects to
// Discord and cannot touch the operating system (restart, autostart, restore, storage moves, admins).
// It starts with a fictional sample conversation imported and indexed, so every page — rooms, tools,
// sources, index, lab, memory — can be tried without affecting the real bot.

//go:embed practice/*.csv
var practiceFS embed.FS

const (
	PracticeGuild   = "practice"
	PracticeChannel = "lounge"
	practiceRoom    = "kakao:practice"
)

// PracticeRoot is where the practice instance lives.
func PracticeRoot(dataRoot string) string { return filepath.Join(dataRoot, "practice") }

// Practice reports whether this service is the practice instance.
func (s *Service) Practice() bool { return s.practice }

// importPath keeps practice imports inside the practice instance. The real instance deliberately
// accepts operator-selected server paths, but practice mode must never be able to read them.
func (s *Service) importPath(path string) (string, error) {
	if !s.practice {
		return path, nil
	}
	root, err := filepath.EvalSymlinks(filepath.Join(s.DataRoot, "imports"))
	if err != nil {
		return "", fmt.Errorf("practice imports folder: %w", err)
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	rel, err := filepath.Rel(root, resolved)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || filepath.IsAbs(rel) {
		return "", fmt.Errorf("practice imports must be uploaded into the practice instance")
	}
	return resolved, nil
}

// practiceDoc is the practice instance's configuration.
func practiceDoc(root, locale string) config.Document {
	pack, ok := lang.Get(locale)
	if !ok {
		locale = "en"
	} else {
		locale = pack.Tag
	}
	doc := config.Default("practice", root)
	doc.Instance.Profile = "practice"
	doc.I18n.DefaultLocale = locale
	doc.Providers = map[string]config.Provider{"practice": {Kind: config.KindPractice, BaseURL: practice.BaseURL}}
	doc.Models = map[string]config.Model{
		"response": {Provider: "practice", Model: "practice/chat"},
		"planner":  {Provider: "practice", Model: "practice/chat"},
		"summary":  {Provider: "practice", Model: "practice/chat"},
		"embed":    {Provider: "practice", Model: "practice/embed", Dimensions: practice.Dims},
		"rerank":   {Provider: "practice", Model: "practice/rerank"},
	}
	doc.Memory.Summaries = false
	doc.Memory.Retrieve = "auto"
	doc.Tools = practiceToolDefaults(doc.Tools)
	if _, ok := lang.Get(locale); ok {
		doc.Memory.Languages = []string{locale}
	}
	doc.Backup.Schedule = "off"
	doc.Discord.Enabled = false
	persona := config.DefaultPersonaCandidates(locale, doc.I18n.PoliteDefault)[0]
	if _, ok := config.BuiltinPersonas()[persona]; !ok {
		persona = "friendly_en"
	}
	doc.Guilds = map[string]config.GuildSettings{PracticeGuild: {Locale: locale, Channels: map[string]config.ChannelPolicy{
		PracticeChannel: {Respond: "always", PersonaRef: persona, MemoryUse: "on", Logging: config.ChannelLogging{Level: "ops", RetainDays: 30}},
	}}}
	return doc
}

// practiceToolDefaults keeps upgrades of an existing practice instance offline and usable too. An
// operator can still replace these values inside practice mode; only missing defaults are supplied.
func practiceToolDefaults(settings map[string]tools.Settings) map[string]tools.Settings {
	out := make(map[string]tools.Settings, len(settings)+2)
	for id, setting := range settings {
		out[id] = setting
	}
	add := func(id string, defaults map[string]any) {
		setting := out[id]
		if setting.Defaults == nil {
			setting.Defaults = defaults
		}
		out[id] = setting
	}
	add("weather.query", map[string]any{"fixture": "ok", "region": "서울"})
	add("disasters.query", map[string]any{"fixture": "none"})
	return out
}

// OpenPractice opens (and on first use creates and seeds) the practice instance under dataRoot.
func OpenPractice(ctx context.Context, dataRoot, locale string, log *slog.Logger) (*Service, error) {
	if pack, ok := lang.Get(locale); ok {
		locale = pack.Tag
	} else {
		locale = "en"
	}
	root := PracticeRoot(dataRoot)
	if err := os.MkdirAll(root, 0o700); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	// always SQLite, whatever the real instance or the environment uses
	store, err := storage.OpenOptions(storage.Options{Backend: storage.BackendSQLite, SQLitePath: filepath.Join(root, "masamong.db")})
	if err != nil {
		return nil, err
	}
	s := &Service{DataRoot: root, Clock: clock.Real{}, I18n: i18n.New(), CfgPath: filepath.Join(root, "config.yaml"),
		WebBuild: true, watchers: map[chan Event]struct{}{}, practice: true, Store: store}
	// its lines still reach the daemon's log file, tagged, but not the real instance's console log
	s.Log = s.CaptureLogs(log.With("instance", "practice"))
	s.cfg.Store(&configSnapshot{reg: buildRegistry(offlineProviders(config.Default("", root)), s.Log, s.Clock.Now)})
	if err := s.reloadConfig(); err != nil {
		_ = s.Close()
		return nil, err
	}
	if s.Revision() == 0 {
		raw, err := config.Marshal(practiceDoc(root, locale))
		if err != nil {
			_ = s.Close()
			return nil, err
		}
		rev := config.Revision{Revision: 1, Status: config.StatusCommitted, ContentHash: config.Hash(raw), Content: raw}
		if err := persistRevision(store, rev); err != nil {
			_ = s.Close()
			return nil, err
		}
		if err := atomicWriteFile(s.CfgPath, raw); err != nil {
			_ = s.Close()
			return nil, err
		}
		if err := s.reloadConfig(); err != nil {
			_ = s.Close()
			return nil, err
		}
	}
	s.samplePracticeLogs()
	if err := s.seedPractice(ctx); err != nil {
		s.Log.Warn("practice seed", "err", err)
	}
	return s, nil
}

// samplePracticeLogs puts a few typical service lines, marked example=practice, at the start of the
// practice console's log, so its level filter, search and grouping have something to show before
// anything happened. The real instance never gets them.
func (s *Service) samplePracticeLogs() {
	now := s.Clock.Now().UTC()
	lines := []struct {
		ago   time.Duration
		level string
		msg   string
		attrs map[string]string
	}{
		{9 * time.Minute, "info", "discord connected", map[string]string{"guilds": "2"}},
		{9 * time.Minute, "info", "memory: index loaded", map[string]string{"rooms": "1", "leaves": "212"}},
		{7 * time.Minute, "info", "chat reply", map[string]string{"channel": PracticeChannel, "ms": "1840", "tools": "memory.search"}},
		{6 * time.Minute, "warn", "provider slow", map[string]string{"provider": "openrouter", "ms": "9200"}},
		{5 * time.Minute, "error", "provider call failed", map[string]string{"provider": "kma", "code": "PROVIDER_UNAVAILABLE"}},
		{4 * time.Minute, "warn", "provider slow", map[string]string{"provider": "openrouter", "ms": "8700"}},
		{3 * time.Minute, "warn", "tool limit reached", map[string]string{"tool": "images.generate", "scope": "user"}},
		{2 * time.Minute, "info", "scheduled backup", map[string]string{"name": "backup-practice-auto"}},
	}
	r := s.ring()
	for _, l := range lines {
		l.attrs["example"] = "practice"
		r.add(logEntry{At: now.Add(-l.ago).Format(time.RFC3339Nano), Level: l.level, Msg: l.msg, Attrs: l.attrs})
	}
}

// practiceUptime is the practice instance's sample uptime history: a month of running with one
// restart, one crash that the supervisor recovered, and one planned stop.
func practiceUptime(now time.Time) ([]uptimeEvent, time.Time) {
	day := 24 * time.Hour
	base := now.Truncate(time.Minute)
	at := func(d time.Duration) string { return base.Add(-d).UTC().Format(time.RFC3339) }
	last := base.Add(-(6*day - 9*time.Hour))
	return []uptimeEvent{
		{At: at(40 * day), Kind: "start", Version: Version},
		{At: at(21*day - 3*time.Hour), Kind: "restart", Version: Version, Reason: "config"},
		{At: at(21*day - 3*time.Hour - 5*time.Second), Kind: "start", Version: Version},
		{At: at(12*day - 14*time.Hour), Kind: "crash", Reason: "signal: killed"},
		{At: at(12*day - 14*time.Hour - 2*time.Second), Kind: "start", Version: Version},
		{At: at(6*day - 2*time.Hour), Kind: "stop", Version: Version},
		{At: last.UTC().Format(time.RFC3339), Kind: "start", Version: Version},
	}, last
}

// seedPractice imports and indexes the sample conversation once and attaches it to the practice room.
func (s *Service) seedPractice(ctx context.Context) error {
	var done bool
	if ok, _ := s.kvGet("practice:seeded", &done); ok && done {
		return nil
	}
	locale := s.Doc().I18n.DefaultLocale
	raw, err := practiceFS.ReadFile("practice/sample-" + locale + ".csv")
	if err != nil {
		raw, err = practiceFS.ReadFile("practice/sample-en.csv")
		if err != nil {
			return err
		}
	}
	dir := filepath.Join(s.DataRoot, "imports")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	path := filepath.Join(dir, "sample-"+locale+".csv")
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return err
	}
	run := func(id string, in map[string]any) (action.Result, error) {
		b, _ := json.Marshal(in)
		r := s.Execute(ctx, action.Request{Action: id, Input: b, RequestID: "practice", Locale: locale})
		if !r.OK {
			return r, fmt.Errorf("%s: %s", id, r.Error.Message)
		}
		return r, nil
	}
	if _, err := run("imports.start", map[string]any{"source": "kakao_csv", "path": path, "room": practiceRoom,
		"label": s.Msg(locale, "practice.sample_label", "language", lang.For(locale).Name), "timezone": s.Doc().I18n.Timezone}); err != nil {
		return err
	}
	if _, err := run("memory.reindex", map[string]any{"rooms": []string{practiceRoom}}); err != nil {
		return err
	}
	if eng, err := s.MemoryEngine(); err == nil {
		_, _ = eng.Reload()
	}
	for _, b := range s.dbBlocks() {
		if b.SourceRoom == practiceRoom {
			if _, err := run("channels.view.attach", map[string]any{"guild_id": PracticeGuild, "channel_id": PracticeChannel, "block_id": b.ID}); err != nil && !strings.Contains(err.Error(), "already") {
				return err
			}
		}
	}
	return s.kvSet("practice:seeded", true)
}

// ResetPractice deletes the practice instance; the next OpenPractice starts fresh.
func ResetPractice(dataRoot string) error {
	root := PracticeRoot(dataRoot)
	if filepath.Base(root) != "practice" {
		return fmt.Errorf("refusing to remove %s", root)
	}
	return os.RemoveAll(root)
}
