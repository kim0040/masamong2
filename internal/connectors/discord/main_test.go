package discord

import (
	"os"
	"testing"
)

// Tests never reach the network (see providers.ErrOffline).
func TestMain(m *testing.M) {
	os.Setenv("MASAMONG_OFFLINE", "1")
	os.Exit(m.Run())
}
