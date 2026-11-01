package secrets

import "errors"

// ErrKeychainValueTooLong means the value is longer than the system credential store keeps (Windows
// Credential Manager: 2,560 bytes). Callers fall back to a private file and say so.
var ErrKeychainValueTooLong = errors.New("keychain: value is longer than the system credential store accepts")

// Windows Credential Manager calls are injectable so keychain references stay testable without
// writing test data to the operator's credential store.
var (
	windowsCredentialRead   = func(string) (string, error) { return "", ErrKeychainUnavailable }
	windowsCredentialWrite  = func(string, string) error { return ErrKeychainUnavailable }
	windowsCredentialDelete = func(string) error { return ErrKeychainUnavailable }
)
