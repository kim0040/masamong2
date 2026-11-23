package application_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"sync"
	"testing"

	"masamong/internal/application"
)

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// The daemon's logger goes in with OpenLogged, before Open starts its workers (they read s.Log). The
// daemon used to swap svc.Log after Open had started them. Its records reach the daemon's handler
// and the console's live log.
func TestOpenLoggedInstallsTheLoggerBeforeWorkers(t *testing.T) {
	t.Parallel()
	first := setupSvcLog(t, nil)
	dir := first.DataRoot
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	var daemon lockedBuffer
	svc, err := application.OpenLogged(dir, nil, false, slog.New(slog.NewJSONHandler(&daemon, nil)))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = svc.Close() })
	if !svc.Ready() {
		t.Fatal("the set-up instance did not open with its store, so no worker started")
	}
	svc.Log.Warn("synthetic daemon line")
	if !strings.Contains(daemon.String(), "synthetic daemon line") {
		t.Fatalf("the daemon's handler did not get the record: %s", daemon.String())
	}
	entries, _ := json.Marshal(out(t, svc, "runtime.logs", map[string]any{})["entries"])
	if !strings.Contains(string(entries), "synthetic daemon line") {
		t.Fatalf("the console's live log did not get the record: %s", entries)
	}
}
