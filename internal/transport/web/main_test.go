//go:build !noweb

package web

import (
	"os"
	"testing"
)

// Tests store keys in 0600 files (never the developer's login keychain) and never reach the network.
func TestMain(m *testing.M) {
	os.Setenv("MASAMONG_SECRET_STORE", "file")
	os.Setenv("MASAMONG_OFFLINE", "1")
	os.Exit(m.Run())
}
