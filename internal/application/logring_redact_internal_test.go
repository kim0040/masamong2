package application

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"

	"masamong/internal/observability"
)

// The console's live log masks what the file log masks: the daemon's logger is
// ContextHandler(RedactHandler(JSON)) wrapped by CaptureLogs, and the ring sits before the redaction.
func TestLogRingMasksWhatTheFileLogMasks(t *testing.T) {
	bearer := "abcdefghij" + "klmnop1234567890"        // fake; split so secret scanners do not flag it
	urlKey := "AIza" + "SECRETSECRETSECRET"            // fake
	skKey := "sk-" + "abcdefghij" + "klmnopqrstuvwxyz" // fake
	var file bytes.Buffer
	base := slog.New(observability.ContextHandler(observability.RedactHandler(slog.NewJSONHandler(&file, nil))))
	s := &Service{}
	l := s.CaptureLogs(base)
	l.Warn("provider failed: Authorization: Bearer "+bearer, "err", errors.New("GET https://x/y?key="+urlKey+" failed, "+skKey), "model", "plain-model")
	l.With("upstream", "retry with "+skKey).Info("second line")

	r := s.ring()
	r.mu.Lock()
	entries := append([]logEntry(nil), r.entries...)
	r.mu.Unlock()
	if len(entries) != 2 {
		t.Fatalf("want two ring entries, got %d", len(entries))
	}
	shown := fmt.Sprint(entries)
	for _, secret := range []string{bearer, urlKey, skKey} {
		if strings.Contains(file.String(), secret) {
			t.Fatalf("file log leaks %q (precondition): %s", secret, file.String())
		}
		if strings.Contains(shown, secret) {
			t.Fatalf("console ring leaks %q: %s", secret, shown)
		}
	}
	// redaction keeps the words around the secret and leaves ordinary values alone
	if !strings.Contains(entries[0].Msg, "provider failed") || entries[0].Attrs["model"] != "plain-model" || !strings.Contains(entries[1].Attrs["upstream"], "retry with") {
		t.Fatalf("redaction removed ordinary text: %s", shown)
	}
}
