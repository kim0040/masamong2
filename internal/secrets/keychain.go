package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// KeychainService is the keychain item service every masamong secret is filed under.
const KeychainService = "masamong"

// ErrKeychainUnavailable means this OS has no keychain masamong can use.
var ErrKeychainUnavailable = errors.New("keychain: not available on this system (use file: instead)")

var accountRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

const securityTool = "/usr/bin/security"

// keychainExec runs the security tool with stdin and returns stdout and the exit code. Tests replace it.
var keychainExec = func(stdin string, args ...string) ([]byte, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, securityTool, args...)
	cmd.Stdin = strings.NewReader(stdin)
	var out, errOut bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errOut
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil, -1, errors.New("keychain: timed out (is the keychain waiting for a password prompt?)")
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return out.Bytes(), ee.ExitCode(), nil
	}
	if err != nil {
		return nil, -1, err
	}
	return out.Bytes(), 0, nil
}

// keychainSupported is replaced by tests.
var keychainSupported = func() bool {
	if runtime.GOOS == "windows" {
		return true
	}
	if runtime.GOOS != "darwin" {
		return false
	}
	_, err := os.Stat(securityTool)
	return err == nil
}

// KeychainSupported reports whether new keys should go to the system credential store. Set
// MASAMONG_SECRET_STORE=file to keep new keys in private files; existing keychain: references remain
// readable on supported systems.
func KeychainSupported() bool {
	if os.Getenv("MASAMONG_SECRET_STORE") == "file" {
		return false
	}
	return keychainSupported()
}

// ValidAccount reports whether name can be a keychain account.
func ValidAccount(name string) bool { return accountRe.MatchString(name) }

// A short cache keeps chat turns from starting a process for every key lookup. Writes and deletes
// through this package invalidate it; a change made outside masamong shows up within the TTL.
var (
	cacheMu sync.Mutex
	cache   = map[string]cached{}
)

type cached struct {
	value string
	at    time.Time
}

const cacheTTL = 2 * time.Minute

// exit codes of security(1)
const (
	secItemNotFound = 44
)

// KeychainRead returns the stored value, or "" with no error when there is no item.
func KeychainRead(account string) (string, error) {
	if !accountRe.MatchString(account) {
		return "", fmt.Errorf("keychain: invalid account name")
	}
	if !keychainSupported() {
		return "", ErrKeychainUnavailable
	}
	if runtime.GOOS == "windows" {
		return windowsCredentialRead(account)
	}
	cacheMu.Lock()
	if c, ok := cache[account]; ok && time.Since(c.at) < cacheTTL {
		cacheMu.Unlock()
		return c.value, nil
	}
	cacheMu.Unlock()
	out, code, err := keychainExec("", "find-generic-password", "-s", KeychainService, "-a", account, "-w")
	if err != nil {
		return "", err
	}
	switch code {
	case 0:
	case secItemNotFound:
		return "", nil
	default:
		return "", fmt.Errorf("keychain: read failed (exit %d; locked keychain or access denied)", code)
	}
	v := strings.TrimSpace(string(out))
	cacheMu.Lock()
	cache[account] = cached{value: v, at: time.Now()}
	cacheMu.Unlock()
	return v, nil
}

// KeychainWrite creates or replaces the item. The value goes to security(1) over stdin, so it never
// appears in a process list.
func KeychainWrite(account, value string) error {
	if !accountRe.MatchString(account) {
		return fmt.Errorf("keychain: invalid account name")
	}
	if value == "" || strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("keychain: value must be one non-empty line")
	}
	if !keychainSupported() {
		return ErrKeychainUnavailable
	}
	if runtime.GOOS == "windows" {
		return windowsCredentialWrite(account, value)
	}
	line := fmt.Sprintf("add-generic-password -U -s %s -a %s -l %s -w %s\n",
		quote(KeychainService), quote(account), quote("masamong "+account), quote(value))
	out, code, err := keychainExec(line, "-i")
	if err != nil {
		return err
	}
	// security -i reports command errors on stdout/stderr but may still exit 0; confirm by reading back.
	if code != 0 {
		return fmt.Errorf("keychain: write failed (exit %d)", code)
	}
	_ = out
	invalidate(account)
	got, err := KeychainRead(account)
	if err != nil {
		return err
	}
	if got != strings.TrimSpace(value) {
		invalidate(account)
		return errors.New("keychain: write could not be verified (keychain locked?)")
	}
	return nil
}

// KeychainDelete removes the item; a missing item is not an error.
func KeychainDelete(account string) error {
	if !accountRe.MatchString(account) {
		return fmt.Errorf("keychain: invalid account name")
	}
	if !keychainSupported() {
		return ErrKeychainUnavailable
	}
	if runtime.GOOS == "windows" {
		return windowsCredentialDelete(account)
	}
	invalidate(account)
	_, code, err := keychainExec("", "delete-generic-password", "-s", KeychainService, "-a", account)
	if err != nil {
		return err
	}
	if code != 0 && code != secItemNotFound {
		return fmt.Errorf("keychain: delete failed (exit %d)", code)
	}
	return nil
}

func invalidate(account string) {
	cacheMu.Lock()
	delete(cache, account)
	cacheMu.Unlock()
}

// quote makes a double-quoted token for security -i: backslash and double quote are escaped.
func quote(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}
