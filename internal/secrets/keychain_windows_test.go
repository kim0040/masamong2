//go:build windows

package secrets

import (
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func TestWindowsCredentialManagerRoundTrip(t *testing.T) {
	account := fmt.Sprintf("selftest-%d-%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() { _ = KeychainDelete(account) })
	value := "test credential, not a real API key"
	if err := KeychainWrite(account, value); err != nil {
		t.Fatal(err)
	}
	got, err := KeychainRead(account)
	if err != nil || got != value {
		t.Fatalf("read back %q, %v", got, err)
	}
	if err := KeychainDelete(account); err != nil {
		t.Fatal(err)
	}
	got, err = KeychainRead(account)
	if err != nil || got != "" {
		t.Fatalf("deleted credential read %q, %v", got, err)
	}
}

func TestWindowsCredentialManagerEnforcesBlobLimit(t *testing.T) {
	err := credentialManagerWrite("selftest-too-large", strings.Repeat("x", credMaximumBlobSize+1))
	if err == nil {
		t.Fatal("oversized Credential Manager blob was accepted")
	}
	if !errors.Is(err, ErrKeychainValueTooLong) {
		t.Fatalf("callers need the typed error to fall back and warn: %v", err)
	}
}
