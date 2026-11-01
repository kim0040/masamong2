package application_test

import (
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/application"
)

// The console follows the log every two seconds; reading it must not write a change line or an
// audit row each time (it once did, and the log filled with its own reads).
func TestReadingTheLogWritesNothing(t *testing.T) {
	svc := setupSvcLog(t, func(s *application.Service) *slog.Logger { return s.CaptureLogs(nil) })
	for i := 0; i < 5; i++ {
		raw, _ := json.Marshal(map[string]any{"after_id": 0})
		if r := svc.Execute(context.Background(), action.Request{Action: "runtime.logs", Input: raw, RequestID: "web"}); !r.OK {
			t.Fatalf("runtime.logs: %+v", r.Error)
		}
	}
	entries, _ := json.Marshal(out(t, svc, "runtime.logs", map[string]any{})["entries"])
	if strings.Contains(string(entries), "runtime.logs") {
		t.Fatalf("reading the log logged itself: %s", entries)
	}
	audit := out(t, svc, "audit.list", map[string]any{})
	if raw, _ := json.Marshal(audit); strings.Contains(string(raw), "runtime.logs") {
		t.Fatalf("reading the log was audited: %s", raw)
	}
}

// The practice instance is opened with the real instance's logger. Its lines belong to its own
// console log; the real one must not show them, and only practice has the example lines.
func TestPracticeLogIsSeparateAndHasExamples(t *testing.T) {
	svc := setupSvcLog(t, func(s *application.Service) *slog.Logger { return s.CaptureLogs(nil) })
	svc.Log.Info("real line")
	p, err := application.OpenPractice(context.Background(), t.TempDir(), "ko", svc.Log)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	p.Log.Info("practice line")
	real, _ := json.Marshal(out(t, svc, "runtime.logs", map[string]any{})["entries"])
	if strings.Contains(string(real), "practice line") || strings.Contains(string(real), `"example"`) {
		t.Fatalf("practice lines reached the real log: %s", real)
	}
	prac, _ := json.Marshal(out(t, p, "runtime.logs", map[string]any{})["entries"])
	if !strings.Contains(string(prac), "practice line") || !strings.Contains(string(prac), `"example":"practice"`) || strings.Contains(string(prac), "real line") {
		t.Fatalf("practice log: %s", prac)
	}
	warn := out(t, p, "runtime.logs", map[string]any{"level": "error"})["entries"].([]any)
	if len(warn) == 0 {
		t.Fatal("practice examples include an error line to filter for")
	}
}

// Real history never contains samples; practice shows a sample month that is internally consistent.
func TestUptimeHistorySamplesOnlyInPractice(t *testing.T) {
	svc := setupSvc(t)
	h := out(t, svc, "runtime.history", map[string]any{"days": 30})
	if h["sample"] != false {
		t.Fatalf("real history marked sample: %v", h)
	}
	p, err := application.OpenPractice(context.Background(), t.TempDir(), "ko", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	ph := out(t, p, "runtime.history", map[string]any{"days": 30})
	pct := ph["uptime_pct"].(float64)
	if ph["sample"] != true || ph["crashes"].(float64) != 1 || pct < 95 || pct >= 100 || len(ph["spans"].([]any)) < 3 {
		t.Fatalf("practice history: %v", ph)
	}
}
