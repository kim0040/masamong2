package storage

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNormalizeBackendAliases(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    string
		wantErr error
	}{
		{"", BackendSQLite, nil},
		{"sqlite", BackendSQLite, nil},
		{" SQLITE ", BackendSQLite, nil},
		{"local", BackendSQLite, nil},
		{"tidb-mysql", "", ErrTiDBDisabled},
	} {
		got, err := NormalizeBackend(tc.in)
		if tc.wantErr != nil {
			if !errors.Is(err, tc.wantErr) {
				t.Errorf("NormalizeBackend(%q) err %v, want %v", tc.in, err, tc.wantErr)
			}
			continue
		}
		if err != nil || got != tc.want {
			t.Errorf("NormalizeBackend(%q) = %q, %v", tc.in, got, err)
		}
	}
	if _, err := NormalizeBackend("postgres"); err == nil {
		t.Error("an unknown backend must be refused")
	}
}

func TestLoadSettingsMissingFileDefaultsToSQLite(t *testing.T) {
	s, err := LoadSettings(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if s.Backend != BackendSQLite {
		t.Fatalf("backend %q", s.Backend)
	}
}

func TestLoadSettingsValidates(t *testing.T) {
	root := t.TempDir()
	write := func(body string) {
		if err := os.WriteFile(SettingsPath(root), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("backend: nosuchdb\n")
	if _, err := LoadSettings(root); err == nil {
		t.Fatal("an unknown backend in the file must be refused")
	}
	write("backend: sqlite\ndsn_ref: plaintext-password\n")
	if _, err := LoadSettings(root); err == nil {
		t.Fatal("a dsn_ref that is not a reference must be refused")
	}
	write("backend: [\n")
	if _, err := LoadSettings(root); err == nil {
		t.Fatal("malformed yaml must be refused")
	}
}

func TestSaveAndLoadSettingsPermAndFields(t *testing.T) {
	root := t.TempDir()
	want := Settings{Backend: BackendSQLite, SQLiteFile: "sub/custom.db", DSNRef: "env:MASAMONG_TIDB_DSN"}
	if err := SaveSettings(root, want); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSettings(root)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("%+v != %+v", got, want)
	}
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(SettingsPath(root))
		if err != nil {
			t.Fatal(err)
		}
		if fi.Mode().Perm() != 0o600 {
			t.Fatalf("storage.yaml mode %04o", fi.Mode().Perm())
		}
	}
}

func TestOptionsFromEnv(t *testing.T) {
	t.Setenv("MASAMONG_DB_BACKEND", "")
	t.Setenv("MASAMONG_TIDB_DSN", "")
	root := t.TempDir()

	opt, err := OptionsFromEnv(root, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if opt.Backend != BackendSQLite || opt.SQLitePath != filepath.Join(root, "masamong.db") {
		t.Fatalf("default options: %+v", opt)
	}

	if err := SaveSettings(root, Settings{Backend: BackendSQLite, SQLiteFile: "sub/custom.db"}); err != nil {
		t.Fatal(err)
	}
	opt, err = OptionsFromEnv(root, Settings{})
	if err != nil {
		t.Fatal(err)
	}
	if opt.SQLitePath != filepath.Join(root, "sub", "custom.db") {
		t.Fatalf("custom file: %+v", opt)
	}

	// An explicit backend request for TiDB is refused while the backend is off,
	// whether it comes from the environment or the flag override.
	for name, ov := range map[string]Settings{
		"env":      {},
		"override": {Backend: "tidb"},
	} {
		t.Run(name, func(t *testing.T) {
			if name == "env" {
				t.Setenv("MASAMONG_DB_BACKEND", "tidb")
			}
			if _, err := OptionsFromEnv(root, ov); !errors.Is(err, ErrTiDBDisabled) {
				t.Fatalf("%s tidb: %v", name, err)
			}
		})
	}
}

func TestResolveDSN(t *testing.T) {
	if _, err := ResolveDSN("literal-password"); err == nil {
		t.Fatal("a bare password is not a reference")
	}
	if _, err := ResolveDSN("env:MASAMONG_DSN_TEST_UNSET"); err == nil {
		t.Fatal("an unset variable must fail")
	}
	t.Setenv("MASAMONG_DSN_TEST", "mysql://user:pw@host/db")
	if got, err := ResolveDSN("env:MASAMONG_DSN_TEST"); err != nil || got != "mysql://user:pw@host/db" {
		t.Fatalf("%q %v", got, err)
	}
	t.Setenv("MASAMONG_TIDB_DSN", "mysql://u:p@h/d")
	if got, err := ResolveDSN(""); err != nil || got != "mysql://u:p@h/d" {
		t.Fatalf("the default ref: %q %v", got, err)
	}
}

func TestRedactDSNHidesUserinfo(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"", ""},
		{"mysql://user:secret@host:3306/db", "mysql://%2A%2A%2A@host:3306/db"},
		{"mysql://host/db", "mysql://host/db"},
		{"user:secret@host/db", "***@host/db"},
		{"plain-string-no-at", "(set)"},
	} {
		if got := RedactDSN(tc.in); got != tc.want {
			t.Errorf("RedactDSN(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
	if strings.Contains(RedactDSN("mysql://u:topsecret@h/d"), "topsecret") {
		t.Error("the password leaked")
	}
}
