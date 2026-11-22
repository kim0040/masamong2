// Bootstrap token helpers compile in every build so the CLI can print the console URL
// even when the daemon was built with -tags noweb.
package web

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"masamong/internal/fsutil"
)

// TokenPath is where the daemon keeps the console bootstrap token (0600, inside the data root).
func TokenPath(dataRoot string) string { return filepath.Join(dataRoot, "admin", "web-token") }

// EnsureToken returns the existing bootstrap token or creates one. The token never appears in logs;
// the CLI prints it only when the operator asks for it.
func EnsureToken(dataRoot string) (string, error) {
	path := TokenPath(dataRoot)
	if err := fsutil.PrivateDir(dataRoot); err != nil {
		return "", err
	}
	if err := fsutil.PrivateDir(filepath.Dir(path)); err != nil {
		return "", err
	}
	if raw, err := os.ReadFile(path); err == nil {
		tok := strings.TrimSpace(string(raw))
		if len(tok) >= 32 {
			return tok, nil
		}
	}
	return writeNewToken(path)
}

func writeNewToken(path string) (string, error) {
	var b [24]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	tok := hex.EncodeToString(b[:])
	if err := fsutil.WriteAtomic(path, []byte(tok+"\n"), 0o600); err != nil {
		return "", err
	}
	return tok, nil
}

// RotateToken replaces the bootstrap token with a new one and deletes pending one-time codes and the
// sessions a restart would keep (sessions.json). A running console must do this through
// Server.rotateToken, which also ends the sessions it holds; the CLI calls it directly only while no
// daemon holds the data folder.
func RotateToken(dataRoot string) (string, error) {
	path := TokenPath(dataRoot)
	if err := fsutil.PrivateDir(dataRoot); err != nil {
		return "", err
	}
	if err := fsutil.PrivateDir(filepath.Dir(path)); err != nil {
		return "", err
	}
	tok, err := writeNewToken(path)
	if err != nil {
		return "", err
	}
	// The new token is in place, so the old one already fails; a code or session file that cannot be
	// removed is reported but does not undo that.
	err = os.RemoveAll(onceDir(dataRoot))
	if rmErr := os.Remove(sessionsPath(dataRoot)); rmErr != nil && !errors.Is(rmErr, os.ErrNotExist) {
		err = errors.Join(err, rmErr)
	}
	return tok, err
}

// Sessions outlive only an intentional restart (including restore): that shutdown writes them to
// sessions.json, and the next start reads and deletes it. The file is 0600 in the data folder, next
// to the bootstrap token that already grants owner access; a stop, signal or crash loses them.
func sessionsPath(dataRoot string) string { return filepath.Join(dataRoot, "sessions.json") }

// ReadToken returns the token without creating one.
func ReadToken(dataRoot string) (string, error) {
	raw, err := os.ReadFile(TokenPath(dataRoot))
	if err != nil {
		return "", err
	}
	tok := strings.TrimSpace(string(raw))
	if len(tok) < 32 {
		return "", errors.New("web token file is malformed")
	}
	return tok, nil
}

// LoginURL builds the one-shot login link for a bind address such as "127.0.0.1:8787" or ":8787".
func LoginURL(bind, token string) string {
	host, port, err := net.SplitHostPort(bind)
	if err != nil {
		host, port = bind, "8787"
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	return fmt.Sprintf("http://%s:%s/login?token=%s", host, port, token)
}

// One-time login codes let the launcher open the browser already signed in without putting the
// long-lived token in a URL. The launcher (same user, same data root) writes a code file; the console
// consumes it once within two minutes.

func onceDir(dataRoot string) string { return filepath.Join(dataRoot, "admin", "login-once") }

// onceTTL bounds how long a one-time login code stays valid; the sweep removes codes past it.
const onceTTL = 2 * time.Minute

// CreateOnce writes a fresh one-time code and returns it.
func CreateOnce(dataRoot string) (string, error) {
	var b [18]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	code := hex.EncodeToString(b[:])
	dir := onceDir(dataRoot)
	if err := fsutil.PrivateDir(dir); err != nil {
		return "", err
	}
	path := filepath.Join(dir, code)
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		return "", err
	}
	if err := fsutil.PrivateFile(path); err != nil {
		_ = os.Remove(path)
		return "", err
	}
	return code, nil
}

// ConsumeOnce reports whether code exists and is fresh, deleting it (and stale codes) either way.
func ConsumeOnce(dataRoot, code string, now time.Time) bool {
	dir := onceDir(dataRoot)
	if entries, err := os.ReadDir(dir); err == nil {
		for _, e := range entries {
			if info, err := e.Info(); err == nil && now.Sub(info.ModTime()) > onceTTL {
				_ = os.Remove(filepath.Join(dir, e.Name()))
			}
		}
	}
	if len(code) != 36 || strings.Trim(code, "0123456789abcdef") != "" {
		return false
	}
	p := filepath.Join(dir, code)
	info, err := os.Stat(p)
	if err != nil {
		return false
	}
	// only the request whose Remove succeeds signs in, so a code cannot be used twice concurrently
	if os.Remove(p) != nil {
		return false
	}
	return now.Sub(info.ModTime()) <= onceTTL
}

// OnceURL is the launcher's sign-in link for a bind address.
func OnceURL(bind, code string) string {
	return strings.Replace(LoginURL(bind, ""), "token=", "once="+code, 1)
}
