package application

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/config"
	"masamong/internal/tools"
)

// A key typed directly into an HTTP tool (a header or a URL query, not {credential}) does not leave
// through config.show, which says its settings are redacted and which the console's export downloads;
// a backup, which must keep it to restore the tool, says it includes a secret.
func TestDirectToolKeysAreMaskedInTheExportAndDeclaredInBackups(t *testing.T) {
	s, _ := deliveryService(t)
	const headerKey, queryKey = "synthetic-header-key-value", "synthetic-query-key-value"
	// A save refuses a typed key now (tools.Validate); an older configuration that holds one still loads at
	// start, so the tool is put in place the way that load does, not through a save.
	d := s.Doc()
	{
		d.Tools = maps.Clone(d.Tools)
		if d.Tools == nil {
			d.Tools = map[string]tools.Settings{}
		}
		d.Tools["acme.lookup"] = tools.Settings{HTTP: &tools.HTTP{
			URL:           "https://example.com/lookup?q={q}&apikey=" + queryKey + "&lang=en",
			Headers:       map[string]string{"Authorization": "Bearer " + headerKey, "Accept": "application/json", "X-Api-Key": "{credential}"},
			CredentialRef: "env:SYNTHETIC_TOOL_KEY",
		}, Params: []tools.Param{{Name: "q"}}, Description: "Looks up a synthetic record."}
	}
	stored, err := config.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Store.DB.Exec(`UPDATE config_revisions SET content=?, content_hash=? WHERE revision=?`, string(stored), config.Hash(stored), s.Revision()); err != nil {
		t.Fatal(err)
	}
	s.setDoc(d, s.Revision())
	show := s.Execute(context.Background(), action.Request{Action: "config.show", Locale: "en"})
	yaml, _ := show.Output["yaml"].(string)
	if !show.OK || show.Output["redacted"] != true || yaml == "" {
		t.Fatalf("config.show %+v", show)
	}
	if strings.Contains(yaml, headerKey) || strings.Contains(yaml, queryKey) {
		t.Fatalf("the export carries a key:\n%s", yaml)
	}
	for _, kept := range []string{"application/json", "{credential}", "q={q}", "lang=en", "env:SYNTHETIC_TOOL_KEY"} {
		if !strings.Contains(yaml, kept) {
			t.Fatalf("the export lost %q:\n%s", kept, yaml)
		}
	}
	if h := s.Doc().Tools["acme.lookup"].HTTP.Headers["Authorization"]; h != "Bearer "+headerKey {
		t.Fatal("the stored settings were changed")
	}

	b := s.Execute(context.Background(), action.Request{Action: "backup.create", Locale: "en"})
	if !b.OK {
		t.Fatalf("backup %+v", b.Error)
	}
	name, _ := b.Output["name"].(string)
	target, err := s.backupPath(name)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(target, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest map[string]any
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest["secrets_included"] != true {
		t.Fatalf("a backup holding a typed key says secrets_included=%v", manifest["secrets_included"])
	}

	// The same classifier must mask the names a legacy save could write in other spellings: the
	// validator's passwd, and a percent-encoded api_key.
	for _, queryName := range []string{"passwd", "api%5Fkey"} {
		d := s.Doc()
		d.Tools = maps.Clone(d.Tools)
		d.Tools["acme.lookup"] = tools.Settings{HTTP: &tools.HTTP{
			URL:           "https://example.com/lookup?q={q}&" + queryName + "=" + queryKey,
			CredentialRef: "env:SYNTHETIC_TOOL_KEY",
		}, Params: []tools.Param{{Name: "q"}}, Description: "Looks up a synthetic record."}
		stored, err := config.Marshal(d)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.Store.DB.Exec(`UPDATE config_revisions SET content=?, content_hash=? WHERE revision=?`, string(stored), config.Hash(stored), s.Revision()); err != nil {
			t.Fatal(err)
		}
		s.setDoc(d, s.Revision())
		show := s.Execute(context.Background(), action.Request{Action: "config.show", Locale: "en"})
		yaml, _ := show.Output["yaml"].(string)
		if !show.OK || strings.Contains(yaml, queryKey) {
			t.Fatalf("%s: the export carries a key:\n%s", queryName, yaml)
		}
		if u := s.Doc().Tools["acme.lookup"].HTTP.URL; !strings.Contains(u, queryKey) {
			t.Fatal("the stored settings were changed")
		}
		b := s.Execute(context.Background(), action.Request{Action: "backup.create", Locale: "en"})
		if !b.OK {
			t.Fatalf("%s: backup %+v", queryName, b.Error)
		}
		target, err := s.backupPath(b.Output["name"].(string))
		if err != nil {
			t.Fatal(err)
		}
		raw, err := os.ReadFile(filepath.Join(target, "manifest.json"))
		if err != nil {
			t.Fatal(err)
		}
		var manifest map[string]any
		if err := json.Unmarshal(raw, &manifest); err != nil {
			t.Fatal(err)
		}
		if manifest["secrets_included"] != true {
			t.Fatalf("%s: a backup holding a typed key says secrets_included=%v", queryName, manifest["secrets_included"])
		}
	}
}

func TestBackupWithoutTypedKeysSaysNoSecrets(t *testing.T) {
	s, _ := deliveryService(t)
	b := s.Execute(context.Background(), action.Request{Action: "backup.create", Locale: "en"})
	if !b.OK {
		t.Fatalf("backup %+v", b.Error)
	}
	name, _ := b.Output["name"].(string)
	target, _ := s.backupPath(name)
	raw, err := os.ReadFile(filepath.Join(target, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"secrets_included": false`) {
		t.Fatalf("manifest %s", raw)
	}
}
