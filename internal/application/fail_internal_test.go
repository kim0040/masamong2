package application

import (
	"bytes"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/errcode"
)

// failKeys is every message key failErr answers with: one per kind of failure. The test below keeps
// each of them in all five languages and out of reach of Go error text.
var failKeys = []string{
	"error.storage", "error.file", "error.invalid_input", "error.permission", "error.stale",
	"import.path_denied", "memory.not_ready", "memory.failed", "memory.provider_missing",
	"backup.invalid_name", "backup.verify_failed", "backup.safety_failed", "agent.plan_rejected",
	"jobs.not_changeable", "backfill.failed", "config.invalid", "config.failed",
	"migration.not_sqlite", "migration.failed", "reset.failed", "reset.backup_failed", "setup.failed",
	"storage.dsn_invalid", "storage.target_failed", "storage.switch_failed", "stats.off", "autostart.failed",
	"update.failed", "update.download_failed", "update.prepare_failed", "update.backup_failed", "update.install_failed",
	"discord.check_unreachable", "daemon.unreachable", "notices.collect_failed", "tool.http_failed", "feature.model_failed",
}

// goText is what a library or the database would say: it must never reach a result.
const goText = `open /var/lib/masamong/x.db: no such table: turn_log (SQLITE_ERROR) dial tcp 10.0.0.7:5432: connect: connection refused`

func TestFailErrKeysExistInEveryLanguageWithoutPlaceholderLeftovers(t *testing.T) {
	for _, lang := range []string{"ko", "en", "ja", "es", "zh-Hans"} {
		raw, err := os.ReadFile(filepath.Join("..", "..", "locales", lang+".json"))
		if err != nil {
			t.Fatal(err)
		}
		var msgs map[string]string
		if err := json.Unmarshal(raw, &msgs); err != nil {
			t.Fatal(err)
		}
		for _, key := range failKeys {
			text, ok := msgs[key]
			if !ok || strings.TrimSpace(text) == "" {
				t.Errorf("%s: key %s is missing", lang, key)
			}
			if key != "tool.http_failed" && strings.Contains(text, "{") {
				t.Errorf("%s: %s has a placeholder that failErr does not fill: %q", lang, key, text)
			}
		}
	}
}

// A failure answers with the key's sentence in the person's language. The Go error is not in the message,
// not in the args, and goes to the log with the Action's name.
func TestFailErrKeepsGoTextOutOfTheResult(t *testing.T) {
	s, _ := deliveryService(t)
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	for _, lang := range []string{"ko", "en", "ja", "es", "zh-Hans"} {
		for _, key := range failKeys {
			logs.Reset()
			req := action.Request{Action: "probe.fail", Locale: lang}
			var args []any
			if key == "tool.http_failed" {
				args = []any{"tool", "ops.lookup"}
			}
			res := s.failErr(req, errcode.IO, key, errors.New(goText), map[string]any{"name": "x"}, args...)
			if res.OK || res.Error == nil || res.Error.MessageKey != key || res.Error.Code != errcode.IO {
				t.Fatalf("%s/%s: %+v", lang, key, res)
			}
			if want := s.msg(req, key, args...); res.Error.Message != want || want == "" || strings.Contains(want, "{") {
				t.Fatalf("%s/%s: message %q, want the catalog sentence %q", lang, key, res.Error.Message, want)
			}
			if lang == "en" && res.Error.Message == s.msg(req, "generic.error") {
				t.Fatalf("%s: answered with the generic sentence", key)
			}
			wire, _ := json.Marshal(res)
			for _, part := range []string{"no such table", "10.0.0.7", "connection refused", "/var/lib"} {
				if strings.Contains(string(wire), part) {
					t.Fatalf("%s/%s: Go error text %q in %s", lang, key, part, wire)
				}
			}
			if got := logs.String(); !strings.Contains(got, "no such table") || !strings.Contains(got, "probe.fail") || !strings.Contains(got, key) {
				t.Fatalf("%s/%s: the log lost the error, the action or the key:\n%s", lang, key, got)
			}
		}
	}
}

// A refusal the caller caused is a warning, a failure of the machine an error.
func TestFailErrLogsCallerFaultsAsWarnings(t *testing.T) {
	s, _ := deliveryService(t)
	var logs bytes.Buffer
	s.Log = slog.New(slog.NewTextHandler(&logs, nil))
	req := action.Request{Action: "probe.fail", Locale: "en"}
	s.failErr(req, errcode.InvalidInput, "error.invalid_input", errors.New("bad value"), nil)
	if !strings.Contains(logs.String(), "level=WARN") {
		t.Fatalf("invalid input not a warning:\n%s", logs.String())
	}
	logs.Reset()
	s.failErr(req, errcode.IO, "error.storage", errors.New("disk"), nil)
	if !strings.Contains(logs.String(), "level=ERROR") {
		t.Fatalf("an I/O failure is not an error:\n%s", logs.String())
	}
}

// Real flows: a table the database cannot read answers with the storage sentence, not with the driver's
// words, and the log has the driver's words.
func TestDatabaseFailureAnswersInPlainWords(t *testing.T) {
	for _, tc := range []struct {
		name, drop, action string
		in                 map[string]any
	}{
		{"preferences list", "user_prefs", "preferences.set", map[string]any{}},
		{"conversation log", "conversation_log", "conversation.log", map[string]any{"guild_id": "g", "channel_id": "c"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s, _ := deliveryService(t)
			var logs bytes.Buffer
			s.Log = slog.New(slog.NewTextHandler(&logs, nil))
			db, err := s.adminDB()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.DB.Exec(`DROP TABLE ` + tc.drop); err != nil {
				t.Fatal(err)
			}
			raw, _ := json.Marshal(tc.in)
			res := s.Execute(t.Context(), action.Request{Action: tc.action, Input: raw, Locale: "en"})
			if res.OK || res.Error == nil || res.Error.MessageKey != "error.storage" {
				t.Fatalf("%+v", res)
			}
			if res.Error.Message != s.msg(action.Request{Locale: "en"}, "error.storage") {
				t.Fatalf("message %q", res.Error.Message)
			}
			if strings.Contains(res.Error.Message, tc.drop) || strings.Contains(strings.ToLower(res.Error.Message), "no such table") {
				t.Fatalf("driver text in %q", res.Error.Message)
			}
			if !strings.Contains(logs.String(), "no such table") {
				t.Fatalf("the log lost the cause:\n%s", logs.String())
			}
		})
	}
}
