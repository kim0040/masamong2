package application_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/tools"
)

// failMessage is the catalog sentence a failed Action must carry.
func failMessage(svc interface {
	Msg(locale, key string, args ...any) string
}, key string, args ...any) string {
	return svc.Msg("en", key, args...)
}

// The failure of an operator HTTP tool names the tool and the error's class, never the server's body or
// the address it was called at.
func TestHTTPToolFailureHasNoServerText(t *testing.T) {
	svc := setupSvc(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "internal trace: synthetic-backend-secret at /srv/app/main.go:42", http.StatusBadRequest)
	}))
	defer srv.Close()
	applyDoc(t, svc, func(d *config.Document) {
		d.Tools = map[string]tools.Settings{"city.data": {Description: "city data", Params: []tools.Param{{Name: "city", Required: true}},
			HTTP: &tools.HTTP{URL: srv.URL + "/x/{city}/data"}}}
	})
	r := exec(svc, "city.data", map[string]any{"city": "seoul"})
	if r.OK || r.Error == nil || r.Error.MessageKey != "tool.http_failed" {
		t.Fatalf("%+v", r)
	}
	if r.Error.Message != failMessage(svc, "tool.http_failed", "tool", "city.data") {
		t.Fatalf("message %q", r.Error.Message)
	}
	wire, _ := json.Marshal(r)
	for _, part := range []string{"synthetic-backend-secret", "main.go", srv.URL, "/x/seoul/data", "http://"} {
		if strings.Contains(string(wire), part) {
			t.Fatalf("%q in %s", part, wire)
		}
	}
	if r.Error.Args["tool"] != "city.data" || r.Error.Args["status"] == nil {
		t.Fatalf("args: %+v", r.Error.Args)
	}
}

// A settings file the validator refuses is told as the key's sentence followed by the validator's own reason
// (the setting and the rule: that is what the operator has to fix), the way prefs.invalid does.
func TestConfigRefusalNamesTheReasonAfterTheKeysSentence(t *testing.T) {
	svc := setupSvc(t)
	in, _ := json.Marshal(map[string]any{"yaml": "instance: [unterminated\n\t- {"})
	r := svc.Execute(t.Context(), action.Request{Action: "config.apply", Input: in, Locale: "en", BaseRevision: svc.Revision()})
	if r.OK || r.Error == nil || r.Error.MessageKey != "config.invalid" {
		t.Fatalf("%+v", r)
	}
	prefix := strings.TrimSuffix(failMessage(svc, "config.invalid", "reason", "X"), "X")
	if !strings.HasPrefix(r.Error.Message, prefix) || len(r.Error.Message) <= len(prefix) || strings.Contains(r.Error.Message, "{reason}") {
		t.Fatalf("message %q, want %q and a reason", r.Error.Message, prefix)
	}
}

// A backup name that is not one of ours is refused with the key, not with the path rule's words.
func TestBackupNameRefusalIsPlainWords(t *testing.T) {
	svc := setupSvc(t)
	for _, act := range []string{"backup.verify", "backup.delete", "backup.restore"} {
		r := exec(svc, act, map[string]any{"name": "../outside", "confirm": "../outside"})
		if r.OK || r.Error == nil || r.Error.MessageKey != "backup.invalid_name" {
			t.Fatalf("%s: %+v", act, r)
		}
		if r.Error.Message != failMessage(svc, "backup.invalid_name") || strings.Contains(r.Error.Message, "..") {
			t.Fatalf("%s: message %q", act, r.Error.Message)
		}
	}
}

// A file that is not a SQLite database answers with the key, not with the driver's "file is not a database".
func TestMigrationOfANonDatabaseIsPlainWords(t *testing.T) {
	svc := setupSvc(t)
	path := filepath.Join(t.TempDir(), "old.db")
	if err := os.WriteFile(path, []byte(strings.Repeat("this is not a database ", 200)), 0o600); err != nil {
		t.Fatal(err)
	}
	r := exec(svc, "migration.apply", map[string]any{"path": path})
	if r.OK || r.Error == nil || r.Error.MessageKey != "migration.not_sqlite" {
		t.Fatalf("%+v", r)
	}
	if strings.Contains(strings.ToLower(r.Error.Message), "not a database") && !strings.Contains(r.Error.Message, "SQLite database.") {
		t.Fatalf("driver text: %q", r.Error.Message)
	}
	if r.Error.Message != failMessage(svc, "migration.not_sqlite") {
		t.Fatalf("message %q", r.Error.Message)
	}
}

var _ = action.Request{}
