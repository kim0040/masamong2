// Package providers holds the outbound HTTP layer shared by every external adapter (02장 §3, 06장 §9).
// Adapters own their wire formats; this package owns credentials, retries, redaction and quotas.
package providers

import (
	"errors"
	"fmt"
	"strings"

	"masamong/internal/secrets"
)

// Credential is a resolved secret. The value is never printed, logged, or serialized.
type Credential struct {
	Ref   string
	value string
}

var ErrNoCredential = errors.New("credential not configured")

// Offline is the stand-in credential of in-process endpoints (practice mode) that need no secret.
func Offline() Credential { return Credential{Ref: "offline", value: "offline"} }

// Present reports whether a non-empty secret was resolved.
func (c Credential) Present() bool { return c.value != "" }

// Value returns the secret for use in a request. Callers must not log it.
func (c Credential) Value() string { return c.value }

// String implements fmt.Stringer so accidental %v formatting stays redacted.
func (c Credential) String() string {
	if c.Ref == "" {
		return "<none>"
	}
	return "<redacted " + c.Ref + ">"
}

// MarshalJSON keeps the secret out of any JSON output.
func (c Credential) MarshalJSON() ([]byte, error) {
	return []byte(fmt.Sprintf("%q", c.String())), nil
}

// Resolve reads a credential reference (env:NAME, file:/path or keychain:NAME, see package secrets).
// An empty ref or a reference with nothing stored yet resolves to an absent credential without error;
// unknown schemes and unreadable stores (a locked keychain) are errors. Placeholder values that
// shipped in example configs are treated as absent.
func Resolve(ref string) (Credential, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return Credential{}, nil
	}
	if !secrets.ValidRef(ref) {
		return Credential{Ref: ref}, fmt.Errorf("credential ref %q: expected %s", ref, secrets.Hint)
	}
	raw, err := secrets.Lookup(ref)
	if errors.Is(err, secrets.ErrMissing) {
		return Credential{Ref: ref}, nil
	}
	if err != nil {
		return Credential{Ref: ref}, err
	}
	if isPlaceholder(raw) {
		raw = ""
	}
	return Credential{Ref: ref, value: raw}, nil
}

func isPlaceholder(v string) bool {
	l := strings.ToLower(v)
	return l == "" || strings.HasPrefix(l, "your_") || strings.HasPrefix(l, "replace-with") || strings.Contains(l, "_key_here") || l == "changeme"
}
