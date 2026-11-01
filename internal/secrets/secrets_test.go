package secrets

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

// fakeKeychain installs the in-memory keychain and clears the cache afterwards.
func fakeKeychain(t *testing.T) map[string]string {
	t.Helper()
	store := map[string]string{}
	t.Cleanup(UseFakeKeychain(store))
	return store
}

func TestValidRef(t *testing.T) {
	for ref, want := range map[string]bool{
		"env:MASAMONG_KEY": true, "file:/x/y.key": true, "keychain:masamo/openrouter": true,
		"keychain:": false, "keychain:../x": false, "env:": false, "sk-live-123": false, "": false,
		"vault:x": false, "keychain:a b": false,
	} {
		if got := ValidRef(ref); got != want {
			t.Errorf("ValidRef(%q) = %v", ref, got)
		}
	}
}

func TestLookupSchemes(t *testing.T) {
	t.Setenv("SECRETS_TEST_VAR", "  from-env \n")
	if v, err := Lookup("env:SECRETS_TEST_VAR"); err != nil || v != "from-env" {
		t.Fatalf("env: %q %v", v, err)
	}
	if _, err := Lookup("env:SECRETS_TEST_UNSET_VAR"); !errors.Is(err, ErrMissing) {
		t.Fatalf("unset env must be ErrMissing: %v", err)
	}
	dir := t.TempDir()
	p := filepath.Join(dir, "k.key")
	if err := os.WriteFile(p, []byte("file-secret\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if v, err := Lookup("file:" + p); err != nil || v != "file-secret" {
		t.Fatalf("file: %q %v", v, err)
	}
	if _, err := Lookup("file:" + filepath.Join(dir, "none")); !errors.Is(err, ErrMissing) {
		t.Fatalf("missing file must be ErrMissing: %v", err)
	}
	if _, err := Lookup("vault:x"); err == nil || errors.Is(err, ErrMissing) {
		t.Fatalf("unknown scheme must be a real error: %v", err)
	}
}

func TestKeychainRoundTripEscapesValue(t *testing.T) {
	store := fakeKeychain(t)
	tricky := `a"b\c $HOME 'q' end`
	if err := KeychainWrite("inst/openrouter", tricky); err != nil {
		t.Fatal(err)
	}
	if store["inst/openrouter"] != tricky {
		t.Fatalf("stored %q", store["inst/openrouter"])
	}
	if v, err := Lookup("keychain:inst/openrouter"); err != nil || v != tricky {
		t.Fatalf("lookup %q %v", v, err)
	}
	if err := KeychainDelete("inst/openrouter"); err != nil {
		t.Fatal(err)
	}
	if _, err := Lookup("keychain:inst/openrouter"); !errors.Is(err, ErrMissing) {
		t.Fatalf("deleted item must be ErrMissing: %v", err)
	}
	if err := KeychainDelete("inst/openrouter"); err != nil {
		t.Fatalf("deleting a missing item is not an error: %v", err)
	}
}

func TestKeychainRejectsBadInput(t *testing.T) {
	fakeKeychain(t)
	if err := KeychainWrite("ok", "two\nlines"); err == nil {
		t.Fatal("multi-line value must be refused")
	}
	if err := KeychainWrite("../escape", "v"); err == nil {
		t.Fatal("bad account must be refused")
	}
	if err := KeychainWrite("ok", ""); err == nil {
		t.Fatal("empty value must be refused")
	}
}

func TestKeychainLockedIsAnError(t *testing.T) {
	fakeKeychain(t)
	if runtime.GOOS == "windows" {
		windowsCredentialRead = func(string) (string, error) { return "", errors.New("credential manager access denied") }
	} else {
		keychainExec = func(string, ...string) ([]byte, int, error) { return nil, 51, nil }
	}
	if _, err := Lookup("keychain:inst/x"); err == nil || errors.Is(err, ErrMissing) {
		t.Fatalf("a locked keychain must not look like a missing key: %v", err)
	}
}

func TestKeychainUnsupported(t *testing.T) {
	fakeKeychain(t)
	keychainSupported = func() bool { return false }
	if _, err := Lookup("keychain:inst/x"); !errors.Is(err, ErrKeychainUnavailable) {
		t.Fatalf("got %v", err)
	}
	if err := KeychainWrite("inst/x", "v"); !errors.Is(err, ErrKeychainUnavailable) {
		t.Fatalf("got %v", err)
	}
}

// TestLiveKeychain touches the real login keychain; run with MASAMONG_KEYCHAIN_LIVE=1 on macOS.
func TestLiveKeychain(t *testing.T) {
	if os.Getenv("MASAMONG_KEYCHAIN_LIVE") != "1" || !KeychainSupported() {
		t.Skip("set MASAMONG_KEYCHAIN_LIVE=1 on macOS")
	}
	acct := "selftest/probe"
	for _, v := range []string{`plain-key_123`, `we"ird\va$lue 'x' \\ end`} {
		if err := KeychainWrite(acct, v); err != nil {
			t.Fatal(err)
		}
		invalidate(acct)
		if got, err := KeychainRead(acct); err != nil || got != v {
			t.Fatalf("got %q err %v", got, err)
		}
	}
	if err := KeychainDelete(acct); err != nil {
		t.Fatal(err)
	}
}
