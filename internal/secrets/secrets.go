// Package secrets resolves credential references. A reference names where a secret lives and is the
// only thing config, Actions, logs and backups ever hold:
//
//	env:NAME          an environment variable
//	file:/path        a file (the console writes these 0600 under <data-root>/secrets)
//	keychain:ACCOUNT  the macOS login keychain, service "masamong" (encrypted at rest, unlocked at login)
//
// The console stores new keys in the keychain when the OS has one and falls back to a 0600 file.
package secrets

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// Schemes are the accepted reference prefixes.
var Schemes = []string{"env:", "file:", "keychain:"}

// Hint is the wording errors use for a valid reference.
const Hint = "env:NAME, file:PATH or keychain:NAME"

// ValidRef reports whether ref uses a known scheme with a non-empty name. It never looks the secret up.
func ValidRef(ref string) bool {
	ref = strings.TrimSpace(ref)
	for _, s := range Schemes {
		if rest, ok := strings.CutPrefix(ref, s); ok {
			if s == "keychain:" {
				return accountRe.MatchString(rest)
			}
			return rest != ""
		}
	}
	return false
}

// Scheme returns "env", "file", "keychain" or "" for ref.
func Scheme(ref string) string {
	scheme, _, ok := strings.Cut(strings.TrimSpace(ref), ":")
	if !ok {
		return ""
	}
	for _, s := range Schemes {
		if s == scheme+":" {
			return scheme
		}
	}
	return ""
}

// ErrMissing means the reference is well-formed but nothing is stored there yet.
var ErrMissing = errors.New("secret not set")

// Lookup reads the secret behind ref. A missing variable, file or keychain item returns ErrMissing;
// any other failure (unreadable file, locked keychain) is a real error. Values are trimmed.
func Lookup(ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	scheme, rest, ok := strings.Cut(ref, ":")
	if !ok || rest == "" {
		return "", fmt.Errorf("credential ref %q: expected %s", ref, Hint)
	}
	switch scheme {
	case "env":
		v := strings.TrimSpace(os.Getenv(rest))
		if v == "" {
			return "", ErrMissing
		}
		return v, nil
	case "file":
		b, err := os.ReadFile(rest)
		if err != nil {
			if os.IsNotExist(err) {
				return "", ErrMissing
			}
			return "", fmt.Errorf("credential ref %q: %w", ref, err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", ErrMissing
		}
		return v, nil
	case "keychain":
		v, err := KeychainRead(rest)
		if err != nil {
			return "", fmt.Errorf("credential ref %q: %w", ref, err)
		}
		if v == "" {
			return "", ErrMissing
		}
		return v, nil
	}
	return "", fmt.Errorf("credential ref %q: unsupported scheme %q (use %s)", ref, scheme, Hint)
}
