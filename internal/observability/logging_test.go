package observability

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func jsonLogger(buf *bytes.Buffer, wrap func(slog.Handler) slog.Handler) *slog.Logger {
	return slog.New(wrap(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
}

func lastLine(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	var m map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &m); err != nil {
		t.Fatalf("%v: %q", err, buf.String())
	}
	return m
}

func TestContextHandlerAddsTurnAndJob(t *testing.T) {
	var buf bytes.Buffer
	l := jsonLogger(&buf, ContextHandler)
	ctx := WithJob(WithTurn(context.Background(), "t-1"), "j-9")
	l.InfoContext(ctx, "hello")
	m := lastLine(t, &buf)
	if m["turn_id"] != "t-1" || m["job_id"] != "j-9" {
		t.Fatalf("%v", m)
	}
	buf.Reset()
	l.InfoContext(context.Background(), "plain")
	l.Info("no ctx")
	m = lastLine(t, &buf)
	if _, ok := m["turn_id"]; ok {
		t.Fatalf("turn_id without a turn: %v", m)
	}
	if _, ok := m["job_id"]; ok {
		t.Fatalf("job_id without a job: %v", m)
	}
	if TurnID(context.Background()) != "" || JobID(context.Background()) != "" {
		t.Fatal("empty context has ids")
	}
}

func TestContextHandlerKeepsAttrsAndGroups(t *testing.T) {
	var buf bytes.Buffer
	l := jsonLogger(&buf, ContextHandler).With("a", 1).WithGroup("g").With("b", 2)
	l.InfoContext(WithTurn(context.Background(), "t-2"), "x", "c", 3)
	m := lastLine(t, &buf)
	if m["a"] != float64(1) {
		t.Fatalf("%v", m)
	}
	g, _ := m["g"].(map[string]any)
	if g["b"] != float64(2) || g["c"] != float64(3) || g["turn_id"] != "t-2" {
		t.Fatalf("%v", m)
	}
}

func TestRedactHandler(t *testing.T) {
	var buf bytes.Buffer
	l := jsonLogger(&buf, RedactHandler).With("pre", "token=presecret1")
	tok := "MTIzNDU2Nzg5MDEyMzQ1Njc4OTA" + ".GabcDE." + "abcdefghijklmnopqrstuvwxyz0123456789" // fake; split so secret scanners do not flag it
	l.Warn("login with "+tok,
		"s", "api_key=hunter2hunter2",
		"err", errors.New("Authorization: Bearer abcdef123456"),
		"n", 7,
		slog.Group("grp", slog.String("u", "https://x.example/a?key=zzz999&b=1"), slog.Group("in", "t", "password=pw123")))
	m := lastLine(t, &buf)
	out := buf.String()
	for _, secret := range []string{tok, "hunter2hunter2", "abcdef123456", "zzz999", "pw123", "presecret1"} {
		if strings.Contains(out, secret) {
			t.Fatalf("%q leaked: %s", secret, out)
		}
	}
	if m["level"] != "WARN" || m["n"] != float64(7) {
		t.Fatalf("record changed: %v", m)
	}
	if !strings.Contains(m["msg"].(string), "login with") {
		t.Fatalf("%v", m)
	}
}

type stringerVal struct{ s string }

func (v stringerVal) String() string { return v.s }

func TestRedactHandlerStringerAndNilPointer(t *testing.T) {
	var buf bytes.Buffer
	l := jsonLogger(&buf, RedactHandler)
	var nilStringer *stringerPtr
	l.Info("m", "v", stringerVal{"token=abcdefgh1234"}, "nilp", nilStringer)
	out := buf.String()
	if strings.Contains(out, "abcdefgh1234") {
		t.Fatal(out)
	}
	if !strings.Contains(out, `"nilp"`) {
		t.Fatalf("record dropped: %s", out)
	}
}

type stringerPtr struct{ s string }

func (p *stringerPtr) String() string { return p.s }

func TestFanoutAndMinLevel(t *testing.T) {
	var all, errs bytes.Buffer
	opts := &slog.HandlerOptions{Level: slog.LevelDebug}
	l := slog.New(Fanout(
		slog.NewJSONHandler(&all, opts),
		MinLevel(slog.NewJSONHandler(&errs, opts), slog.LevelError),
	)).With("k", "v")
	l.Info("fine")
	l.Error("boom")
	if strings.Count(all.String(), "\n") != 2 {
		t.Fatalf("all: %q", all.String())
	}
	if strings.Count(errs.String(), "\n") != 1 || !strings.Contains(errs.String(), "boom") || !strings.Contains(errs.String(), `"k":"v"`) {
		t.Fatalf("errs: %q", errs.String())
	}
}

func TestSetLevel(t *testing.T) {
	t.Cleanup(func() { _ = SetLevel("") })
	for name, want := range map[string]slog.Level{
		"": slog.LevelInfo, "info": slog.LevelInfo, "DEBUG": slog.LevelDebug, "Warn": slog.LevelWarn, "error": slog.LevelError,
	} {
		if err := SetLevel(name); err != nil || Level.Level() != want {
			t.Fatalf("%q: %v %v", name, err, Level.Level())
		}
	}
	_ = SetLevel("error")
	if err := SetLevel("loud"); err == nil || Level.Level() != slog.LevelError {
		t.Fatalf("bad name accepted or level changed: %v %v", err, Level.Level())
	}
}

func TestLevelGatesJSONHandler(t *testing.T) {
	t.Cleanup(func() { _ = SetLevel("") })
	var buf bytes.Buffer
	l := slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: Level}))
	l.Debug("hidden")
	_ = SetLevel("debug")
	l.Debug("shown")
	if strings.Contains(buf.String(), "hidden") || !strings.Contains(buf.String(), "shown") {
		t.Fatal(buf.String())
	}
}
