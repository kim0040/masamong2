package crash

import (
	"bytes"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"masamong/internal/testutil"
)

func TestRecoverKeepsTheProcessAndLogsTheStack(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	log := slog.New(slog.NewTextHandler(&lockedWriter{w: &buf, mu: &mu}, nil))
	Go(log, "test goroutine", func() {
		var m map[string]int
		m["boom"] = 1 // nil map write panics
	})
	testutil.WaitFor(t, 5*time.Second, "the goroutine crash in the log", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return strings.Contains(buf.String(), "test goroutine")
	})
	err := Run(log, "test run", func() { panic("bad input") })
	if err == nil || !strings.Contains(err.Error(), "bad input") {
		t.Fatalf("Run: %v", err)
	}
	mu.Lock()
	out := buf.String()
	mu.Unlock()
	for _, want := range []string{"test goroutine", "test run", "panic recovered", "crash_test.go"} {
		if !strings.Contains(out, want) {
			t.Errorf("log lacks %q:\n%s", want, out)
		}
	}
	if Run(log, "ok", func() {}) != nil {
		t.Fatal("no panic, no error")
	}
}

type lockedWriter struct {
	w  *bytes.Buffer
	mu *sync.Mutex
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}
