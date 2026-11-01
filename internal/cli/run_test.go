package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"masamong/internal/action"
	"masamong/internal/fsutil"
)

func TestDecideOutputJSONOnPipeNoColorCI(t *testing.T) {
	if got := DecideOutput(Options{}, false); got != "json" {
		t.Fatalf("pipe default %s", got)
	}
	if got := DecideOutput(Options{NoColor: true}, true); got != "json" {
		t.Fatalf("NO_COLOR/CI on TTY %s", got)
	}
	if got := DecideOutput(Options{Output: "json"}, true); got != "json" {
		t.Fatalf("--output json %s", got)
	}
	if got := DecideOutput(Options{}, true); got != "rich" {
		t.Fatalf("TTY+color %s", got)
	}
	_, act, _, _ := ParseArgs([]string{"jobs", "watch"})
	if act != "jobs.watch" {
		t.Fatalf("jobs watch → %s", act)
	}
}

func TestPipeRunEmitsJSONActionSchema(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	var errb bytes.Buffer
	var setup bytes.Buffer
	if code := Run(ctx, []string{"--data-root", dir, "setup", "--instance", "gate"}, &setup, &errb, false); code != 0 {
		t.Fatalf("setup %d %s %s", code, setup.String(), errb.String())
	}
	if strings.Contains(setup.String(), "\x1b[") {
		t.Fatal("pipe JSON must not contain ANSI")
	}
	var res action.Result
	if err := json.Unmarshal(setup.Bytes(), &res); err != nil {
		t.Fatalf("pipe default must be Action JSON: %v %s", err, setup.String())
	}
	if !res.OK || res.Action != "setup.resume" || res.Output["instance"] != "gate" {
		t.Fatalf("%+v", res)
	}
	var pipe, flag bytes.Buffer
	if code := Run(ctx, []string{"--data-root", dir, "doctor"}, &pipe, &errb, false); code != 0 {
		t.Fatal(pipe.String())
	}
	if code := Run(ctx, []string{"--data-root", dir, "--output", "json", "doctor"}, &flag, &errb, false); code != 0 {
		t.Fatal(flag.String())
	}
	if strings.Contains(pipe.String(), "\x1b[") {
		t.Fatal("ANSI in pipe doctor")
	}
	if !JSONEqualSchema(pipe.Bytes(), flag.Bytes()) {
		t.Fatalf("pipe doctor JSON must match --output json\npipe=%s\nflag=%s", pipe.String(), flag.String())
	}
}

func TestRunNOCOLORAndCIForceJSONOnTTY(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	var errb bytes.Buffer
	if code := Run(ctx, []string{"--data-root", dir, "setup", "--instance", "gate"}, ioDiscard(), &errb, false); code != 0 {
		t.Fatal(errb.String())
	}
	assertJSONDoctor := func(t *testing.T, envKey, envVal string) {
		t.Helper()
		t.Setenv("NO_COLOR", "")
		t.Setenv("CI", "")
		t.Setenv(envKey, envVal)
		var out bytes.Buffer
		// stdoutTTY=true would be rich unless NO_COLOR/CI folded into ParseArgs.NoColor.
		code := run(ctx, []string{"--data-root", dir, "doctor"}, &out, &errb, false, true)
		if code != 0 {
			t.Fatalf("%s=%s doctor %d %s", envKey, envVal, code, out.String())
		}
		if strings.Contains(out.String(), "\x1b[") {
			t.Fatalf("%s=%s leaked ANSI: %s", envKey, envVal, out.String())
		}
		var res action.Result
		if err := json.Unmarshal(out.Bytes(), &res); err != nil {
			t.Fatalf("%s=%s must be Action JSON: %v %s", envKey, envVal, err, out.String())
		}
		if !res.OK || res.Action != "runtime.doctor" || res.Output["instance"] != "gate" {
			t.Fatalf("%s=%s %+v", envKey, envVal, res)
		}
		var flagged bytes.Buffer
		t.Setenv("NO_COLOR", "")
		t.Setenv("CI", "")
		if code := run(ctx, []string{"--data-root", dir, "--output", "json", "doctor"}, &flagged, &errb, false, true); code != 0 {
			t.Fatal(flagged.String())
		}
		if !JSONEqualSchema(out.Bytes(), flagged.Bytes()) {
			t.Fatalf("%s=%s JSON must match --output json\nenv=%s\nflag=%s", envKey, envVal, out.String(), flagged.String())
		}
	}
	assertJSONDoctor(t, "NO_COLOR", "1")
	assertJSONDoctor(t, "CI", "true")
}

func TestRunTTYColorIsRichNotJSON(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("CI", "")
	dir := t.TempDir()
	ctx := context.Background()
	var errb bytes.Buffer
	if code := Run(ctx, []string{"--data-root", dir, "setup", "--instance", "gate"}, ioDiscard(), &errb, false); code != 0 {
		t.Fatal(errb.String())
	}
	var out bytes.Buffer
	code := run(ctx, []string{"--data-root", dir, "doctor"}, &out, &errb, false, true)
	if code != 0 {
		t.Fatal(out.String())
	}
	if !strings.Contains(out.String(), "\x1b[") {
		t.Fatalf("TTY+color must be rich ANSI, got %q", out.String())
	}
	if json.Valid(bytes.TrimSpace(out.Bytes())) && bytes.Contains(out.Bytes(), []byte(`"action"`)) && !bytes.Contains(out.Bytes(), []byte("\x1b")) {
		t.Fatal("TTY+color must not fall back to JSON")
	}
}

func TestRichWriteUsesANSI(t *testing.T) {
	var buf bytes.Buffer
	res := action.Result{OK: true, Action: "runtime.doctor", Output: map[string]any{"instance": "gate", "profile": "lite"}}
	_ = WriteResult(&buf, Options{Output: "rich"}, res)
	if !strings.Contains(buf.String(), "\x1b[") {
		t.Fatalf("rich must use color: %q", buf.String())
	}
	buf.Reset()
	_ = WriteResult(&buf, Options{Output: "json"}, res)
	if strings.Contains(buf.String(), "\x1b[") {
		t.Fatal("json must not contain ANSI")
	}
}

func TestCLISetupDoctorConfigTwice(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	var out1, err1 bytes.Buffer
	code := Run(ctx, []string{"--data-root", dir, "--output", "json", "setup", "--instance", "gate"}, &out1, &err1, false)
	if code != 0 {
		t.Fatalf("setup1 %d %s %s", code, out1.String(), err1.String())
	}
	var res1 map[string]any
	if err := json.Unmarshal(out1.Bytes(), &res1); err != nil {
		t.Fatal(err)
	}
	if res1["ok"] != true {
		t.Fatalf("%s", out1.String())
	}
	out := res1["output"].(map[string]any)
	if out["instance"] != "gate" || out["profile"] != "lite" {
		t.Fatalf("%+v", out)
	}
	if strings.Contains(out1.String(), "\x1b[") {
		t.Fatal("ansi in json")
	}

	var out2, err2 bytes.Buffer
	code = Run(ctx, []string{"--data-root", dir, "--output", "json", "setup", "--instance", "gate"}, &out2, &err2, false)
	if code != 0 {
		t.Fatalf("setup2 %d %s", code, out2.String())
	}

	var dout bytes.Buffer
	code = Run(ctx, []string{"--data-root", dir, "--output", "json", "doctor"}, &dout, ioDiscard(), false)
	if code != 0 {
		t.Fatalf("doctor %s", dout.String())
	}
	if !strings.Contains(dout.String(), `"single_ready":false`) && !strings.Contains(dout.String(), `"single_ready": false`) {
		t.Fatalf("doctor must not use a single ready bit: %s", dout.String())
	}
	// a fresh instance has no model key: the doctor says so instead of calling itself ready
	if !strings.Contains(dout.String(), `"unconfigured"`) {
		t.Fatalf("expected features that need setup: %s", dout.String())
	}

	cand := filepath.Join(dir, "new.yaml")
	body, err := os.ReadFile(filepath.Join(dir, "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	replaced := strings.Replace(string(body), "default_locale: en", "default_locale: ko", 1)
	if err := os.WriteFile(cand, []byte(replaced), 0o600); err != nil {
		t.Fatal(err)
	}
	var stale bytes.Buffer
	code = Run(ctx, []string{"--data-root", dir, "--output", "json", "config", "apply", "--file", cand, "--base_revision", "0"}, &stale, ioDiscard(), false)
	if code == 0 || !strings.Contains(stale.String(), "CONFIG_CONFLICT") {
		t.Fatalf("stale apply %d %s", code, stale.String())
	}
	var okb bytes.Buffer
	code = Run(ctx, []string{"--data-root", dir, "--output", "json", "config", "apply", "--file", cand, "--base_revision", "1"}, &okb, ioDiscard(), false)
	if code != 0 {
		t.Fatalf("apply %s", okb.String())
	}
}

type discard struct{}

func ioDiscard() *discard { return &discard{} }

func (*discard) Write(p []byte) (int, error) { return len(p), nil }

func TestActionMappingCoversNewVerbs(t *testing.T) {
	cases := map[string][]string{
		"finance.quote":   {"finance", "quote"},
		"finance.fx":      {"finance", "fx"},
		"disasters.query": {"disasters"},
		"admin.token":     {"admin", "token"},
		"admin.open":      {"admin", "open"},
		"usage.budgets":   {"usage"},
		"usage.reset":     {"usage", "reset"},
		"providers.test":  {"providers", "test"},
		"people.servers":  {"people", "servers"},
	}
	for want, args := range cases {
		if _, got, _, _ := ParseArgs(args); got != want {
			t.Errorf("%v → %s, want %s", args, got, want)
		}
	}
	opt, _, _, _ := ParseArgs([]string{"doctor", "-i"})
	if !opt.Interactive {
		t.Fatal("-i must set Interactive")
	}
}

func TestCLIJSONInputFileAndStandardInput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "input.json")
	if err := os.WriteFile(path, []byte(`{"query":"audit value"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	parse := func(args []string) string {
		t.Helper()
		_, act, raw, err := ParseArgs(args)
		if err != nil {
			t.Fatal(err)
		}
		if act != "tools.test" {
			t.Fatalf("action = %s", act)
		}
		var input map[string]any
		if err := json.Unmarshal(raw, &input); err != nil {
			t.Fatal(err)
		}
		value, ok := input["input"].(string)
		if !ok {
			t.Fatalf("input = %#v", input["input"])
		}
		return value
	}
	if got := parse([]string{"tools", "test", "--id", "memory.search", "--input-file", path}); got != `{"query":"audit value"}` {
		t.Fatalf("file input = %s", got)
	}
	stdin, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	oldStdin := os.Stdin
	os.Stdin = stdin
	t.Cleanup(func() {
		os.Stdin = oldStdin
		_ = stdin.Close()
	})
	if got := parse([]string{"tools", "test", "--id", "memory.search", "--input-file", "-"}); got != `{"query":"audit value"}` {
		t.Fatalf("stdin input = %s", got)
	}
}

func TestInvalidInlineToolJSONExplainsPowerShellQuoting(t *testing.T) {
	dir := t.TempDir()
	var out, errb bytes.Buffer
	if code := Run(context.Background(), []string{"--data-root", dir, "setup", "--instance", "gate"}, ioDiscard(), &errb, false); code != 0 {
		t.Fatalf("setup failed: %s", errb.String())
	}
	code := Run(context.Background(), []string{"--data-root", dir, "--output", "json", "tools", "test",
		"--id", "memory.search", "--input", "{query: value}"}, &out, &errb, false)
	if code == 0 || !strings.Contains(out.String(), "PowerShell") || !strings.Contains(out.String(), "--input-file") {
		t.Fatalf("code %d, output %s, stderr %s", code, out.String(), errb.String())
	}
}

func TestAdminTokenIsLocalAndNotInDoctor(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	var errb bytes.Buffer
	if code := Run(ctx, []string{"--data-root", dir, "setup", "--instance", "gate"}, ioDiscard(), &errb, false); code != 0 {
		t.Fatal(errb.String())
	}
	var out bytes.Buffer
	if code := Run(ctx, []string{"--data-root", dir, "--output", "json", "admin", "token"}, &out, &errb, false); code != 0 {
		t.Fatalf("admin token %s %s", out.String(), errb.String())
	}
	var res action.Result
	if err := json.Unmarshal(out.Bytes(), &res); err != nil {
		t.Fatal(err)
	}
	tok, _ := res.Output["token"].(string)
	if len(tok) < 32 {
		t.Fatalf("token too short: %q", tok)
	}
	if err := fsutil.VerifyPrivateFile(filepath.Join(dir, "admin", "web-token")); err != nil {
		t.Fatalf("token file permissions: %v", err)
	}
	var open bytes.Buffer
	if code := Run(ctx, []string{"--data-root", dir, "--output", "json", "admin", "open"}, &open, &errb, false); code != 0 {
		t.Fatal(open.String())
	}
	if !strings.Contains(open.String(), "/login?token="+tok) {
		t.Fatalf("open url must carry the token: %s", open.String())
	}
	var doc bytes.Buffer
	_ = Run(ctx, []string{"--data-root", dir, "--output", "json", "doctor"}, &doc, &errb, false)
	if strings.Contains(doc.String(), tok) {
		t.Fatal("token must never appear in doctor output")
	}
}

func TestRichDoctorRendersFeatureTable(t *testing.T) {
	t.Setenv("NO_COLOR", "")
	t.Setenv("CI", "")
	dir := t.TempDir()
	ctx := context.Background()
	var errb bytes.Buffer
	if code := Run(ctx, []string{"--data-root", dir, "setup", "--instance", "gate"}, ioDiscard(), &errb, false); code != 0 {
		t.Fatal(errb.String())
	}
	var out bytes.Buffer
	if code := run(ctx, []string{"--data-root", dir, "doctor"}, &out, &errb, false, true); code != 0 {
		t.Fatal(out.String())
	}
	s := out.String()
	for _, want := range []string{"✓ ok", "runtime.doctor", "features", "polls", "unconfigured", "memory"} {
		if !strings.Contains(s, want) {
			t.Fatalf("rich doctor missing %q:\n%s", want, s)
		}
	}
	var plain bytes.Buffer
	_ = WriteResult(&plain, Options{Output: "table"}, action.Result{OK: true, Action: "runtime.doctor", Output: map[string]any{"features": map[string]any{"polls": map[string]any{"status": "ready"}}}})
	if strings.Contains(plain.String(), "\x1b[") || !strings.Contains(plain.String(), "polls") {
		t.Fatalf("plain table must not use ANSI: %q", plain.String())
	}
}

// Windows PowerShell 5.1 writes UTF-8 with a byte-order mark (Set-Content -Encoding UTF8) or UTF-16
// (Out-File, >): --input-file must read both.
func TestInputFileAcceptsWhatPowerShellWrites(t *testing.T) {
	const body = `{"query":"안녕 hello"}`
	utf16le := []byte{0xFF, 0xFE}
	utf16be := []byte{0xFE, 0xFF}
	for _, r := range body {
		utf16le = append(utf16le, byte(r), byte(r>>8))
		utf16be = append(utf16be, byte(r>>8), byte(r))
	}
	for name, raw := range map[string][]byte{
		"plain":    []byte(body),
		"utf8 bom": append([]byte{0xEF, 0xBB, 0xBF}, body...),
		"utf16 le": utf16le,
		"utf16 be": utf16be,
		"crlf":     []byte("\xEF\xBB\xBF" + body + "\r\n"),
	} {
		path := filepath.Join(t.TempDir(), "in.json")
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		got, err := readJSONInput(path, nil)
		if err != nil || got != body {
			t.Errorf("%s: %q, %v", name, got, err)
		}
	}
	path := filepath.Join(t.TempDir(), "bad.json")
	_ = os.WriteFile(path, []byte("{query: 1}"), 0o600)
	if _, err := readJSONInput(path, nil); err == nil || !strings.Contains(err.Error(), "invalid character") {
		t.Errorf("the real parse error must be shown: %v", err)
	}
	_ = os.WriteFile(path, []byte("[1]"), 0o600)
	if _, err := readJSONInput(path, nil); err == nil || !strings.Contains(err.Error(), "JSON object") {
		t.Errorf("an array is not an object: %v", err)
	}
}

func TestInputFileIsOnlyForToolsTest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "in.json")
	_ = os.WriteFile(path, []byte(`{"a":1}`), 0o600)
	if _, _, _, err := ParseArgs([]string{"config", "get", "--input-file", path}); err == nil || !strings.Contains(err.Error(), "tools test") {
		t.Fatalf("a command that ignores the file must say so: %v", err)
	}
	if _, act, _, err := ParseArgs([]string{"tools", "test", "--id", "x", "--input-file", path}); err != nil || act != "tools.test" {
		t.Fatalf("tools test reads it: %s %v", act, err)
	}
}

// A tray that cannot be closed (no PowerShell, an unidentifiable process) must not keep the service
// running: stop still stops the service and says what it could not do.
func TestStopStillStopsTheServiceWhenTheTrayCannotBeClosed(t *testing.T) {
	old := stopTray
	stopTray = func(string) error { return errors.New("find or stop tray process: access denied") }
	t.Cleanup(func() { stopTray = old })
	dir := t.TempDir()
	var out, errb bytes.Buffer
	code := Run(context.Background(), []string{"--data-root", dir, "--output", "json", "stop"}, &out, &errb, false)
	if code != 0 || !strings.Contains(out.String(), `"ok":true`) {
		t.Fatalf("code %d, out %s, err %s", code, out.String(), errb.String())
	}
	if !strings.Contains(errb.String(), "stopping the service anyway") {
		t.Fatalf("the tray failure must be reported: %s", errb.String())
	}
}
