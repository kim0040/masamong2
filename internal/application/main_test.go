package application_test

import (
	"os"
	"strings"
	"testing"
)

// Tests never reach the network: keyless providers (worldwide weather, the open exchange-rate
// endpoint) fail fast instead, and keys are stored in files, not the developer's keychain. Key
// variables in the developer's shell (MASAMONG_OPENROUTER_KEY …) are cleared, since the default
// config reads them and a test would then see a provider with a key it did not set up.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		if strings.HasPrefix(name, "MASAMONG_") && strings.HasSuffix(name, "_KEY") {
			os.Unsetenv(name)
		}
	}
	os.Setenv("MASAMONG_OFFLINE", "1")
	os.Setenv("MASAMONG_SECRET_STORE", "file")
	os.Exit(m.Run())
}
