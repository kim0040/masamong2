package application

import (
	"log/slog"
	"testing"

	"masamong/internal/config"
	"masamong/internal/observability"
)

func TestApplyLogLevel(t *testing.T) {
	t.Cleanup(func() { _ = observability.SetLevel("") })
	s := &Service{Log: slog.Default()}
	doc := config.Document{}
	doc.Instance.LogLevel = "debug"
	s.applyLogLevel(doc)
	if observability.Level.Level() != slog.LevelDebug {
		t.Fatalf("config level not applied: %v", observability.Level.Level())
	}
	t.Setenv("MASAMONG_LOG_LEVEL", "error")
	s.applyLogLevel(doc)
	if observability.Level.Level() != slog.LevelError {
		t.Fatalf("environment did not override the config: %v", observability.Level.Level())
	}
	t.Setenv("MASAMONG_LOG_LEVEL", "")
	doc.Instance.LogLevel = "nonsense" // never fatal; the level stays as it was
	s.applyLogLevel(doc)
	if observability.Level.Level() != slog.LevelError {
		t.Fatalf("a bad level changed the threshold: %v", observability.Level.Level())
	}
}
